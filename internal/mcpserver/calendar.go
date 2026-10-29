package mcpserver

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/perlsaiyan/zorp/internal/agenda"
	"github.com/perlsaiyan/zorp/internal/render"
	"github.com/perlsaiyan/zorp/internal/store"
)

func (s *server) addCalendarTools(srv *mcp.Server) {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: boolPtr(false)}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_events",
		Title:       "List calendar events",
		Annotations: ro,
		Description: "The user's meetings from the cached calendar (30 days back to 180 ahead), earliest first. " +
			"Defaults to the next 7 days from now. Declined and cancelled meetings are left out unless include_declined. " +
			"Times are RFC 3339; all-day events span whole days. " + untrustedNote,
	}, s.listEvents)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_event",
		Title:       "Read a calendar event",
		Annotations: ro,
		Description: "One event from the cached calendar: time, place, organizer, attendees and their answers, and the start of its description. " + untrustedNote,
	}, s.getEvent)
}

// EventsInput is list_events' input.
type EventsInput struct {
	From            string `json:"from,omitempty" jsonschema:"start, as RFC 3339 or YYYY-MM-DD (local time); default now"`
	Days            int    `json:"days,omitempty" jsonschema:"how many days from the start (default 7, max 180)"`
	IncludeDeclined bool   `json:"include_declined,omitempty" jsonschema:"also list declined and cancelled meetings"`
}

// EventSummary is an event in list_events.
type EventSummary struct {
	ID        string `json:"id" jsonschema:"event id, for get_event"`
	Subject   string `json:"subject"`
	Start     string `json:"start"`
	End       string `json:"end"`
	AllDay    bool   `json:"all_day,omitempty"`
	Location  string `json:"location,omitempty"`
	Organizer string `json:"organizer,omitempty"`
	Response  string `json:"response,omitempty" jsonschema:"the user's answer: accepted, tentativelyAccepted, declined, notResponded, organizer, none"`
	ShowAs    string `json:"show_as,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
	Online    bool   `json:"online,omitempty" jsonschema:"has a Teams (or other) join link"`
}

type EventsOutput struct {
	Events []EventSummary `json:"events"`
	Note   string         `json:"note,omitempty"`
}

func eventSummary(e store.Event) EventSummary {
	d := e.Details
	return EventSummary{
		ID: e.GraphID, Subject: agenda.Subject(e), Start: e.Start.Format(time.RFC3339), End: e.End.Format(time.RFC3339),
		AllDay: d.AllDay, Location: clean(d.Location), Organizer: address(d.Organizer), Response: d.Response,
		ShowAs: d.ShowAs, Cancelled: d.Cancelled, Online: d.JoinURL != "",
	}
}

func (s *server) listEvents(ctx context.Context, _ *mcp.CallToolRequest, in EventsInput) (*mcp.CallToolResult, EventsOutput, error) {
	from := s.now()
	if in.From != "" {
		t, err := parseDate(in.From)
		if err != nil {
			return nil, EventsOutput{}, err
		}
		from = t
	}
	days := in.Days
	if days <= 0 {
		days = 7
	}
	evs, err := s.st.EventsBetween(ctx, s.account.ID, from, from.AddDate(0, 0, min(days, 180)))
	if err != nil {
		return nil, EventsOutput{}, err
	}
	out := EventsOutput{Events: []EventSummary{}}
	for _, e := range evs {
		if agenda.Shown(e, in.IncludeDeclined) {
			out.Events = append(out.Events, eventSummary(e))
		}
	}
	if sync, err := s.st.CalendarSync(ctx, s.account.ID); err == nil && sync.SyncedAt.IsZero() {
		out.Note = "The calendar hasn't been synced: zorp needs calendar access, and the daemon must be running."
	}
	return nil, out, nil
}

func parseDate(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, errors.New("from must be RFC 3339 or YYYY-MM-DD")
}

// EventInput is get_event's input.
type EventInput struct {
	ID string `json:"id" jsonschema:"event id from list_events"`
}

type EventAttendee struct {
	Address  string `json:"address"`
	Type     string `json:"type,omitempty"`
	Response string `json:"response,omitempty"`
}

type EventOutput struct {
	Event       EventSummary    `json:"event"`
	Attendees   []EventAttendee `json:"attendees,omitempty"`
	Description string          `json:"description,omitempty" jsonschema:"start of the description, between untrusted-content markers"`
}

func (s *server) getEvent(ctx context.Context, _ *mcp.CallToolRequest, in EventInput) (*mcp.CallToolResult, EventOutput, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return nil, EventOutput{}, errors.New("id is required")
	}
	e, err := s.st.EventByGraphID(ctx, s.account.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, EventOutput{}, errors.New("no cached event with that id (use an id from list_events)")
	}
	if err != nil {
		return nil, EventOutput{}, err
	}
	out := EventOutput{Event: eventSummary(e)}
	for _, a := range e.Details.Attendees {
		out.Attendees = append(out.Attendees, EventAttendee{Address: address(a.Address), Type: a.Type, Response: a.Response})
	}
	if p := strings.TrimSpace(e.Details.Preview); p != "" {
		out.Description = fence(render.Sanitize(p, true))
	}
	return nil, out, nil
}
