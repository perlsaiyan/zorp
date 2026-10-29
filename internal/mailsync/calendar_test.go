package mailsync

import (
	"context"
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

// fakeCalendar serves a calendarView delta: a two-page first pass, then
// one change per incremental pass.
type fakeCalendar struct {
	mu       sync.Mutex
	srv      *httptest.Server
	requests []string
	expire   bool // answer delta tokens with 410 Gone
}

func (f *fakeCalendar) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	if r.URL.Path == "/me" {
		io.WriteString(w, `{"displayName":"Me","userPrincipalName":"me@example.com"}`)
		return
	}
	switch {
	case q.Get("startDateTime") != "":
		f.requests = append(f.requests, "initial")
		io.WriteString(w, `{"value":[
		 {"id":"s","type":"seriesMaster","subject":"Standup","start":{"dateTime":"2026-10-01T15:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-10-01T15:15:00","timeZone":"UTC"},"responseStatus":{"response":"accepted"},"showAs":"busy"},
		 {"id":"o1","type":"occurrence","seriesMasterId":"s","start":{"dateTime":"2026-11-02T15:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-11-02T15:15:00","timeZone":"UTC"}}
		],"@odata.nextLink":"`+f.srv.URL+`/me/calendarView/delta?$skiptoken=p2"}`)
	case q.Get("$skiptoken") == "p2":
		f.requests = append(f.requests, "page2")
		io.WriteString(w, `{"value":[
		 {"id":"x","type":"singleInstance","subject":"Review","start":{"dateTime":"2026-11-02T16:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-11-02T17:00:00","timeZone":"UTC"},"onlineMeeting":{"joinUrl":"https://teams.example/x"},"organizer":{"emailAddress":{"name":"Al","address":"al@example.com"}}}
		],"@odata.deltaLink":"`+f.srv.URL+`/me/calendarView/delta?$deltatoken=d1"}`)
	case q.Get("$deltatoken") != "":
		f.requests = append(f.requests, "delta "+q.Get("$deltatoken"))
		if f.expire {
			w.WriteHeader(http.StatusGone)
			io.WriteString(w, `{"error":{"code":"SyncStateNotFound","message":"gone"}}`)
			return
		}
		io.WriteString(w, `{"value":[{"id":"x","@removed":{"reason":"deleted"}}],"@odata.deltaLink":"`+f.srv.URL+`/me/calendarView/delta?$deltatoken=d2"}`)
	default:
		http.NotFound(w, r)
	}
}

func TestSyncCalendar(t *testing.T) {
	ctx := context.Background()
	f := &fakeCalendar{}
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "zorp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	g := graph.New(staticToken{}, graph.WithBaseURL(f.srv.URL))
	e := New(g, st, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }

	res, err := e.SyncCalendar(ctx, g)
	if err != nil || !res.Initial || res.Upserted != 3 {
		t.Fatalf("first pass: %+v, %v", res, err)
	}
	acct, _ := e.Account(ctx)
	day := [2]time.Time{now, now.Add(24 * time.Hour)}
	evs, _ := st.EventsBetween(ctx, acct, day[0], day[1])
	if len(evs) != 2 || evs[0].Details.Subject != "Standup" || evs[1].Details.JoinURL != "https://teams.example/x" || evs[1].Details.Organizer.Addr != "al@example.com" {
		t.Fatalf("events = %+v", evs)
	}

	res, err = e.SyncCalendar(ctx, g)
	if err != nil || res.Initial || res.Removed != 1 {
		t.Fatalf("incremental: %+v, %v", res, err)
	}
	if evs, _ := st.EventsBetween(ctx, acct, day[0], day[1]); len(evs) != 1 {
		t.Errorf("after removal: %d events", len(evs))
	}

	// A new day moves the window: a fresh pass.
	now = now.Add(24 * time.Hour)
	if res, err := e.SyncCalendar(ctx, g); err != nil || !res.Initial {
		t.Fatalf("next day: %+v, %v", res, err)
	}
	// An expired link starts over too.
	f.expire = true
	if res, err := e.SyncCalendar(ctx, g); err != nil || !res.Initial {
		t.Fatalf("expired: %+v, %v", res, err)
	}
	want := "initial page2 delta d1 initial page2 delta d1 initial page2"
	if got := strings.Join(f.requests, " "); got != want {
		t.Errorf("requests:\n got %s\nwant %s", got, want)
	}
}
