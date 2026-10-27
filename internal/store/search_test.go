package store

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseQuery(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	q, err := ParseQuery(`budget "status report" -newsletter from:"Alice Example" to:bob subject:q3 in:Archive is:unread has:attachment after:7d before:2026-10-25 http://x.example/a:b`, now)
	if err != nil {
		t.Fatal(err)
	}
	want := Query{
		Terms:   []string{"budget", "status report", "http://x.example/a:b"},
		Exclude: []string{"newsletter"},
		From:    []string{"Alice Example"},
		To:      []string{"bob"},
		Subject: []string{"q3"},
		Folder:  "Archive",
		After:   now.AddDate(0, 0, -7),
		Before:  time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC),
	}
	if !reflect.DeepEqual(q.Terms, want.Terms) || !reflect.DeepEqual(q.Exclude, want.Exclude) ||
		!reflect.DeepEqual(q.From, want.From) || !reflect.DeepEqual(q.To, want.To) ||
		!reflect.DeepEqual(q.Subject, want.Subject) || q.Folder != want.Folder ||
		!q.After.Equal(want.After) || !q.Before.Equal(want.Before) ||
		q.Unread == nil || !*q.Unread || q.Attachments == nil || !*q.Attachments || q.Flagged != nil {
		t.Errorf("got %+v", q)
	}
	for _, bad := range []string{"is:spam", "has:pony", "after:yesterday", "before:2026-13-01"} {
		if _, err := ParseQuery(bad, now); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	if q, _ := ParseQuery(`"is:unread" -`, now); q.Unread != nil || !reflect.DeepEqual(q.Terms, []string{"is:unread", "-"}) {
		t.Errorf("quoted key / lone dash: %+v", q)
	}
}

