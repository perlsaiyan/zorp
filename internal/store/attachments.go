package store

import (
	"context"
	"database/sql"
	"errors"
)

// Attachment is a cached entry of a message's attachment list.
type Attachment struct {
	GraphID     string
	Name        string
	ContentType string
	Kind        string // "file", "item" (an attached email) or "reference"
	Size        int64
	IsInline    bool
}

// SetAttachments replaces a message's cached attachment list. It is a no-op
// if the message is no longer cached.
func (s *Store) SetAttachments(ctx context.Context, accountID int64, graphID string, atts []Attachment) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE account_id = ? AND graph_id = ?`, accountID, graphID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM attachments WHERE message_id = ?`, id); err != nil {
			return err
		}
		for _, a := range atts {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO attachments (message_id, graph_id, name, content_type, size, is_inline, kind)
				VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (message_id, graph_id) DO NOTHING`,
				id, a.GraphID, a.Name, a.ContentType, a.Size, a.IsInline, a.Kind); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE messages SET attachments_fetched = 1 WHERE id = ?`, id)
		return err
	})
}

// Attachments returns a message's cached attachment list, in the server's
// order, and whether it has been fetched at all.
func (s *Store) Attachments(ctx context.Context, messageID int64) (atts []Attachment, known bool, err error) {
	if err := s.db.QueryRowContext(ctx, `SELECT attachments_fetched FROM messages WHERE id = ?`, messageID).Scan(&known); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, ErrNotFound
		}
		return nil, false, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT graph_id, name, content_type, kind, size, is_inline FROM attachments
		WHERE message_id = ? ORDER BY id`, messageID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var a Attachment
		if err := rows.Scan(&a.GraphID, &a.Name, &a.ContentType, &a.Kind, &a.Size, &a.IsInline); err != nil {
			return nil, false, err
		}
		atts = append(atts, a)
	}
	return atts, known, rows.Err()
}
