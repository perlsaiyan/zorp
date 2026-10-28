package store

import (
	"context"
	"database/sql"
	"errors"
)

// MessagesNeedingBodies returns graph IDs of messages among the account's
// newest `window` that don't have a body yet, newest first, at most limit.
func (s *Store) MessagesNeedingBodies(ctx context.Context, accountID int64, window, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT graph_id FROM (
			SELECT graph_id, body_fetched, received_at FROM messages
			WHERE account_id = ? AND deleted = 0 AND from_search = 0
			ORDER BY received_at DESC LIMIT ?
		) WHERE body_fetched = 0 ORDER BY received_at DESC LIMIT ?`, accountID, window, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var gid string
		if err := rows.Scan(&gid); err != nil {
			return nil, err
		}
		out = append(out, gid)
	}
	return out, rows.Err()
}

// Body is a message body as sent. For HTML bodies, Text is a plain-text
// rendering used for search.
type Body struct {
	Type string // "html" or "text"
	HTML string
	Text string
}

// SetBody stores a message's body. It is a no-op if the message is no
// longer cached.
func (s *Store) SetBody(ctx context.Context, accountID int64, graphID string, b Body) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE messages SET body_type = ?, body_html = ?, body_text = ?, body_fetched = 1 WHERE account_id = ? AND graph_id = ?`,
		b.Type, b.HTML, b.Text, accountID, graphID)
	return err
}

// MessageBody returns a message's body and whether it has been fetched yet.
func (s *Store) MessageBody(ctx context.Context, id int64) (b Body, fetched bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT body_type, body_html, body_text, body_fetched FROM messages WHERE id = ?`, id).
		Scan(&b.Type, &b.HTML, &b.Text, &fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return b, false, ErrNotFound
	}
	return b, fetched, err
}

// BlankMessage is a cached message with no summary (see BlankMessages).
type BlankMessage struct {
	GraphID  string
	FolderID int64
}

// BlankMessages finds synced messages with no received date: their
// summary was overwritten by a partial delta item and needs fetching again.
func (s *Store) BlankMessages(ctx context.Context, accountID int64) ([]BlankMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT graph_id, folder_id FROM messages
		WHERE account_id = ? AND deleted = 0 AND from_search = 0 AND received_at <= 0
		LIMIT 200`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BlankMessage
	for rows.Next() {
		var b BlankMessage
		if err := rows.Scan(&b.GraphID, &b.FolderID); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// MessageByGraphID returns one cached message's summary.
func (s *Store) MessageByGraphID(ctx context.Context, accountID int64, graphID string) (Message, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx,
		`SELECT `+messageCols+` FROM messages WHERE account_id = ? AND graph_id = ?`, accountID, graphID))
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}