func TestSearchFilters(t *testing.T) {
	ctx := context.Background()
	s, _, acct, inbox, archive := setup(t)
	now := time.Now()
	m1 := msg("m1", "Q3 budget review", now.Add(-time.Hour))
	m1.To = []Address{{Name: "Bob", Addr: "bob@example.com"}}
	m1.HasAttachments = true
	m2 := msg("m2", "Budget newsletter", now.Add(-2*time.Hour))
	m2.IsRead = true
	m3 := msg("m3", "Lunch", now.AddDate(0, 0, -30))
	m3.From = Address{Name: "Carol Jones", Addr: "carol@example.org"}
	m3.Flagged = true
	if err := s.ApplyDelta(ctx, acct, inbox.ID, DeltaBatch{Upserts: []Message{m1, m2}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyDelta(ctx, acct, archive.ID, DeltaBatch{Upserts: []Message{m3}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBody(ctx, acct, "m3", Body{Type: "text", Text: "Shall we try the new ramen place?"}); err != nil {
		t.Fatal(err)
	}

	cases := map[string][]string{
		"budget":                 {"m1", "m2"},
		"budg":                   {"m1", "m2"}, // prefix
		"budget -newsletter":     {"m1"},
		"subject:lunch":          {"m3"},
		"ramen":                  {"m3"}, // body text
		"from:carol":             {"m3"},
		"from:example.org":       {"m3"},
		"to:bob@":                {"m1"},
		"in:archive":             {"m3"},
		"in:INBOX budget":        {"m1", "m2"},
		"is:unread":              {"m1", "m3"},
		"is:read":                {"m2"},
		"is:flagged":             {"m3"},
		"has:attachment":         {"m1"},
		"after:7d":               {"m1", "m2"},
		"before:7d":              {"m3"},
		`"budget review"`:        {"m1"},
		`"review budget"`:        {},
		`odd"quote AND OR NEAR(`: {},                 // FTS syntax is neutralised
		"!!!":                    {"m1", "m2", "m3"}, // nothing searchable: no full-text filter
	}
	for in, want := range cases {
		q, err := ParseQuery(in, now)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		got, more, err := s.Search(ctx, acct, q, 10)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		ids := []string{}
		for _, m := range got {
			ids = append(ids, m.GraphID)
		}
		if !reflect.DeepEqual(ids, want) || more {
			t.Errorf("%q = %v (more=%v), want %v", in, ids, more, want)
		}
	}

	q, _ := ParseQuery("budget", now)
	if got, more, _ := s.Search(ctx, acct, q, 1); len(got) != 1 || !more {
		t.Errorf("limit: %d results, more=%v", len(got), more)
	}
	q, _ = ParseQuery("in:nowhere", now)
	if _, _, err := s.Search(ctx, acct, q, 10); err == nil {
		t.Error("unknown folder: no error")
	}
	q, _ = ParseQuery("-budget", now)
	if _, _, err := s.Search(ctx, acct, q, 10); err == nil {
		t.Error("only exclusions: no error")
	}
}

func TestKQL(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	q, _ := ParseQuery(`budget "status report" from:alice -spam has:attachment after:2026-01-01`, now)
	kql, ok := q.KQL()
	if !ok || kql != `budget "status report" from:alice received>=2026-01-01 hasattachment:true NOT spam` {
		t.Errorf("KQL = %q, %v", kql, ok)
	}
	for _, in := range []string{"budget is:unread", "in:archive x", ""} {
		q, _ := ParseQuery(in, now)
		if _, ok := q.KQL(); ok {
			t.Errorf("%q: KQL should not be possible", in)
		}
	}
}

func TestSearchHitsStayOutOfFolders(t *testing.T) {
	ctx := context.Background()
	s, _, acct, inbox, archive := setup(t)
	now := time.Now()
	if err := s.ApplyDelta(ctx, acct, inbox.ID, DeltaBatch{Upserts: []Message{msg("synced", "Recent", now)}}); err != nil {
		t.Fatal(err)
	}
	old := msg("old", "Ancient budget", now.AddDate(-2, 0, 0))
	changed := msg("synced", "Server title", now)
	err := s.SaveSearchHits(ctx, acct, []SearchHit{{archive.ID, old}, {archive.ID, changed}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if n, _, _ := s.FolderCounts(ctx, archive.ID); n != 0 {
		t.Errorf("search hit counted in folder: %d", n)
	}
	if msgs, _ := s.MessagesInFolder(ctx, archive.ID, 10); len(msgs) != 0 {
		t.Errorf("search hit listed in folder: %v", msgs)
	}
	got, err := s.MessagesByGraphIDs(ctx, acct, []string{"old", "synced", "missing"})
	if err != nil || len(got) != 2 || got[0].GraphID != "synced" || got[1].GraphID != "old" {
		t.Fatalf("MessagesByGraphIDs = %v, %v", got, err)
	}
	if got[0].Subject != "Recent" || got[0].FolderID != inbox.ID {
		t.Errorf("search overwrote a synced message: %+v", got[0])
	}
	q, _ := ParseQuery("ancient", now)
	if res, _, _ := s.Search(ctx, acct, q, 10); len(res) != 1 {
		t.Errorf("local search should find cached search hits: %v", res)
	}
	if ids, _ := s.MessagesNeedingBodies(ctx, acct, 100, 100); len(ids) != 1 || ids[0] != "synced" {
		t.Errorf("bodies prefetched for search hits: %v", ids)
	}

	// A later sync of the folder adopts the message.
	if err := s.ApplyDelta(ctx, acct, archive.ID, DeltaBatch{Upserts: []Message{old}}); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := s.FolderCounts(ctx, archive.ID); n != 1 {
		t.Errorf("synced message not listed: %d", n)
	}

	// Pruning drops only stale search-only rows.
	s.SaveSearchHits(ctx, acct, []SearchHit{{inbox.ID, msg("stale", "x", now.AddDate(-1, 0, 0))}}, now.AddDate(0, 0, -10))
	n, err := s.PruneSearchHits(ctx, acct, now.AddDate(0, 0, -7))
	if err != nil || n != 1 {
		t.Errorf("pruned %d, %v; want 1", n, err)
	}
}

// FuzzSearch checks that no input reaches SQLite as invalid FTS5 syntax.
func FuzzSearch(f *testing.F) {
	for _, seed := range []string{`budget "q3`, `a AND OR NOT (b)`, `from:"x y" -z`, `"" -"" *`, `NEAR(a b)`, `{subject}: x`, `in:Inbox ^x`} {
		f.Add(seed)
	}
	s, err := Open(filepath.Join(f.TempDir(), "zorp.db"))
	if err != nil {
		f.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	acct, _ := s.UpsertAccount(ctx, "f@example.com", "F")
	s.ReplaceFolders(ctx, acct, []Folder{{GraphID: "i", Name: "Inbox", WellKnown: "inbox"}})
	folders, _ := s.Folders(ctx, acct)
	s.ApplyDelta(ctx, acct, folders[0].ID, DeltaBatch{Upserts: []Message{msg("m", "hello world", time.Now())}})
	f.Fuzz(func(t *testing.T, in string) {
		q, err := ParseQuery(in, time.Now())
		if err != nil {
			return
		}
		if _, _, err := s.Search(ctx, acct, q, 5); err != nil && strings.Contains(err.Error(), "fts5") {
			t.Fatalf("%q: %v", in, err)
		}
	})
}
