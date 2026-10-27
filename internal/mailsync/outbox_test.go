package mailsync

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/perlsaiyan/zorp/internal/graph"
	"github.com/perlsaiyan/zorp/internal/store"
)

// mutationServer fakes Graph's PATCH and move endpoints. Messages whose ID
// starts with "gone" return 404; "flaky" returns 503 (transient).
type mutationServer struct {
	mu    sync.Mutex
	calls []string
}

func (s *mutationServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	body, _ := io.ReadAll(r.Body)
	s.calls = append(s.calls, r.Method+" "+r.URL.Path+" "+string(body))
	s.mu.Unlock()
	id := strings.Split(strings.TrimPrefix(r.URL.Path, "/me/messages/"), "/")[0]
	switch {
	case strings.HasPrefix(id, "gone"):
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"code":"ErrorItemNotFound","message":"The specified object was not found in the store."}}`)
	case strings.HasPrefix(id, "flaky"):
		w.WriteHeader(http.StatusServiceUnavailable)
	case strings.HasSuffix(r.URL.Path, "/move"):
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"id": id})
	default:
		io.WriteString(w, `{}`)
	}
}

func TestFlushOutbox(t *testing.T) {
	ctx := context.Background()
	ms := &mutationServer{}
	srv := httptest.NewServer(ms)
	t.Cleanup(srv.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "zorp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	acct, _ := st.UpsertAccount(ctx, "test@example.com", "Test")
	st.ReplaceFolders(ctx, acct, []store.Folder{
		{GraphID: "AAInbox", Name: "Inbox", WellKnown: "inbox"},
		{GraphID: "AAScratch", Name: "zorp-test"}, // not synced
	})
	folders, _ := st.Folders(ctx, acct)
	var inbox, scratch store.Folder
	for _, f := range folders {
		if f.WellKnown == "inbox" {
			inbox = f
		} else {
			scratch = f
		}
	}
	st.SetDeltaLink(ctx, inbox.ID, "x", time.Now())
	var msgs []store.Message
	for _, id := range []string{"m1", "m2", "gone1"} {
		msgs = append(msgs, store.Message{GraphID: id, ReceivedAt: time.Now()})
	}
	st.ApplyDelta(ctx, acct, inbox.ID, store.DeltaBatch{Upserts: msgs})

	e := New(graph.New(staticToken{}, graph.WithBaseURL(srv.URL)), st, Options{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.accountID = acct

	st.Enqueue(ctx, acct, "m1", store.Mutation{Kind: store.OpSetRead, Value: true})
	st.Enqueue(ctx, acct, "m2", store.Mutation{Kind: store.OpMove, Dest: scratch})
	st.Enqueue(ctx, acct, "gone1", store.Mutation{Kind: store.OpSetFlag, Value: true})

	res, err := e.FlushOutbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sent != 2 || len(res.Reverted) != 1 || res.Reverted[0].Op.GraphID != "gone1" {
		t.Fatalf("flush = %+v", res)
	}
	want := []string{
		`PATCH /me/messages/m1 {"isRead":true}`,
		`POST /me/messages/m2/move {"destinationId":"AAScratch"}`,
		`PATCH /me/messages/gone1 {"flag":{"flagStatus":"flagged"}}`,
	}
	if strings.Join(ms.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(ms.calls, "\n"), strings.Join(want, "\n"))
	}
	inInbox, _ := st.MessagesInFolder(ctx, inbox.ID, 10)
	byID := map[string]store.Message{}
	for _, m := range inInbox {
		byID[m.GraphID] = m
	}
	if !byID["m1"].IsRead {
		t.Error("m1 should stay read")
	}
	if _, ok := byID["m2"]; ok {
		t.Error("m2 should have left the inbox")
	}
	if inScratch, _ := st.MessagesInFolder(ctx, scratch.ID, 10); len(inScratch) != 0 {
		t.Error("m2 moved to an unsynced folder should be dropped from the cache")
	}
	if byID["gone1"].Flagged {
		t.Error("rejected flag should have been reverted")
	}
	if n, _ := st.PendingCount(ctx); n != 0 {
		t.Errorf("pending = %d", n)
	}
}

func TestFlushStopsOnTransientErrorPreservingOrder(t *testing.T) {
	ctx := context.Background()
	ms := &mutationServer{}
	srv := httptest.NewServer(ms)
	t.Cleanup(srv.Close)
	st, _ := store.Open(filepath.Join(t.TempDir(), "zorp.db"))
	t.Cleanup(func() { st.Close() })
	acct, _ := st.UpsertAccount(ctx, "test@example.com", "Test")
	st.ReplaceFolders(ctx, acct, []store.Folder{{GraphID: "AAInbox", Name: "Inbox", WellKnown: "inbox"}})
	folders, _ := st.Folders(ctx, acct)
	st.ApplyDelta(ctx, acct, folders[0].ID, store.DeltaBatch{Upserts: []store.Message{
		{GraphID: "flaky1", ReceivedAt: time.Now()}, {GraphID: "m1", ReceivedAt: time.Now()},
	}})
	e := New(graph.New(staticToken{}, graph.WithBaseURL(srv.URL)), st, Options{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.accountID = acct
	st.Enqueue(ctx, acct, "flaky1", store.Mutation{Kind: store.OpSetRead, Value: true})
	st.Enqueue(ctx, acct, "m1", store.Mutation{Kind: store.OpSetRead, Value: true})

	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond) // cut the client's 5xx backoff short
	defer cancel()
	if _, err := e.FlushOutbox(ctx); err == nil {
		t.Fatal("expected a transient error")
	}
	ops, _ := st.PendingOps(context.Background(), 10)
	if len(ops) != 2 || ops[0].GraphID != "flaky1" {
		t.Fatalf("queue = %+v; both ops should remain, in order", ops)
	}
	for _, c := range ms.calls {
		if strings.Contains(c, "/m1") {
			t.Error("later op was sent before the failing one")
		}
	}
}
