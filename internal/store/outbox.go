package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// OpKind is a kind of queued mutation.
type OpKind string

const (
	OpSetRead OpKind = "set_read"
	OpSetFlag OpKind = "set_flag"
	OpMove    OpKind = "move"
)

// Op is a mutation applied locally and waiting to be sent to Graph.
type Op struct {
	ID        int64
	AccountID int64
	GraphID   string
	Kind      OpKind
	Attempts  int
	LastError string
	CreatedAt time.Time
	opPayload
}

type opPayload struct {
	Value        bool   `json:"value,omitempty"`          // set_read, set_flag
	DestFolderID int64  `json:"dest_folder_id,omitempty"` // move
	DestGraphID  string `json:"dest_graph_id,omitempty"`  // move
	// State before this op, for reverting it if Graph rejects it.
	PrevRead     bool  `json:"prev_read"`
	PrevFlagged  bool  `json:"prev_flagged"`
	PrevFolderID int64 `json:"prev_folder_id"`
}

// Mutation describes a change the user asked for.
type Mutation struct {
	Kind  OpKind
	Value bool   // read / flagged
	Dest  Folder // destination for moves
}

// Enqueue applies m to the cached message immediately (optimistic update)
// and queues it for Graph. It returns the IDs of folders whose contents
// changed. A mutation that wouldn't change anything is not queued.
func (s *Store) Enqueue(ctx context.Context, accountID int64, graphID string, m Mutation) (changed []int64, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var folderID int64
		var read, flagged bool
		err := tx.QueryRowContext(ctx,
			`SELECT folder_id, is_read, flagged FROM messages WHERE account_id = ? AND graph_id = ?`,
			accountID, graphID).Scan(&folderID, &read, &flagged)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		op := Op{AccountID: accountID, GraphID: graphID, Kind: m.Kind, opPayload: opPayload{
			Value: m.Value, PrevRead: read, PrevFlagged: flagged, PrevFolderID: folderID,
		}}
		switch m.Kind {
		case OpSetRead:
			if read == m.Value {
				return nil
			}
		case OpSetFlag:
			if flagged == m.Value {
				return nil
			}
		case OpMove:
			if m.Dest.ID == folderID {
				return nil
			}
			op.DestFolderID, op.DestGraphID = m.Dest.ID, m.Dest.GraphID
			changed = append(changed, m.Dest.ID)
		default:
			return errors.New("unknown mutation " + string(m.Kind))
		}
		changed = append(changed, folderID)
		if err := applyOp(ctx, tx, op); err != nil {
			return err
		}
		payload, err := json.Marshal(op.opPayload)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO outbox (account_id, message_graph_id, op, payload_json, created_at)
			VALUES (?, ?, ?, ?, ?)`, accountID, graphID, string(op.Kind), string(payload), time.Now().Unix())
		return err
	})
	return changed, err
}

// applyOp writes an op's effect onto the cached message, if it's cached.
func applyOp(ctx context.Context, tx *sql.Tx, op Op) error {
	var err error
	switch op.Kind {
	case OpSetRead:
		_, err = tx.ExecContext(ctx, `UPDATE messages SET is_read = ? WHERE account_id = ? AND graph_id = ?`,
			op.Value, op.AccountID, op.GraphID)
	case OpSetFlag:
		_, err = tx.ExecContext(ctx, `UPDATE messages SET flagged = ? WHERE account_id = ? AND graph_id = ?`,
			op.Value, op.AccountID, op.GraphID)
	case OpMove:
		_, err = tx.ExecContext(ctx, `UPDATE messages SET folder_id = ? WHERE account_id = ? AND graph_id = ?`,
			op.DestFolderID, op.AccountID, op.GraphID)
	}
	return err
}

// rebase re-applies a message's queued ops, oldest first, on top of whatever
// was just written to its row. This is what keeps a sync that lands before
// an op is flushed from undoing the user's change on screen.
func rebase(ctx context.Context, tx *sql.Tx, accountID int64, graphID string) error {
	ops, err := queryOps(ctx, tx, `WHERE account_id = ? AND message_graph_id = ? ORDER BY id`, accountID, graphID)
	if err != nil {
		return err
	}
	for _, op := range ops {
		if err := applyOp(ctx, tx, op); err != nil {
			return err
		}
	}
	return nil
}

func hasPendingOps(ctx context.Context, tx *sql.Tx, accountID int64) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM outbox WHERE account_id = ?)`, accountID).Scan(&n)
	return n == 1, err
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func queryOps(ctx context.Context, q querier, where string, args ...any) ([]Op, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, account_id, message_graph_id, op, payload_json, attempts, last_error, created_at
		FROM outbox `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Op
	for rows.Next() {
		var op Op
		var kind, payload string
		var created int64
		if err := rows.Scan(&op.ID, &op.AccountID, &op.GraphID, &kind, &payload, &op.Attempts, &op.LastError, &created); err != nil {
			return nil, err
		}
		op.Kind = OpKind(kind)
		op.CreatedAt = time.Unix(created, 0)
		if err := json.Unmarshal([]byte(payload), &op.opPayload); err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// PendingOps returns queued ops, oldest first.
func (s *Store) PendingOps(ctx context.Context, limit int) ([]Op, error) {
	return queryOps(ctx, s.db, `ORDER BY id LIMIT ?`, limit)
}

// PendingCount returns the number of queued ops.
func (s *Store) PendingCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM outbox`).Scan(&n)
	return n, err
}

