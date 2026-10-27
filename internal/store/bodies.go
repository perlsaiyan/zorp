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
			WHERE account_id = ? AND deleted = 0
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

// SetBody stores a message's plain-text body. It is a no-op if the message
// is no longer cached.
func (s *Store) SetBody(ctx context.Context, accountID int64, graphID, text string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE messages SET body_text = ?, body_fetched = 1 WHERE account_id = ? AND graph_id = ?`,
		text, accountID, graphID)
	return err
}

// MessageBody returns a message's plain-text body and whether it has been
// fetched yet.
func (s *Store) MessageBody(ctx context.Context, id int64) (text string, fetched bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT body_text, body_fetched FROM messages WHERE id = ?`, id).Scan(&text, &fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrNotFound
	}
	return text, fetched, err
}
