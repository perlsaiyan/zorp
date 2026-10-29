package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/ipc"
)

// eventsGraph fakes creating, fetching, changing and cancelling events.
type eventsGraph struct {
	calendarGraph
	mu    sync.Mutex
	event string // what GET /me/events/{id} returns
	calls []string
}

func (g *eventsGraph) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/me" || strings.HasPrefix(r.URL.Path, "/me/messages/") {
		g.calendarGraph.ServeHTTP(w, r)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	g.calls = append(g.calls, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+string(body)))
	switch {
	case r.URL.Path == "/me/people":
		io.WriteString(w, `{"value":[{"displayName":"Alice Ng","jobTitle":"Engineer","department":"Platform","scoredEmailAddresses":[{"address":"alice@example.com"}]},{"displayName":"No Mail","scoredEmailAddresses":[]}]}`)
	case r.URL.Path == "/users":
		io.WriteString(w, `{"value":[{"displayName":"Alice Ng","mail":"Alice@example.com"},{"displayName":"Alan Oak","mail":"alan@example.com"},{"displayName":"Room 4"}]}`)
	case r.URL.Path == "/me/calendar/getSchedule":
		io.WriteString(w, `{"value":[
			{"scheduleId":"alice@example.com","scheduleItems":[
				{"status":"busy","start":{"dateTime":"2026-11-03T13:00:00.0000000","timeZone":"UTC"},"end":{"dateTime":"2026-11-03T13:30:00.0000000","timeZone":"UTC"}},
				{"status":"free","start":{"dateTime":"2026-11-03T13:30:00","timeZone":"UTC"},"end":{"dateTime":"2026-11-03T14:00:00","timeZone":"UTC"}}]},
			{"scheduleId":"dana@partner.org","error":{"message":"not shared","responseCode":"ErrorAccessDenied"}}]}`)
	case r.Method == http.MethodPost && r.URL.Path == "/me/events":
		io.WriteString(w, `{"id":"new1","onlineMeeting":{"joinUrl":"https://teams.example/new"}}`)
	case r.Method == http.MethodGet:
		io.WriteString(w, g.event)
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

func (g *eventsGraph) last() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := g.calls[len(g.calls)-1]
	_, body, _ := strings.Cut(c, "{")
	var out map[string]any
	json.Unmarshal([]byte("{"+body), &out)
	return out
}

func call[T any](t *testing.T, f func(context.Context, json.RawMessage) (T, error), p any) (T, error) {
	t.Helper()
	b, _ := json.Marshal(p)
	return f(context.Background(), b)
}

func ptr[T any](v T) *T { return &v }

func TestCreateMeeting(t *testing.T) {
	t.Setenv("TZ", "Europe/Oslo")
	g := &eventsGraph{}
	cfg := &config.Config{Calendar: config.Calendar{Enabled: true}, Safety: config.Safety{SafeMode: true, SendAllow: []string{"example.com"}}}
	d := newMeetingDaemon(t, cfg, g)
	start := time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC)
	p := ipc.EventSaveParams{
		Subject: ptr("Roadmap"), Start: &start, End: ptr(start.Add(45 * time.Minute)), AllDay: ptr(false),
		Location:  ptr("Room 4"),
		Attendees: &ipc.Attendees{Required: []ipc.Address{{Name: "Alice", Addr: "alice@example.com"}}, Optional: []ipc.Address{{Addr: "tom@example.com"}}},
		Online:    ptr(true), Body: ptr("Let's **plan**."),
	}
	res, err := call(t, d.eventSave, p)
	if err != nil || res.ID != "new1" || res.Notified != 1 || res.JoinURL == "" {
		t.Fatalf("create: %+v, %v", res, err)
	}
	got := g.last()
	if s := got["start"].(map[string]any); s["dateTime"] != "2026-11-03T14:00:00" || s["timeZone"] != "Europe/Oslo" {
		t.Errorf("start = %v", s)
	}
	if got["isOnlineMeeting"] != true || got["onlineMeetingProvider"] != "teamsForBusiness" {
		t.Errorf("teams: %v", got)
	}
	if b := got["body"].(map[string]any); b["contentType"] != "html" || !strings.Contains(b["content"].(string), "<strong>plan</strong>") {
		t.Errorf("body = %v", b)
	}
	if as := got["attendees"].([]any); len(as) != 2 || as[1].(map[string]any)["type"] != "optional" {
		t.Errorf("attendees = %v", as)
	}

	// The guardrail covers invites.
	n := len(g.calls)
	p.Attendees.Required = append(p.Attendees.Required, ipc.Address{Addr: "dana@partner.org"})
	if _, err := call(t, d.eventSave, p); err == nil || !strings.Contains(err.Error(), "dana@partner.org") {
		t.Errorf("guardrail: %v", err)
	}
	if len(g.calls) != n {
		t.Error("blocked invite still reached Graph")
	}
	if _, err := call(t, d.eventSave, ipc.EventSaveParams{Subject: ptr("x")}); err == nil {
		t.Error("a meeting with no time")
	}
}