// CompleteOp removes an op that Graph accepted. With dropLocal, the cached
// message is deleted too (used when it moved to a folder that isn't synced,
// so no delta will ever update it again).
func (s *Store) CompleteOp(ctx context.Context, op Op, dropLocal bool) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE id = ?`, op.ID); err != nil {
			return err
		}
		if !dropLocal {
			return nil
		}
		_, err := tx.ExecContext(ctx, `
			DELETE FROM messages WHERE account_id = ? AND graph_id = ?
			AND NOT EXISTS (SELECT 1 FROM outbox WHERE account_id = ? AND message_graph_id = ?)`,
			op.AccountID, op.GraphID, op.AccountID, op.GraphID)
		return err
	})
}

// RetryOp records a transient failure; the op stays queued.
func (s *Store) RetryOp(ctx context.Context, op Op, errText string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outbox SET attempts = attempts + 1, last_error = ? WHERE id = ?`, errText, op.ID)
	return err
}

// RevertOp drops an op Graph rejected and undoes its local effect, then
// re-applies any later ops for the same message. It returns the folders
// whose contents changed.
func (s *Store) RevertOp(ctx context.Context, op Op) (changed []int64, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE id = ?`, op.ID); err != nil {
			return err
		}
		var res sql.Result
		var err error
		switch op.Kind {
		case OpSetRead:
			res, err = tx.ExecContext(ctx, `UPDATE messages SET is_read = ? WHERE account_id = ? AND graph_id = ?`,
				op.PrevRead, op.AccountID, op.GraphID)
		case OpSetFlag:
			res, err = tx.ExecContext(ctx, `UPDATE messages SET flagged = ? WHERE account_id = ? AND graph_id = ?`,
				op.PrevFlagged, op.AccountID, op.GraphID)
		case OpMove:
			res, err = tx.ExecContext(ctx, `UPDATE messages SET folder_id = ? WHERE account_id = ? AND graph_id = ?`,
				op.PrevFolderID, op.AccountID, op.GraphID)
		}
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			changed = append(changed, op.PrevFolderID)
			if op.Kind == OpMove {
				changed = append(changed, op.DestFolderID)
			}
		}
		return rebase(ctx, tx, op.AccountID, op.GraphID)
	})
	return changed, err
}

// FolderByWellKnown finds a folder by well-known name ("archive", ...).
func (s *Store) FolderByWellKnown(ctx context.Context, accountID int64, name string) (Folder, error) {
	f, err := scanFolder(s.db.QueryRowContext(ctx,
		`SELECT `+folderCols+` FROM folders WHERE account_id = ? AND well_known = ?`, accountID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	return f, err
}

// FolderByName finds a folder by display name, ignoring case.
func (s *Store) FolderByName(ctx context.Context, accountID int64, name string) (Folder, error) {
	f, err := scanFolder(s.db.QueryRowContext(ctx,
		`SELECT `+folderCols+` FROM folders WHERE account_id = ? AND name = ? COLLATE NOCASE ORDER BY id LIMIT 1`, accountID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	return f, err
}
