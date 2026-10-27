package store

import (
	"context"
	"testing"
	"time"
)

func getMsg(t *testing.T, s *Store, acct int64, gid string) (Message, bool) {
	t.Helper()
	m, err := scanMessage(s.db.QueryRow(`SELECT `+messageCols+` FROM messages WHERE account_id = ? AND graph_id = ?`, acct, gid))
	if err != nil {
		return m, false
	}
	return m, true
}

func TestSyncBeforeFlushDoesNotUndoLocalChange(t *testing.T) {
	ctx := context.Background()
	s, _, acct, inbox, _ := setup(t)
	unread := msg("m1", "hi", time.Now())
	if err := s.ApplyDelta(ctx, acct, inbox.ID, DeltaBatch{Upserts: []Message{unread}}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Enqueue(ctx, acct, "m1", Mutation{Kind: OpSetRead, Value: true}); err != nil {
		t.Fatal(err)
	}
	// A delta page fetched before the PATCH went out still says unread.
	if err := s.ApplyDelta(ctx, acct, inbox.ID, DeltaBatch{Upserts: []Message{unread}}); err != nil {
		t.Fatal(err)
	}
	if m, _ := getMsg(t, s, acct, "m1"); !m.IsRead {
		t.Fatal("stale delta undid the pending mark-as-read")
	}

	ops, _ := s.PendingOps(ctx, 10)
	if len(ops) != 1 {
		t.Fatalf("pending ops = %d", len(ops))
	}
	if err := s.CompleteOp(ctx, ops[0], false); err != nil {
		t.Fatal(err)
	}
	// Once flushed, the server is authoritative again: another client marks
	// it unread and we follow.
	if err := s.ApplyDelta(ctx, acct, inbox.ID, DeltaBatch{Upserts: []Message{unread}}); err != nil {
		t.Fatal(err)
	}
	if m, _ := getMsg(t, s, acct, "m1"); m.IsRead {
		t.Error("after flush, delta should be authoritative")
	}
}

func TestPendingMoveSurvivesSourceDeltaAndPrune(t *testing.T) {
	ctx := context.Background()
	s, _, acct, inbox, archive := setup(t)
	m1 := msg("m1", "hi", time.Now())
	if err := s.ApplyDelta(ctx, acct, inbox.ID, DeltaBatch{Upserts: []Message{m1}}); err != nil {
		t.Fatal(err)
	}
	changed, err := s.Enqueue(ctx, acct, "m1", Mutation{Kind: OpMove, Dest: archive})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 2 {
		t.Errorf("changed folders = %v, want inbox and archive", changed)
	}
	// The inbox delta still has it (move not sent yet).
	if err := s.ApplyDelta(ctx, acct, inbox.ID, DeltaBatch{Upserts: []Message{m1}}); err != nil {
		t.Fatal(err)
	}
	if m, _ := getMsg(t, s, acct, "m1"); m.FolderID != archive.ID {
		t.Fatalf("stale delta moved message back to folder %d", m.FolderID)
	}
	// A from-scratch pass of the archive doesn't include it yet; prune must
	// not delete it while the move is queued.
	if _, err := s.PruneFolder(ctx, archive.ID, time.Time{}, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := getMsg(t, s, acct, "m1"); !ok {
		t.Fatal("prune deleted a message with a pending move")
	}

	ops, _ := s.PendingOps(ctx, 10)
	if err := s.CompleteOp(ctx, ops[0], false); err != nil {
		t.Fatal(err)
	}
	// After the move: the inbox reports removal (stale for our row) and the
	// archive reports the message.
	if err := s.ApplyDelta(ctx, acct, inbox.ID, DeltaBatch{Removed: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyDelta(ctx, acct, archive.ID, DeltaBatch{Upserts: []Message{m1}}); err != nil {
		t.Fatal(err)
	}
	if m, ok := getMsg(t, s, acct, "m1"); !ok || m.FolderID != archive.ID {
		t.Errorf("after move: ok=%v folder=%d", ok, m.FolderID)
	}
}

func TestRevertRestoresAndKeepsLaterOps(t *testing.T) {
	ctx := context.Background()
	s, _, acct, inbox, archive := setup(t)
	if err := s.ApplyDelta(ctx, acct, inbox.ID, DeltaBatch{Upserts: []Message{msg("m1", "hi", time.Now())}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(ctx, acct, "m1", Mutation{Kind: OpMove, Dest: archive}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(ctx, acct, "m1", Mutation{Kind: OpSetFlag, Value: true}); err != nil {
		t.Fatal(err)
	}
	ops, _ := s.PendingOps(ctx, 10)
	if len(ops) != 2 || ops[0].Kind != OpMove {
		t.Fatalf("ops = %+v", ops)
	}
	changed, err := s.RevertOp(ctx, ops[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 2 {
		t.Errorf("changed = %v", changed)
	}
	m, _ := getMsg(t, s, acct, "m1")
	if m.FolderID != inbox.ID || !m.Flagged {
		t.Errorf("after revert: folder=%d flagged=%v, want inbox and still flagged", m.FolderID, m.Flagged)
	}
	if n, _ := s.PendingCount(ctx); n != 1 {
		t.Errorf("pending = %d, want 1 (the flag)", n)
	}
}

func TestEnqueueNoOpAndMissing(t *testing.T) {
	ctx := context.Background()
	s, _, acct, inbox, _ := setup(t)
	if err := s.ApplyDelta(ctx, acct, inbox.ID, DeltaBatch{Upserts: []Message{msg("m1", "hi", time.Now())}}); err != nil {
		t.Fatal(err)
	}
	changed, err := s.Enqueue(ctx, acct, "m1", Mutation{Kind: OpSetRead, Value: false}) // already unread
	if err != nil || len(changed) != 0 {
		t.Errorf("no-op enqueue: changed=%v err=%v", changed, err)
	}
	if n, _ := s.PendingCount(ctx); n != 0 {
		t.Errorf("no-op was queued")
	}
	if _, err := s.Enqueue(ctx, acct, "nope", Mutation{Kind: OpSetRead, Value: true}); err != ErrNotFound {
		t.Errorf("missing message: err=%v", err)
	}
}

func TestMoveToUnsyncedFolderDropsLocalCopy(t *testing.T) {
	ctx := context.Background()
	s, _, acct, inbox, archive := setup(t)
	if err := s.ApplyDelta(ctx, acct, inbox.ID, DeltaBatch{Upserts: []Message{msg("m1", "hi", time.Now())}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(ctx, acct, "m1", Mutation{Kind: OpMove, Dest: archive}); err != nil {
		t.Fatal(err)
	}
	ops, _ := s.PendingOps(ctx, 10)
	if err := s.CompleteOp(ctx, ops[0], true); err != nil {
		t.Fatal(err)
	}
	if _, ok := getMsg(t, s, acct, "m1"); ok {
		t.Error("message moved to an unsynced folder should be dropped from the cache")
	}
}