func TestChangeAndCancelMeeting(t *testing.T) {
	g := &eventsGraph{event: `{"id":"e9","type":"singleInstance","isOrganizer":true,
		"attendees":[{"emailAddress":{"address":"alice@example.com"},"type":"required"}],"isOnlineMeeting":true}`}
	cfg := &config.Config{Calendar: config.Calendar{Enabled: true}, Safety: config.Safety{SafeMode: true, SendAllow: []string{"example.com"}}}
	d := newMeetingDaemon(t, cfg, g)

	// Taking Alice off and adding Bob tells both.
	res, err := call(t, d.eventSave, ipc.EventSaveParams{ID: "e9", Subject: ptr("Renamed"), Attendees: &ipc.Attendees{Required: []ipc.Address{{Addr: "bob@example.com"}}}})
	if err != nil || res.Notified != 2 {
		t.Fatalf("change: %+v, %v", res, err)
	}
	if c := g.calls[len(g.calls)-1]; !strings.HasPrefix(c, "PATCH /me/events/e9 ") || strings.Contains(c, `"start"`) {
		t.Errorf("patch = %q", c)
	}
	if _, err := call(t, d.eventSave, ipc.EventSaveParams{ID: "e9", Online: ptr(false)}); err == nil {
		t.Error("took Teams off")
	}

	if res, err := call(t, d.eventCancel, ipc.EventCancelParams{ID: "e9", Comment: "sorry"}); err != nil || res.Notified != 1 {
		t.Fatalf("cancel: %+v, %v", res, err)
	}
	if c := g.calls[len(g.calls)-1]; c != `POST /me/events/e9/cancel {"comment":"sorry"}` {
		t.Errorf("cancel call = %q", c)
	}

	// With nobody else invited, cancelling just deletes it.
	g.event = `{"id":"e9","isOrganizer":true,"attendees":[]}`
	if res, err := call(t, d.eventCancel, ipc.EventCancelParams{ID: "e9"}); err != nil || res.Notified != 0 {
		t.Fatalf("delete: %+v, %v", res, err)
	}
	if c := g.calls[len(g.calls)-1]; c != "DELETE /me/events/e9" {
		t.Errorf("delete call = %q", c)
	}

	// Someone else's meeting can't be changed or cancelled.
	g.event = `{"id":"e9","isOrganizer":false}`
	if _, err := call(t, d.eventSave, ipc.EventSaveParams{ID: "e9", Subject: ptr("mine now")}); err == nil {
		t.Error("changed someone else's meeting")
	}
	if _, err := call(t, d.eventCancel, ipc.EventCancelParams{ID: "e9"}); err == nil || !strings.Contains(err.Error(), "decline") {
		t.Errorf("cancel someone else's: %v", err)
	}
}

func TestFreeBusy(t *testing.T) {
	g := &eventsGraph{}
	d := newMeetingDaemon(t, &config.Config{Calendar: config.Calendar{Enabled: true}}, g)
	start := time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC)
	res, err := call(t, d.freeBusy, ipc.FreeBusyParams{Addrs: []string{"alice@example.com", "dana@partner.org"}, Start: start, End: start.Add(time.Hour)})
	if err != nil || len(res) != 2 {
		t.Fatalf("free-busy: %+v, %v", res, err)
	}
	if b := res[0].Busy; len(b) != 1 || b[0].Status != "busy" || !b[0].Start.Equal(start) || !b[0].End.Equal(start.Add(30*time.Minute)) {
		t.Errorf("alice = %+v", res[0])
	}
	if res[1].Error != "not shared" {
		t.Errorf("dana = %+v", res[1])
	}
	if _, err := call(t, d.freeBusy, ipc.FreeBusyParams{Start: start, End: start}); err == nil {
		t.Error("empty request")
	}
}

func TestPeople(t *testing.T) {
	g := &eventsGraph{}
	d := newMeetingDaemon(t, &config.Config{Contacts: config.Contacts{Directory: true}}, g)
	res, err := call(t, d.people, ipc.PeopleParams{Query: "al"})
	if err != nil || len(res) != 2 || res[0].Title != "Engineer, Platform" || res[1].Addr != "alan@example.com" {
		t.Fatalf("people = %+v, %v", res, err)
	}
	if res, _ := call(t, d.people, ipc.PeopleParams{Query: "a"}); len(res) != 0 {
		t.Error("one letter shouldn't look anyone up")
	}
	d.cfg.Contacts.Directory = false
	if _, err := call(t, d.people, ipc.PeopleParams{Query: "al"}); err == nil {
		t.Error("directory off")
	}
}
