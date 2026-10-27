package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "zorp.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func setup(t *testing.T) (s *Store, path string, acct int64, inbox, archive Folder) {
	t.Helper()
	ctx := context.Background()
	s, path = openTest(t)
	acct, err := s.UpsertAccount(ctx, "test@example.com", "Test")
	if err != nil {
		t.Fatal(err)
	}
	err = s.ReplaceFolders(ctx, acct, []Folder{
		{GraphID: "AAInbox", Name: "Inbox", WellKnown: "inbox"},
		{GraphID: "AAArchive", Name: "Archive", WellKnown: "archive"},
	})
	if err != nil {
		t.Fatal(err)
	}
	folders, err := s.Folders(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range folders {
		switch f.WellKnown {
		case "inbox":
			inbox = f
		case "archive":
			archive = f
		}
	}
	return s, path, acct, inbox, archive
}

func msg(id, subject string, received time.Time) Message {
	return Message{GraphID: id, Subject: subject, ReceivedAt: received, From: Address{Name: "Alice", Addr: "alice@example.com"}}
}

func TestMoveBetweenFoldersKeepsOneRow(t *testing.T) {
	ctx := context.Background()
	s, _, acct, inbox, archive := setup(t)
	now := time.Now()

	if err := s.ApplyDelta(ctx, acct, inbox.ID, DeltaBatch{Upserts: []Message{msg("m1", "hello", now)}}); err != nil {
		t.Fatal(err)
	}
	// The archive folder's delta reports the moved message before the inbox
	// delta reports its removal (the order is not guaranteed).
	if err := s.ApplyDelta(ctx, acct, archive.ID, DeltaBatch{Upserts: []Message{msg("m1", "hello", now)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyDelta(ctx, acct, inbox.ID, DeltaBatch{Removed: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}

	inInbox, _, _ := s.FolderCounts(ctx, inbox.ID)
	inArchive, _, _ := s.FolderCounts(ctx, archive.ID)
	if inInbox != 0 || inArchive != 1 {
		t.Errorf("inbox=%d archive=%d, want 0 and 1 (stale removal must not delete the moved row)", inInbox, inArchive)
	}
}

func TestReplaceFoldersKeepsDeltaLinkAndDropsMissing(t *testing.T) {
	ctx := context.Background()
	s, _, acct, inbox, archive := setup(t)
	if err := s.SetDeltaLink(ctx, inbox.ID, "https://graph/delta?d=1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyDelta(ctx, acct, archive.ID, DeltaBatch{Upserts: []Message{msg("a1", "old", time.Now())}}); err != nil {
		t.Fatal(err)
	}
	// Archive was deleted on the server; Inbox was renamed.
	if err := s.ReplaceFolders(ctx, acct, []Folder{{GraphID: "AAInbox", Name: "Inbox2", WellKnown: "inbox"}}); err != nil {
		t.Fatal(err)
	}
	folders, err := s.Folders(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 || folders[0].Name != "Inbox2" || folders[0].DeltaLink != "https://graph/delta?d=1" {
		t.Errorf("folders = %+v", folders)
	}
	if n, _, _ := s.FolderCounts(ctx, archive.ID); n != 0 {
		t.Errorf("messages of deleted folder remain: %d", n)
	}
}

func TestPruneOnlyWithinWindow(t *testing.T) {
	ctx := context.Background()
	s, _, acct, inbox, _ := setup(t)
	now := time.Now()
	old := now.AddDate(0, 0, -200)
	since := now.AddDate(0, 0, -90)
	err := s.ApplyDelta(ctx, acct, inbox.ID, DeltaBatch{Upserts: []Message{
		msg("keep", "a", now), msg("gone", "b", now), msg("ancient", "c", old),
	}})
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneFolder(ctx, inbox.ID, since, map[string]bool{"keep": true})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("pruned %d, want 1", n)
	}
	msgs, _ := s.MessagesInFolder(ctx, inbox.ID, 10)
	if len(msgs) != 2 {
		t.Errorf("remaining = %d, want 2 (keep + ancient)", len(msgs))
	}
}

func TestSearchAndReadOnly(t *testing.T) {
	ctx := context.Background()
	s, path, acct, inbox, _ := setup(t)
	err := s.ApplyDelta(ctx, acct, inbox.ID, DeltaBatch{Upserts: []Message{
		msg("m1", "Quarterly numbers", time.Now()), msg("m2", "Lunch plans", time.Now()),
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Updating the subject must keep the FTS index in sync.
	err = s.ApplyDelta(ctx, acct, inbox.ID, DeltaBatch{Upserts: []Message{msg("m2", "Dinner plans", time.Now())}})
	if err != nil {
		t.Fatal(err)
	}

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	for q, want := range map[string]int{"quarterly": 1, "lunch": 0, "dinner": 1, "alice": 2} {
		pq, _ := ParseQuery(q, time.Now())
		got, _, err := ro.Search(ctx, acct, pq, 10)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if len(got) != want {
			t.Errorf("search %q = %d results, want %d", q, len(got), want)
		}
	}
	if _, err := ro.UpsertAccount(ctx, "x@example.com", "x"); err == nil {
		t.Error("read-only store accepted a write")
	}
}

func TestSetFolderSync(t *testing.T) {
	ctx := context.Background()
	s, _, acct, inbox, archive := setup(t)
	s.SetDeltaLink(ctx, archive.ID, "link", time.Now())
	s.ApplyDelta(ctx, acct, archive.ID, DeltaBatch{Upserts: []Message{msg("a1", "x", time.Now())}})

	if err := s.SetFolderSync(ctx, inbox.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFolderSync(ctx, archive.ID, false); err != nil {
		t.Fatal(err)
	}
	in, _ := s.Folder(ctx, inbox.ID)
	ar, _ := s.Folder(ctx, archive.ID)
	if in.SyncOverride == nil || !*in.SyncOverride || ar.SyncOverride == nil || *ar.SyncOverride {
		t.Errorf("overrides: inbox=%v archive=%v", in.SyncOverride, ar.SyncOverride)
	}
	if n, _, _ := s.FolderCounts(ctx, archive.ID); n != 0 || ar.DeltaLink != "" || !ar.LastSync.IsZero() {
		t.Errorf("unsynced folder kept %d messages, link %q", n, ar.DeltaLink)
	}
	// ReplaceFolders (the periodic folder refresh) must keep the choice.
	s.ReplaceFolders(ctx, acct, []Folder{{GraphID: "AAInbox", Name: "Inbox", WellKnown: "inbox"}, {GraphID: "AAArchive", Name: "Archive", WellKnown: "archive"}})
	if ar, _ := s.Folder(ctx, archive.ID); ar.SyncOverride == nil || *ar.SyncOverride {
		t.Error("folder refresh lost the sync override")
	}
	if err := s.SetFolderSync(ctx, 9999, true); err != ErrNotFound {
		t.Errorf("unknown folder: %v", err)
	}
}
