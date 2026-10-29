// Package store is zorp's local SQLite cache. The daemon opens it read-write
// and is the only writer; every other process opens it read-only.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type Store struct {
	db       *sql.DB
	readOnly bool
}

// Open opens the database read-write, creating and migrating it as needed.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection serializes all writes through one writer, which
	// rules out SQLITE_BUSY between our own goroutines.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// OpenReadOnly opens an existing database for queries only.
func OpenReadOnly(path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no local cache yet (%w); run `zorp sync`", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, readOnly: true}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version != len(migrations) {
		db.Close()
		return nil, fmt.Errorf("cache schema is version %d, this zorp expects %d; restart the daemon", version, len(migrations))
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// migrations are applied in order; never edit one that has shipped, append a new one.
var migrations = []string{
	`
CREATE TABLE accounts (
	id           INTEGER PRIMARY KEY,
	upn          TEXT NOT NULL UNIQUE,
	display_name TEXT NOT NULL DEFAULT ''
);

CREATE TABLE folders (
	id              INTEGER PRIMARY KEY,
	account_id      INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	graph_id        TEXT NOT NULL,
	parent_graph_id TEXT NOT NULL DEFAULT '',
	name            TEXT NOT NULL,
	well_known      TEXT NOT NULL DEFAULT '',
	unread_count    INTEGER NOT NULL DEFAULT 0,
	total_count     INTEGER NOT NULL DEFAULT 0,
	delta_link      TEXT NOT NULL DEFAULT '',
	last_sync       INTEGER NOT NULL DEFAULT 0,
	UNIQUE (account_id, graph_id)
);

CREATE TABLE messages (
	id                  INTEGER PRIMARY KEY,
	account_id          INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	graph_id            TEXT NOT NULL,
	folder_id           INTEGER NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
	conversation_id     TEXT NOT NULL DEFAULT '',
	internet_message_id TEXT NOT NULL DEFAULT '',
	subject             TEXT NOT NULL DEFAULT '',
	from_name           TEXT NOT NULL DEFAULT '',
	from_addr           TEXT NOT NULL DEFAULT '',
	to_json             TEXT NOT NULL DEFAULT '[]',
	cc_json             TEXT NOT NULL DEFAULT '[]',
	received_at         INTEGER NOT NULL DEFAULT 0,
	is_read             INTEGER NOT NULL DEFAULT 0,
	has_attachments     INTEGER NOT NULL DEFAULT 0,
	importance          TEXT NOT NULL DEFAULT 'normal',
	flagged             INTEGER NOT NULL DEFAULT 0,
	categories_json     TEXT NOT NULL DEFAULT '[]',
	body_preview        TEXT NOT NULL DEFAULT '',
	body_html           TEXT NOT NULL DEFAULT '',
	body_text           TEXT NOT NULL DEFAULT '',
	body_fetched        INTEGER NOT NULL DEFAULT 0,
	deleted             INTEGER NOT NULL DEFAULT 0,
	UNIQUE (account_id, graph_id)
);
CREATE INDEX messages_folder_received ON messages (folder_id, received_at DESC);
CREATE INDEX messages_conversation ON messages (conversation_id);

CREATE TABLE attachments (
	id           INTEGER PRIMARY KEY,
	message_id   INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	graph_id     TEXT NOT NULL,
	name         TEXT NOT NULL DEFAULT '',
	content_type TEXT NOT NULL DEFAULT '',
	size         INTEGER NOT NULL DEFAULT 0,
	is_inline    INTEGER NOT NULL DEFAULT 0,
	content_id   TEXT NOT NULL DEFAULT '',
	local_path   TEXT NOT NULL DEFAULT '',
	UNIQUE (message_id, graph_id)
);

CREATE VIRTUAL TABLE messages_fts USING fts5 (
	subject, from_name, from_addr, body_text,
	content = 'messages', content_rowid = 'id'
);
CREATE TRIGGER messages_fts_ai AFTER INSERT ON messages BEGIN
	INSERT INTO messages_fts (rowid, subject, from_name, from_addr, body_text)
	VALUES (new.id, new.subject, new.from_name, new.from_addr, new.body_text);
END;
CREATE TRIGGER messages_fts_ad AFTER DELETE ON messages BEGIN
	INSERT INTO messages_fts (messages_fts, rowid, subject, from_name, from_addr, body_text)
	VALUES ('delete', old.id, old.subject, old.from_name, old.from_addr, old.body_text);
END;
CREATE TRIGGER messages_fts_au AFTER UPDATE OF subject, from_name, from_addr, body_text ON messages BEGIN
	INSERT INTO messages_fts (messages_fts, rowid, subject, from_name, from_addr, body_text)
	VALUES ('delete', old.id, old.subject, old.from_name, old.from_addr, old.body_text);
	INSERT INTO messages_fts (rowid, subject, from_name, from_addr, body_text)
	VALUES (new.id, new.subject, new.from_name, new.from_addr, new.body_text);
END;

CREATE TABLE outbox (
	id               INTEGER PRIMARY KEY,
	account_id       INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	message_graph_id TEXT NOT NULL DEFAULT '',
	op               TEXT NOT NULL,
	payload_json     TEXT NOT NULL DEFAULT '{}',
	created_at       INTEGER NOT NULL,
	attempts         INTEGER NOT NULL DEFAULT 0,
	last_error       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX outbox_message ON outbox (account_id, message_graph_id);
`,
	// 2: bodies are stored in their original format (HTML or text) and
	// rendered locally. Bodies cached as Graph's text conversion are
	// dropped so they're refetched.
	`
ALTER TABLE messages ADD COLUMN body_type TEXT NOT NULL DEFAULT '';
UPDATE messages SET body_fetched = 0, body_text = '', body_html = '' WHERE body_fetched = 1;
`,
	// 3: per-folder sync choice made in the TUI. NULL means "as configured".
	`
ALTER TABLE folders ADD COLUMN sync_override INTEGER;
`,
	// 4: messages found by a server search outside the synced window. They
	// are kept for opening and acting on, but don't appear in folder lists,
	// and are dropped some days after the last search that found them.
	`
ALTER TABLE messages ADD COLUMN from_search INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN searched_at INTEGER NOT NULL DEFAULT 0;
`,
	// 5: Reply-To addresses, for replying. Delta links are dropped so the
	// next sync re-reads each folder's window with the new field selected.
	`
ALTER TABLE messages ADD COLUMN reply_to_json TEXT NOT NULL DEFAULT '[]';
UPDATE folders SET delta_link = '';
`,
	// 6: attachment lists are cached (fetched with the body, or when
	// listed), with their kind: file, item (an attached email) or reference.
	`
ALTER TABLE messages ADD COLUMN attachments_fetched INTEGER NOT NULL DEFAULT 0;
ALTER TABLE attachments ADD COLUMN kind TEXT NOT NULL DEFAULT 'file';
`,
	// 7: what meeting messages say about their meeting (JSON, '' for
	// other mail). Delta links are dropped so each folder's window is
	// re-read with the meeting fields selected.
	`
ALTER TABLE messages ADD COLUMN meeting_json TEXT NOT NULL DEFAULT '';
UPDATE folders SET delta_link = '';
`,
}
