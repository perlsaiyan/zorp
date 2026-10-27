package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Folder struct {
	ID            int64
	AccountID     int64
	GraphID       string
	ParentGraphID string
	Name          string
	WellKnown     string // "inbox", "sentitems", ... or "" for user folders
	UnreadCount   int
	TotalCount    int
	DeltaLink     string
	LastSync      time.Time
}

// ErrNotFound is returned when a looked-up row doesn't exist.
var ErrNotFound = errors.New("not found")

// UpsertAccount records the signed-in user and returns the account's ID.
func (s *Store) UpsertAccount(ctx context.Context, upn, displayName string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO accounts (upn, display_name) VALUES (?, ?)
		ON CONFLICT (upn) DO UPDATE SET display_name = excluded.display_name
		RETURNING id`, upn, displayName).Scan(&id)
	return id, err
}

// ReplaceFolders makes the account's folder table match folders: new ones are
// inserted, existing ones updated (keeping their delta link), and folders that
// no longer exist are deleted along with their cached messages.
func (s *Store) ReplaceFolders(ctx context.Context, accountID int64, folders []Folder) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS seen_folders (graph_id TEXT PRIMARY KEY)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM seen_folders`); err != nil {
			return err
		}
		for _, f := range folders {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO folders (account_id, graph_id, parent_graph_id, name, well_known, unread_count, total_count)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (account_id, graph_id) DO UPDATE SET
					parent_graph_id = excluded.parent_graph_id,
					name            = excluded.name,
					well_known      = excluded.well_known,
					unread_count    = excluded.unread_count,
					total_count     = excluded.total_count`,
				accountID, f.GraphID, f.ParentGraphID, f.Name, f.WellKnown, f.UnreadCount, f.TotalCount)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO seen_folders (graph_id) VALUES (?)`, f.GraphID); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `
			DELETE FROM folders
			WHERE account_id = ? AND graph_id NOT IN (SELECT graph_id FROM seen_folders)`, accountID)
		return err
	})
}

const folderCols = `id, account_id, graph_id, parent_graph_id, name, well_known, unread_count, total_count, delta_link, last_sync`

func scanFolder(row interface{ Scan(...any) error }) (Folder, error) {
	var f Folder
	var lastSync int64
	err := row.Scan(&f.ID, &f.AccountID, &f.GraphID, &f.ParentGraphID, &f.Name, &f.WellKnown,
		&f.UnreadCount, &f.TotalCount, &f.DeltaLink, &lastSync)
	if lastSync != 0 {
		f.LastSync = time.Unix(lastSync, 0)
	}
	return f, err
}

// Folders returns all of the account's folders, ordered by name.
func (s *Store) Folders(ctx context.Context, accountID int64) ([]Folder, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+folderCols+` FROM folders WHERE account_id = ? ORDER BY name COLLATE NOCASE`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Folder
	for rows.Next() {
		f, err := scanFolder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Folder returns one folder by its local ID.
func (s *Store) Folder(ctx context.Context, id int64) (Folder, error) {
	f, err := scanFolder(s.db.QueryRowContext(ctx, `SELECT `+folderCols+` FROM folders WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	return f, err
}

// SetDeltaLink stores the folder's delta link after a completed sync pass.
// An empty link forces the next sync of the folder to start over.
func (s *Store) SetDeltaLink(ctx context.Context, folderID int64, link string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE folders SET delta_link = ?, last_sync = ? WHERE id = ?`, link, at.Unix(), folderID)
	return err
}

// ResetSync clears every delta link and cached message for the account, so
// the next sync rebuilds from scratch. Folders are kept.
func (s *Store) ResetSync(ctx context.Context, accountID int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE account_id = ?`, accountID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE folders SET delta_link = '', last_sync = 0 WHERE account_id = ?`, accountID)
		return err
	})
}

type Account struct {
	ID          int64
	UPN         string
	DisplayName string
}

// DefaultAccount returns the first (for now, only) account.
func (s *Store) DefaultAccount(ctx context.Context) (Account, error) {
	var a Account
	err := s.db.QueryRowContext(ctx, `SELECT id, upn, display_name FROM accounts ORDER BY id LIMIT 1`).
		Scan(&a.ID, &a.UPN, &a.DisplayName)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// LocalFolderCounts returns cached and unread message counts per folder, for
// folders that have any cached messages.
func (s *Store) LocalFolderCounts(ctx context.Context, accountID int64) (total, unread map[int64]int, err error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT folder_id, count(*), coalesce(sum(is_read = 0), 0) FROM messages
		WHERE account_id = ? AND deleted = 0 GROUP BY folder_id`, accountID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	total, unread = map[int64]int{}, map[int64]int{}
	for rows.Next() {
		var id int64
		var t, u int
		if err := rows.Scan(&id, &t, &u); err != nil {
			return nil, nil, err
		}
		total[id], unread[id] = t, u
	}
	return total, unread, rows.Err()
}
