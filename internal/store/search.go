package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Query is a parsed search. Every part that is set must match.
type Query struct {
	Terms   []string // words or "quoted phrases" anywhere in subject, sender or body
	Exclude []string // -word: must not appear
	From    []string
	To      []string // To or Cc
	Subject []string
	Folder  string // in:name, a folder's display name or well-known name
	After   time.Time
	Before  time.Time

	Unread      *bool // is:unread, is:read
	Flagged     *bool // is:flagged
	Attachments *bool // has:attachment
}

// Empty reports whether the query has nothing to search for.
func (q Query) Empty() bool {
	return len(q.Terms) == 0 && len(q.Exclude) == 0 && len(q.From) == 0 && len(q.To) == 0 &&
		len(q.Subject) == 0 && q.Folder == "" && q.After.IsZero() && q.Before.IsZero() &&
		q.Unread == nil && q.Flagged == nil && q.Attachments == nil
}

// ParseQuery parses the search syntax:
//
//	budget "status report"  words and phrases (all must match; words match prefixes)
//	-newsletter             exclude a word
//	from:alice to:bob subject:invoice
//	in:archive              one folder, by name
//	is:unread is:read is:flagged has:attachment
//	after:2026-01-31 before:2026-03-01, or relative: after:7d (also w, m, y)
//
// Values can be quoted: from:"Alice Example". Unknown prefixes are treated
// as plain words, so URLs and times still search as text.
func ParseQuery(s string, now time.Time) (Query, error) {
	var q Query
	yes, no := true, false
	for _, tok := range tokenize(s) {
		if !tok.quoted {
			if key, val, ok := strings.Cut(tok.text, ":"); ok && val != "" {
				handled := true
				switch strings.ToLower(key) {
				case "from":
					q.From = append(q.From, val)
				case "to":
					q.To = append(q.To, val)
				case "subject":
					q.Subject = append(q.Subject, val)
				case "in":
					q.Folder = val
				case "is":
					switch strings.ToLower(val) {
					case "unread":
						q.Unread = &yes
					case "read":
						q.Unread = &no
					case "flagged":
						q.Flagged = &yes
					case "unflagged":
						q.Flagged = &no
					default:
						return q, fmt.Errorf("unknown is:%s (try is:unread, is:read, is:flagged)", val)
					}
				case "has":
					switch strings.ToLower(val) {
					case "attachment", "attachments":
						q.Attachments = &yes
					default:
						return q, fmt.Errorf("unknown has:%s (try has:attachment)", val)
					}
				case "after", "before":
					t, err := parseWhen(val, now)
					if err != nil {
						return q, fmt.Errorf("%s:%s: %w", key, val, err)
					}
					if strings.EqualFold(key, "after") {
						q.After = t
					} else {
						q.Before = t
					}
				default:
					handled = false
				}
				if handled {
					continue
				}
			}
			if len(tok.text) > 1 && tok.text[0] == '-' {
				q.Exclude = append(q.Exclude, tok.text[1:])
				continue
			}
		}
		q.Terms = append(q.Terms, tok.text)
	}
	return q, nil
}

type token struct {
	text   string
	quoted bool // the whole token was a "quoted phrase"
}

