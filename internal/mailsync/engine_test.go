package mailsync

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tom/zorp/internal/graph"
	"github.com/tom/zorp/internal/store"
)

type staticToken struct{}

func (staticToken) Token(context.Context) (string, error) { return "tok", nil }

// fakeGraph serves fixtures from testdata and records delta requests.
type fakeGraph struct {
	t   *testing.T
	srv *httptest.Server

	mu            sync.Mutex
	deltaQueries  []string
	expireTokenD2 bool // answer $deltatoken=d2 with SyncStateNotFound
}

func newFakeGraph(t *testing.T) *fakeGraph {
	f := &fakeGraph{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGraph) fixture(w http.ResponseWriter, name string) {
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		f.t.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, strings.ReplaceAll(string(data), "{{BASE}}", f.srv.URL))
}

func (f *fakeGraph) serve(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
		f.t.Errorf("%s: missing ImmutableId Prefer header", r.URL.Path)
	}
	q := r.URL.Query()
	switch r.URL.Path {
	case "/me":
		f.fixture(w, "me.json")
	case "/me/mailFolders":
		f.fixture(w, "folders.json")
	case "/me/mailFolders/AAInbox/childFolders":
		f.fixture(w, "child_folders.json")
	case "/me/mailFolders/inbox":
		io.WriteString(w, `{"id":"AAInbox","displayName":"Inbox"}`)
	case "/me/mailFolders/archive":
		io.WriteString(w, `{"id":"AAArchive","displayName":"Archive"}`)
	case "/me/mailFolders/AAInbox/messages/delta":
		if !strings.Contains(r.Header.Get("Prefer"), "odata.maxpagesize=100") {
			f.t.Errorf("delta request missing maxpagesize")
		}
		f.mu.Lock()
		f.deltaQueries = append(f.deltaQueries, r.URL.RawQuery)
		expire := f.expireTokenD2
		f.mu.Unlock()
		switch {
		case q.Get("$skiptoken") == "page2":
			f.fixture(w, "delta_initial_2.json")
		case q.Get("$deltatoken") == "d1":
			f.fixture(w, "delta_incremental.json")
		case q.Get("$deltatoken") == "d2" && expire:
			w.WriteHeader(http.StatusGone)
			f.fixture(w, "sync_state_not_found.json")
		case q.Get("$deltatoken") == "d2":
			io.WriteString(w, `{"value":[],"@odata.deltaLink":"`+f.srv.URL+`/me/mailFolders/AAInbox/messages/delta?$deltatoken=d2"}`)
		default:
			f.fixture(w, "delta_initial_1.json")
		}
	default:
		// Other well-known folders don't exist in this mailbox.
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"code":"ErrorFolderNotFound","message":"not found"}}`)
	}
}

func newTestEngine(t *testing.T, fg *fakeGraph) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "zorp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	g := graph.New(staticToken{}, graph.WithBaseURL(fg.srv.URL))
	e := New(g, st, Options{Folders: []string{"inbox"}, InitialDays: 90},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.now = func() time.Time { return time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC) }
	return e, st
}

func inboxMessages(t *testing.T, st *store.Store, folderID int64) map[string]store.Message {
	t.Helper()
	msgs, err := st.MessagesInFolder(context.Background(), folderID, 100)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.Message{}
	for _, m := range msgs {
		out[m.GraphID] = m
	}
	return out
}

func TestSyncInitialThenIncrementalThenExpired(t *testing.T) {
	ctx := context.Background()
	fg := newFakeGraph(t)
	e, st := newTestEngine(t, fg)

	// Pass 1: from scratch, two pages.
	res, err := e.Sync(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Folders != 3 {
		t.Errorf("folders = %d, want 3 (incl. child folder)", res.Folders)
	}
	if len(res.FolderResults) != 1 {
		t.Fatalf("folder results = %+v", res.FolderResults)
	}
	fr := res.FolderResults[0]
	if !fr.Initial || fr.Upserted != 3 || fr.Total != 3 || fr.Unread != 2 {
		t.Errorf("pass 1 = %+v", fr)
	}
	if !strings.Contains(fg.deltaQueries[0], "receivedDateTime+ge+2026-07-29T12") {
		t.Errorf("initial delta not limited to InitialDays: %s", fg.deltaQueries[0])
	}

	folders, err := st.Folders(ctx, e.AccountID())
	if err != nil {
		t.Fatal(err)
	}
	wellKnown := map[string]string{}
	for _, f := range folders {
		wellKnown[f.Name] = f.WellKnown
	}
	if wellKnown["Inbox"] != "inbox" || wellKnown["Archive"] != "archive" || wellKnown["Projects"] != "" {
		t.Errorf("well-known mapping = %v", wellKnown)
	}

	msgs := inboxMessages(t, st, fr.FolderID)
	m1 := msgs["m1"]
	if m1.From.Addr != "alice@example.com" || !m1.Flagged || !m1.HasAttachments || m1.Importance != "high" ||
		len(m1.To) != 1 || len(m1.Categories) != 1 {
		t.Errorf("m1 = %+v", m1)
	}
	if m3 := msgs["m3"]; m3.From.Addr != "" || m3.Subject != "System notice" {
		t.Errorf("m3 (null from) = %+v", m3)
	}

	// Pass 2: incremental from d1 — m1 read, m2 removed, m4 new.
	res, err = e.Sync(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	fr = res.FolderResults[0]
	if fr.Initial || fr.Upserted != 2 || fr.Removed != 1 || fr.Total != 3 || fr.Unread != 2 {
		t.Errorf("pass 2 = %+v", fr)
	}
	msgs = inboxMessages(t, st, fr.FolderID)
	if _, ok := msgs["m2"]; ok {
		t.Error("m2 should have been removed")
	}
	if !msgs["m1"].IsRead {
		t.Error("m1 should be read")
	}
	if _, ok := msgs["m4"]; !ok {
		t.Error("m4 should have been added")
	}

	// Pass 3: d2 has expired; the folder resyncs from scratch and m4, which
	// the fresh pass doesn't return, is pruned.
	fg.mu.Lock()
	fg.expireTokenD2 = true
	fg.mu.Unlock()
	res, err = e.Sync(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	fr = res.FolderResults[0]
	if !fr.Initial {
		t.Errorf("pass 3 should be initial after expiry: %+v", fr)
	}
	msgs = inboxMessages(t, st, fr.FolderID)
	if _, ok := msgs["m4"]; ok {
		t.Error("m4 should have been pruned by the from-scratch pass")
	}
	if len(msgs) != 3 {
		t.Errorf("after resync have %d messages, want 3", len(msgs))
	}
}

func TestResetSyncOnFreshEngine(t *testing.T) {
	ctx := context.Background()
	fg := newFakeGraph(t)
	e, st := newTestEngine(t, fg)
	if _, err := e.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}

	// A new Engine (e.g. a restarted daemon) handling `sync --full` first.
	g := graph.New(staticToken{}, graph.WithBaseURL(fg.srv.URL))
	e2 := New(g, st, e.opts, e.log)
	e2.now = e.now
	if err := e2.ResetSync(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := e2.Sync(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if fr := res.FolderResults[0]; !fr.Initial || fr.Total != 3 {
		t.Errorf("after reset = %+v", fr)
	}
}
