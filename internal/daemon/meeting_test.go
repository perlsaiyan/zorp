package daemon

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

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/graph"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/mailsync"
	"github.com/perlsaiyan/zorp/internal/store"
)

type token struct{}

func (token) Token(context.Context) (string, error) { return "tok", nil }

// calendarGraph fakes the Graph calls behind invites: each message's
// event, answers and deletes.
type calendarGraph struct {
	mu     sync.Mutex
	events map[string]string // message ID -> event JSON ("" for none)
	calls  []string          // "POST /me/events/e1/accept {body}"
}

func (g *calendarGraph) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case r.URL.Path == "/me":
		io.WriteString(w, `{"displayName":"Tom","userPrincipalName":"tom@example.com"}`)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/me/messages/"):
		ev := g.events[strings.TrimPrefix(r.URL.Path, "/me/messages/")]
		if ev == "" {
			ev = "null"
		}
		io.WriteString(w, `{"id":"x","event":`+ev+`}`)
	case strings.HasPrefix(r.URL.Path, "/me/events/"):
		body, _ := io.ReadAll(r.Body)
		g.calls = append(g.calls, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+string(body)))
		w.WriteHeader(http.StatusAccepted)
	default:
		http.NotFound(w, r)
	}
}

func newMeetingDaemon(t *testing.T, cfg *config.Config, g http.Handler) *daemon {
	t.Helper()
	ctx := context.Background()
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "zorp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	acct, _ := st.UpsertAccount(ctx, "tom@example.com", "Tom")
	st.ReplaceFolders(ctx, acct, []store.Folder{
		{GraphID: "inbox", Name: "Inbox", WellKnown: "inbox"},
		{GraphID: "archive", Name: "Archive", WellKnown: "archive"},
	})
	inbox, _ := st.FolderByWellKnown(ctx, acct, "inbox")
	at := time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)
	meeting := func(kind string) *store.Meeting {
		return &store.Meeting{Kind: kind, Start: at, End: at.Add(time.Hour), ResponseRequested: true}
	}
	st.ApplyDelta(ctx, acct, inbox.ID, store.DeltaBatch{Upserts: []store.Message{
		{GraphID: "invite", Subject: "Planning", From: store.Address{Addr: "boss@example.com"}, ReceivedAt: at, Meeting: meeting(store.MeetingRequest)},
		{GraphID: "cancel", Subject: "Canceled: Planning", ReceivedAt: at, Meeting: meeting(store.MeetingCancelled)},
		{GraphID: "plain", Subject: "Lunch?", ReceivedAt: at},
	}})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opt := graph.WithBaseURL(srv.URL)
	return &daemon{
		cfg:    cfg,
		store:  st,
		engine: mailsync.New(graph.New(token{}, opt), st, mailsync.Options{}, log),
		cal:    graph.New(token{}, opt),
		server: ipc.NewServer(nil, log),
		log:    log,
	}
}

func respondTo(d *daemon, id, answer string, send bool) (ipc.RespondResult, error) {
	params, _ := json.Marshal(ipc.RespondParams{GraphID: id, Answer: answer, Comment: "see you there", Send: send})
	return d.respond(context.Background(), params)
}

const organizerEvent = `{"id":"e1","isCancelled":false,"isOrganizer":false,"responseRequested":true,
	"responseStatus":{"response":"none"},"organizer":{"emailAddress":{"address":"boss@other.org"}},
	"onlineMeeting":{"joinUrl":"https://teams.example/join"}}`

func TestRespondToInvite(t *testing.T) {
	g := &calendarGraph{events: map[string]string{"invite": organizerEvent, "cancel": `{"id":"e2","isCancelled":true}`}}
	cfg := &config.Config{Calendar: config.Calendar{Enabled: true, AfterRespond: "archive"}}
	d := newMeetingDaemon(t, cfg, g)
	ctx := context.Background()

	info, err := d.meetingInfo(ctx, json.RawMessage(`{"graph_id":"invite"}`))
	if err != nil || !info.Found || info.Response != "none" || info.JoinURL != "https://teams.example/join" {
		t.Fatalf("meeting info = %+v, %v", info, err)
	}
	if _, err := d.meetingInfo(ctx, json.RawMessage(`{"graph_id":"plain"}`)); err == nil {
		t.Error("plain mail has no meeting")
	}

	res, err := respondTo(d, "invite", ipc.AnswerAccept, true)
	if err != nil || res.After != "archive" {
		t.Fatalf("accept: %+v, %v", res, err)
	}
	if len(g.calls) != 1 || g.calls[0] != `POST /me/events/e1/accept {"comment":"see you there","sendResponse":true}` {
		t.Errorf("calls = %q", g.calls)
	}
	acct, _ := d.engine.Account(ctx)
	if m, _ := d.store.MessageByGraphID(ctx, acct, "invite"); m.FolderID == 0 || d.mustFolder(t, m.FolderID) != "Archive" {
		t.Errorf("invite not archived: folder %d", m.FolderID)
	}

	// Answering a cancelled meeting is refused; removing it deletes the event.
	if _, err := respondTo(d, "cancel", ipc.AnswerAccept, false); err == nil {
		t.Error("accepted a cancelled meeting")
	}
	if _, err := respondTo(d, "cancel", ipc.AnswerRemove, false); err != nil {
		t.Fatal(err)
	}
	if last := g.calls[len(g.calls)-1]; last != "DELETE /me/events/e2" {
		t.Errorf("remove: %q", last)
	}
}

func TestRespondGuardrailsAndQuietAnswers(t *testing.T) {
	g := &calendarGraph{events: map[string]string{"invite": organizerEvent}}
	cfg := &config.Config{
		Calendar: config.Calendar{Enabled: true, AfterRespond: "keep"},
		Safety:   config.Safety{SafeMode: true},
	}
	d := newMeetingDaemon(t, cfg, g)

	// Telling an organizer outside send_allow is a send the guardrail stops.
	if _, err := respondTo(d, "invite", ipc.AnswerDecline, true); err == nil || !strings.Contains(err.Error(), "guardrail") {
		t.Fatalf("decline with send: %v", err)
	}
	// Answering without telling anyone only changes the user's calendar.
	res, err := respondTo(d, "invite", ipc.AnswerDecline, false)
	if err != nil || res.After != "" {
		t.Fatalf("quiet decline: %+v, %v", res, err)
	}
	if len(g.calls) != 1 || g.calls[0] != `POST /me/events/e1/decline {"sendResponse":false}` {
		t.Errorf("calls = %q", g.calls)
	}

	cfg.Calendar.Enabled = false
	if _, err := respondTo(d, "invite", ipc.AnswerAccept, false); err == nil {
		t.Error("answered with calendar.enabled = false")
	}
}

func (d *daemon) mustFolder(t *testing.T, id int64) string {
	t.Helper()
	f, err := d.store.Folder(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return f.Name
}