// tokenize splits on spaces, keeping "quoted phrases" (also after a key:)
// together.
func tokenize(s string) []token {
	var out []token
	var cur strings.Builder
	inQuote, quotedWhole := false, false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, token{text: cur.String(), quoted: quotedWhole})
		}
		cur.Reset()
		quotedWhole = false
	}
	for _, r := range s {
		switch {
		case r == '"':
			if !inQuote && cur.Len() == 0 {
				quotedWhole = true
			}
			inQuote = !inQuote
		case unicode.IsSpace(r) && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// parseWhen parses a date (2006-01-02) or a relative age (7d, 2w, 3m, 1y).
func parseWhen(s string, now time.Time) (time.Time, error) {
	if t, err := time.ParseInLocation("2006-01-02", s, now.Location()); err == nil {
		return t, nil
	}
	if len(s) >= 2 {
		n, err := strconv.Atoi(s[:len(s)-1])
		if err == nil && n >= 0 {
			switch s[len(s)-1] {
			case 'd':
				return now.AddDate(0, 0, -n), nil
			case 'w':
				return now.AddDate(0, 0, -7*n), nil
			case 'm':
				return now.AddDate(0, -n, 0), nil
			case 'y':
				return now.AddDate(-n, 0, 0), nil
			}
		}
	}
	return time.Time{}, errors.New("want a date like 2026-01-31 or an age like 7d, 2w, 3m, 1y")
}

// searchable reports whether s has anything the FTS tokenizer indexes.
func searchable(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0
}

// ftsPrefix quotes s as an FTS5 phrase whose last word matches as a prefix.
// Quoting makes any user input safe to pass to MATCH.
func ftsPrefix(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"*`
}

// match builds the FTS5 MATCH expression, or "" when the query has no
// full-text part.
func (q Query) match() (string, error) {
	var pos []string
	for _, t := range q.Terms {
		if searchable(t) {
			pos = append(pos, ftsPrefix(t))
		}
	}
	for _, f := range q.From {
		if searchable(f) {
			pos = append(pos, "{from_name from_addr} : "+ftsPrefix(f))
		}
	}
	for _, s := range q.Subject {
		if searchable(s) {
			pos = append(pos, "subject : "+ftsPrefix(s))
		}
	}
	expr := strings.Join(pos, " AND ")
	for _, x := range q.Exclude {
		if !searchable(x) {
			continue
		}
		if expr == "" {
			return "", errors.New("add something to search for besides excluded words")
		}
		expr += " NOT " + ftsPrefix(x)
	}
	return expr, nil
}

// Search returns cached messages matching q, newest first, at most limit.
// more reports whether there were further matches.
func (s *Store) Search(ctx context.Context, accountID int64, q Query, limit int) (msgs []Message, more bool, err error) {
	match, err := q.match()
	if err != nil {
		return nil, false, err
	}
	from := `messages m`
	where := []string{"m.account_id = ?", "m.deleted = 0"}
	args := []any{accountID}
	if match != "" {
		from = `messages_fts f JOIN messages m ON m.id = f.rowid`
		where = append(where, "messages_fts MATCH ?")
		args = append(args, match)
	}
	for _, t := range q.To {
		where = append(where, "instr(lower(m.to_json || m.cc_json), ?) > 0")
		args = append(args, strings.ToLower(t))
	}
	if q.Folder != "" {
		where = append(where, `m.folder_id IN (SELECT id FROM folders WHERE account_id = ? AND (lower(name) = lower(?) OR well_known = lower(?)))`)
		args = append(args, accountID, q.Folder, q.Folder)
		var n int
		err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM folders WHERE account_id = ? AND (lower(name) = lower(?) OR well_known = lower(?))`,
			accountID, q.Folder, q.Folder).Scan(&n)
		if err != nil {
			return nil, false, err
		}
		if n == 0 {
			return nil, false, fmt.Errorf("no folder named %q", q.Folder)
		}
	}
	if !q.After.IsZero() {
		where = append(where, "m.received_at >= ?")
		args = append(args, q.After.Unix())
	}
	if !q.Before.IsZero() {
		where = append(where, "m.received_at < ?")
		args = append(args, q.Before.Unix())
	}
	boolCond := func(col string, v *bool) {
		if v != nil {
			where = append(where, col+" = ?")
			args = append(args, *v)
		}
	}
	if q.Unread != nil {
		read := !*q.Unread
		boolCond("m.is_read", &read)
	}
	boolCond("m.flagged", q.Flagged)
	boolCond("m.has_attachments", q.Attachments)

	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+prefixCols("m.", messageCols)+` FROM `+from+`
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY m.received_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, false, err
	}
	msgs, err = scanMessages(rows)
	if len(msgs) > limit {
		return msgs[:limit], true, err
	}
	return msgs, false, err
}

// KQL translates the query for Graph's server-side $search. ok is false if
// the query uses filters Graph search can't express (in:, is:).
func (q Query) KQL() (kql string, ok bool) {
	if q.Folder != "" || q.Unread != nil || q.Flagged != nil || (q.Attachments != nil && !*q.Attachments) {
		return "", false
	}
	quote := func(s string) string {
		if strings.ContainsAny(s, " \t:()") {
			return `"` + strings.ReplaceAll(s, `"`, "") + `"`
		}
		return s
	}
	var parts []string
	for _, t := range q.Terms {
		parts = append(parts, quote(t))
	}
	for _, f := range q.From {
		parts = append(parts, "from:"+quote(f))
	}
	for _, t := range q.To {
		parts = append(parts, "to:"+quote(t))
	}
	for _, s := range q.Subject {
		parts = append(parts, "subject:"+quote(s))
	}
	if !q.After.IsZero() {
		parts = append(parts, "received>="+q.After.Format("2006-01-02"))
	}
	if !q.Before.IsZero() {
		parts = append(parts, "received<"+q.Before.Format("2006-01-02"))
	}
	if q.Attachments != nil {
		parts = append(parts, "hasattachment:true")
	}
	for _, x := range q.Exclude {
		parts = append(parts, "NOT "+quote(x))
	}
	return strings.Join(parts, " "), len(parts) > 0
}

// SearchHit is a message found by a server-side search, in a known folder.
type SearchHit struct {
	FolderID int64
	Message  Message
}

// SaveSearchHits caches messages found by a server search so they can be
// opened and acted on. Messages already cached by sync are left alone (sync
// keeps them current); the others are marked as search results, which keeps
// them out of folder lists.
func (s *Store) SaveSearchHits(ctx context.Context, accountID int64, hits []SearchHit, now time.Time) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		pending, err := hasPendingOps(ctx, tx, accountID)
		if err != nil {
			return err
		}
		for _, h := range hits {
			m := h.Message
			_, err := tx.ExecContext(ctx, `
				INSERT INTO messages (
					account_id, graph_id, folder_id, conversation_id, internet_message_id, subject,
					from_name, from_addr, to_json, cc_json, received_at, is_read, has_attachments,
					importance, flagged, categories_json, body_preview, reply_to_json, from_search, searched_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)
				ON CONFLICT (account_id, graph_id) DO UPDATE SET
					folder_id           = excluded.folder_id,
					subject             = excluded.subject,
					from_name           = excluded.from_name,
					from_addr           = excluded.from_addr,
					to_json             = excluded.to_json,
					cc_json             = excluded.cc_json,
					is_read             = excluded.is_read,
					has_attachments     = excluded.has_attachments,
					importance          = excluded.importance,
					flagged             = excluded.flagged,
					categories_json     = excluded.categories_json,
					body_preview        = excluded.body_preview,
					reply_to_json       = excluded.reply_to_json,
					searched_at         = excluded.searched_at
				WHERE messages.from_search = 1`,
				accountID, m.GraphID, h.FolderID, m.ConversationID, m.InternetMessageID, m.Subject,
				m.From.Name, m.From.Addr, jsonOrEmpty(m.To), jsonOrEmpty(m.Cc), m.ReceivedAt.Unix(), m.IsRead, m.HasAttachments,
				m.Importance, m.Flagged, jsonOrEmpty(m.Categories), m.BodyPreview, jsonOrEmpty(m.ReplyTo), now.Unix())
			if err != nil {
				return err
			}
			if pending {
				if err := rebase(ctx, tx, accountID, m.GraphID); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// PruneSearchHits drops search-only messages last found before cutoff,
// unless they have queued changes.
func (s *Store) PruneSearchHits(ctx context.Context, accountID int64, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM messages
		WHERE account_id = ? AND from_search = 1 AND searched_at < ?
		AND NOT EXISTS (SELECT 1 FROM outbox o WHERE o.account_id = messages.account_id AND o.message_graph_id = messages.graph_id)`,
		accountID, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MessagesByGraphIDs returns the cached messages among ids, newest first.
func (s *Store) MessagesByGraphIDs(ctx context.Context, accountID int64, ids []string) ([]Message, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := []any{accountID}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageCols+` FROM messages
		WHERE account_id = ? AND deleted = 0 AND graph_id IN (?`+strings.Repeat(", ?", len(ids)-1)+`)
		ORDER BY received_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
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

// MessagesInConversation returns a conversation's cached messages (in any
// folder), oldest first, at most limit.
func (s *Store) MessagesInConversation(ctx context.Context, accountID int64, conversationID string, limit int) ([]Message, error) {
	if conversationID == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageCols+` FROM messages
		WHERE account_id = ? AND conversation_id = ? AND deleted = 0
		ORDER BY received_at ASC LIMIT ?`, accountID, conversationID, limit)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

// KnownGraphIDs reports which of ids are already cached (in any folder).
func (s *Store) KnownGraphIDs(ctx context.Context, accountID int64, ids []string) (map[string]bool, error) {
	known := map[string]bool{}
	for len(ids) > 0 {
		n := min(len(ids), 200)
		args := []any{accountID}
		for _, id := range ids[:n] {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT graph_id FROM messages WHERE account_id = ? AND graph_id IN (?`+
			strings.Repeat(",?", n-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			known[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		ids = ids[n:]
	}
	return known, nil
}
