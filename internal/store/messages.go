package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

type Address struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
}

// Message is a message's summary fields as synced from Graph. Body columns
// are filled separately, so they aren't part of this struct.
type Message struct {
	ID                int64
	GraphID           string
	FolderID          int64
	ConversationID    string
	InternetMessageID string
	Subject           string
	From              Address
	To                []Address
	Cc                []Address
	ReceivedAt        time.Time
	IsRead            bool
	HasAttachments    bool
	Importance        string
	Flagged           bool
	Categories        []string
	BodyPreview       string
}

// DeltaBatch is one page of delta results for a folder.
type DeltaBatch struct {
	Upserts []Message
	// Removed are graph IDs that left the folder (deleted or moved away).
	Removed []string
}

// ApplyDelta writes one page of delta results for folderID in a single
// transaction.
//
// Upserts are keyed on the immutable graph ID, so a message that moved
// between folders updates its existing row (including folder_id) rather than
// duplicating. Each upserted message then has its queued outbox ops
// re-applied, so server state that predates a local change doesn't undo it.
//
// A removal only deletes the row if it is still in folderID: if the
// destination folder's delta (or a pending local move) already put it
// elsewhere, the removal is stale.
func (s *Store) ApplyDelta(ctx context.Context, accountID, folderID int64, b DeltaBatch) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		pending, err := hasPendingOps(ctx, tx, accountID)
		if err != nil {
			return err
		}
		for _, m := range b.Upserts {
			if err := upsertMessage(ctx, tx, accountID, folderID, m); err != nil {
				return err
			}
			if pending {
				if err := rebase(ctx, tx, accountID, m.GraphID); err != nil {
					return err
				}
			}
		}
		for _, gid := range b.Removed {
			_, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE account_id = ? AND graph_id = ? AND folder_id = ?`,
				accountID, gid, folderID)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func upsertMessage(ctx context.Context, tx *sql.Tx, accountID, folderID int64, m Message) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO messages (
			account_id, graph_id, folder_id, conversation_id, internet_message_id, subject,
			from_name, from_addr, to_json, cc_json, received_at, is_read, has_attachments,
			importance, flagged, categories_json, body_preview)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (account_id, graph_id) DO UPDATE SET
			folder_id           = excluded.folder_id,
			conversation_id     = excluded.conversation_id,
			internet_message_id = excluded.internet_message_id,
			subject             = excluded.subject,
			from_name           = excluded.from_name,
			from_addr           = excluded.from_addr,
			to_json             = excluded.to_json,
			cc_json             = excluded.cc_json,
			received_at         = excluded.received_at,
			is_read             = excluded.is_read,
			has_attachments     = excluded.has_attachments,
			importance          = excluded.importance,
			flagged             = excluded.flagged,
			categories_json     = excluded.categories_json,
			body_preview        = excluded.body_preview`,
		accountID, m.GraphID, folderID, m.ConversationID, m.InternetMessageID, m.Subject,
		m.From.Name, m.From.Addr, jsonOrEmpty(m.To), jsonOrEmpty(m.Cc), m.ReceivedAt.Unix(), m.IsRead, m.HasAttachments,
		m.Importance, m.Flagged, jsonOrEmpty(m.Categories), m.BodyPreview)
	return err
}

// PruneFolder deletes messages in folderID received at or after since whose
// graph IDs are not in keep. It finishes a from-scratch sync pass, removing
// rows for messages that disappeared while the folder had no valid delta link.
// Messages older than since were outside the pass's window and are left alone,
// as are messages with queued ops (e.g. moved here locally, not yet on the server).
func (s *Store) PruneFolder(ctx context.Context, folderID int64, since time.Time, keep map[string]bool) (int, error) {
	removed := 0
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT graph_id FROM messages m
			WHERE folder_id = ? AND received_at >= ?
			AND NOT EXISTS (SELECT 1 FROM outbox o WHERE o.account_id = m.account_id AND o.message_graph_id = m.graph_id)`,
			folderID, since.Unix())
		if err != nil {
			return err
		}
		var stale []string
		for rows.Next() {
			var gid string
			if err := rows.Scan(&gid); err != nil {
				rows.Close()
				return err
			}
			if !keep[gid] {
				stale = append(stale, gid)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, gid := range stale {
			if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE folder_id = ? AND graph_id = ?`, folderID, gid); err != nil {
				return err
			}
		}
		removed = len(stale)
		return nil
	})
	return removed, err
}

// FolderCounts returns the number of cached and unread messages in a folder.
func (s *Store) FolderCounts(ctx context.Context, folderID int64) (total, unread int, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT count(*), coalesce(sum(is_read = 0), 0) FROM messages WHERE folder_id = ? AND deleted = 0`,
		folderID).Scan(&total, &unread)
	return total, unread, err
}

const messageCols = `id, graph_id, folder_id, conversation_id, internet_message_id, subject, from_name, from_addr,
	to_json, cc_json, received_at, is_read, has_attachments, importance, flagged, categories_json, body_preview`

func scanMessage(row interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var to, cc, cats string
	var received int64
	err := row.Scan(&m.ID, &m.GraphID, &m.FolderID, &m.ConversationID, &m.InternetMessageID, &m.Subject,
		&m.From.Name, &m.From.Addr, &to, &cc, &received, &m.IsRead, &m.HasAttachments, &m.Importance,
		&m.Flagged, &cats, &m.BodyPreview)
	if err != nil {
		return m, err
	}
	m.ReceivedAt = time.Unix(received, 0)
	// These columns are only ever written by jsonOrEmpty, so errors would mean
	// a corrupt row; show the message without them rather than failing the list.
	_ = json.Unmarshal([]byte(to), &m.To)
	_ = json.Unmarshal([]byte(cc), &m.Cc)
	_ = json.Unmarshal([]byte(cats), &m.Categories)
	return m, nil
}

// MessagesInFolder returns up to limit messages, newest first.
func (s *Store) MessagesInFolder(ctx context.Context, folderID int64, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+messageCols+` FROM messages
		WHERE folder_id = ? AND deleted = 0
		ORDER BY received_at DESC LIMIT ?`, folderID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Search runs an FTS5 query over subject, sender and body text, newest first.
func (s *Store) Search(ctx context.Context, accountID int64, query string, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+prefixCols("m.", messageCols)+` FROM messages_fts f
		JOIN messages m ON m.id = f.rowid
		WHERE messages_fts MATCH ? AND m.account_id = ? AND m.deleted = 0
		ORDER BY m.received_at DESC LIMIT ?`, query, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func jsonOrEmpty[T any](v []T) string {
	if len(v) == 0 {
		return "[]"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// prefixCols qualifies each column in a comma-separated list with prefix.
func prefixCols(prefix, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = prefix + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}
