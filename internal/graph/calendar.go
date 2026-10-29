package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DateTimeTimeZone is Graph's local time plus zone name.
type DateTimeTimeZone struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

// Time parses it. Calendar calls ask for UTC (preferUTC), and meeting
// messages come in UTC, so other zones only appear as a fallback, where
// an IANA name is tried and anything else is read as UTC.
func (d *DateTimeTimeZone) Time() time.Time {
	if d == nil || d.DateTime == "" {
		return time.Time{}
	}
	loc := time.UTC
	if z := d.TimeZone; z != "" && !strings.EqualFold(z, "UTC") {
		if l, err := time.LoadLocation(z); err == nil {
			loc = l
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05.9999999", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, d.DateTime, loc); err == nil {
			return t
		}
	}
	return time.Time{}
}

const preferUTC = `outlook.timezone="UTC"`

// Location is an event's place.
type Location struct {
	DisplayName string `json:"displayName"`
}

// ResponseStatus is an attendee's (or the user's) answer: "none",
// "organizer", "tentativelyAccepted", "accepted", "declined" or
// "notResponded".
type ResponseStatus struct {
	Response string `json:"response"`
}

// Attendee is someone invited to an event.
type Attendee struct {
	EmailAddress EmailAddress   `json:"emailAddress"`
	Type         string         `json:"type"` // required, optional, resource
	Status       ResponseStatus `json:"status"`
}

// Event is a calendar item, as far as zorp uses it.
type Event struct {
	ID                string           `json:"id"`
	Type              string           `json:"type"` // singleInstance, occurrence, exception, seriesMaster
	SeriesMasterID    string           `json:"seriesMasterId"`
	ICalUID           string           `json:"iCalUId"`
	ShowAs            string           `json:"showAs"` // free, tentative, busy, oof, workingElsewhere
	BodyPreview       string           `json:"bodyPreview"`
	WebLink           string           `json:"webLink"`
	Attendees         []Attendee       `json:"attendees"`
	Subject           string           `json:"subject"`
	Start             DateTimeTimeZone `json:"start"`
	End               DateTimeTimeZone `json:"end"`
	IsAllDay          bool             `json:"isAllDay"`
	IsCancelled       bool             `json:"isCancelled"`
	IsOrganizer       bool             `json:"isOrganizer"`
	ResponseRequested bool             `json:"responseRequested"`
	ResponseStatus    ResponseStatus   `json:"responseStatus"`
	Location          Location         `json:"location"`
	Organizer         *Recipient       `json:"organizer"`
	OnlineMeeting     *struct {
		JoinURL string `json:"joinUrl"`
	} `json:"onlineMeeting"`
	IsOnlineMeeting bool      `json:"isOnlineMeeting"`
	Body            *ItemBody `json:"body"` // only when asked for (EventForEdit)

	// Removed is set on delta items for events that left the window.
	Removed *struct {
		Reason string `json:"reason"`
	} `json:"@removed"`
}

// JoinURL is the Teams (or other online meeting) link, if any.
func (e *Event) JoinURL() string {
	if e.OnlineMeeting == nil {
		return ""
	}
	return e.OnlineMeeting.JoinURL
}

const eventSelect = "id,subject,start,end,isAllDay,isCancelled,isOrganizer,responseRequested,responseStatus,location,organizer,onlineMeeting"

// TodayEventCount counts today's events (local time): a calendar access
// check for `zorp probe`.
func (c *Client) TodayEventCount(ctx context.Context) (int, error) {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var page struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	q := url.Values{
		"startDateTime": {start.UTC().Format(time.RFC3339)},
		"endDateTime":   {start.AddDate(0, 0, 1).UTC().Format(time.RFC3339)},
		"$select":       {"id"},
		"$top":          {"100"},
	}
	err := c.get(ctx, "/me/calendarView?"+q.Encode(), &page, preferUTC)
	return len(page.Value), err
}

// EventPage is one page of a calendarView delta pass. Occurrences of a
// series come with only their id, type, times and seriesMasterId; the
// rest is the series master's.
type EventPage struct {
	Events    []Event
	Removed   []string
	NextLink  string
	DeltaLink string
}

// CalendarDeltaURL starts a delta pass over the calendar between start and
// end. Its delta links only ever cover that window.
func (c *Client) CalendarDeltaURL(start, end time.Time) string {
	q := url.Values{
		"startDateTime": {start.UTC().Format(time.RFC3339)},
		"endDateTime":   {end.UTC().Format(time.RFC3339)},
	}
	return c.baseURL + "/me/calendarView/delta?" + q.Encode()
}

// EventsDelta fetches one page of a calendar delta pass.
func (c *Client) EventsDelta(ctx context.Context, link string) (*EventPage, error) {
	var raw struct {
		Value     []json.RawMessage `json:"value"`
		NextLink  string            `json:"@odata.nextLink"`
		DeltaLink string            `json:"@odata.deltaLink"`
	}
	if err := c.get(ctx, link, &raw, "odata.maxpagesize=100", preferUTC); err != nil {
		return nil, err
	}
	page := &EventPage{NextLink: raw.NextLink, DeltaLink: raw.DeltaLink}
	for _, item := range raw.Value {
		var e Event
		if err := json.Unmarshal(item, &e); err != nil {
			return nil, fmt.Errorf("graph: calendar delta item: %w", err)
		}
		if e.Removed != nil {
			page.Removed = append(page.Removed, e.ID)
		} else {
			page.Events = append(page.Events, e)
		}
	}
	if page.NextLink == "" && page.DeltaLink == "" {
		return nil, errors.New("graph: calendar delta page has neither nextLink nor deltaLink")
	}
	return page, nil
}

// MessageEvent returns the calendar event a meeting message is about, or
// nil if it has none (any more), e.g. a cancellation already removed.
func (c *Client) MessageEvent(ctx context.Context, messageID string) (*Event, error) {
	var m struct {
		Event *Event `json:"event"`
	}
	path := "/me/messages/" + url.PathEscape(messageID) + "?$select=id&$expand=microsoft.graph.eventMessage/event($select=" + eventSelect + ")"
	if err := c.get(ctx, path, &m, preferUTC); err != nil {
		return nil, err
	}
	return m.Event, nil
}

// GetEvent fetches one event.
func (c *Client) GetEvent(ctx context.Context, id string) (*Event, error) {
	var e Event
	if err := c.get(ctx, "/me/events/"+url.PathEscape(id)+"?$select="+eventSelect, &e, preferUTC); err != nil {
		return nil, err
	}
	return &e, nil
}

// Answers to a meeting request.
const (
	Accept    = "accept"
	Tentative = "tentativelyAccept"
	Decline   = "decline"
)

// RespondToEvent answers an invite. With send, the organizer gets the
// answer (and comment) by mail.
func (c *Client) RespondToEvent(ctx context.Context, eventID, answer, comment string, send bool) error {
	switch answer {
	case Accept, Tentative, Decline:
	default:
		return fmt.Errorf("unknown answer %q", answer)
	}
	body := map[string]any{"sendResponse": send}
	if send && comment != "" {
		body["comment"] = comment
	}
	return c.postOnce(ctx, "/me/events/"+url.PathEscape(eventID)+"/"+answer, body, nil)
}

// DeleteEvent removes an event from the user's calendar. For an event
// someone else organized and has cancelled, this sends nothing.
func (c *Client) DeleteEvent(ctx context.Context, eventID string) error {
	return c.do(ctx, http.MethodDelete, "/me/events/"+url.PathEscape(eventID), nil, nil)
}

// EventForEdit fetches an event with everything the meeting file shows,
// its description included.
func (c *Client) EventForEdit(ctx context.Context, id string) (*Event, error) {
	var e Event
	sel := eventSelect + ",type,seriesMasterId,attendees,isOnlineMeeting,body"
	if err := c.get(ctx, "/me/events/"+url.PathEscape(id)+"?$select="+sel, &e, preferUTC); err != nil {
		return nil, err
	}
	return &e, nil
}

// EventChange is what to set on an event. Nil fields are left as they are.
type EventChange struct {
	Subject    *string
	Start, End *time.Time
	AllDay     *bool
	// Zone is the IANA time zone Start and End are given in (and the
	// meeting is shown in); UTC if empty. All-day events start and end at
	// midnight in it.
	Zone      string
	Location  *string
	Attendees *[]Attendee // everyone invited, the whole list
	Online    *bool       // add a Teams meeting
	Body      *string
	BodyHTML  bool
}

func (ch EventChange) payload() map[string]any {
	out := map[string]any{}
	zone := ch.Zone
	if zone == "" {
		zone = "UTC"
	}
	at := func(t time.Time) DateTimeTimeZone {
		if loc, err := time.LoadLocation(zone); err == nil {
			t = t.In(loc)
		}
		return DateTimeTimeZone{DateTime: t.Format("2006-01-02T15:04:05"), TimeZone: zone}
	}
	if ch.Subject != nil {
		out["subject"] = *ch.Subject
	}
	if ch.Start != nil {
		out["start"] = at(*ch.Start)
	}
	if ch.End != nil {
		out["end"] = at(*ch.End)
	}
	if ch.AllDay != nil {
		out["isAllDay"] = *ch.AllDay
	}
	if ch.Location != nil {
		out["location"] = Location{DisplayName: *ch.Location}
	}
	if ch.Attendees != nil {
		as := make([]map[string]any, 0, len(*ch.Attendees))
		for _, a := range *ch.Attendees {
			as = append(as, map[string]any{"emailAddress": a.EmailAddress, "type": a.Type})
		}
		out["attendees"] = as
	}
	if ch.Online != nil {
		out["isOnlineMeeting"] = *ch.Online
		if *ch.Online {
			out["onlineMeetingProvider"] = "teamsForBusiness"
		}
	}
	if ch.Body != nil {
		ct := "text"
		if ch.BodyHTML {
			ct = "html"
		}
		out["body"] = ItemBody{ContentType: ct, Content: *ch.Body}
	}
	return out
}

// CreateEvent puts a new event on the user's calendar. If it has
// attendees, Exchange sends them the invitation. Never retried after an
// ambiguous failure, so nobody gets two invites.
func (c *Client) CreateEvent(ctx context.Context, ch EventChange) (*Event, error) {
	var e Event
	if err := c.postOnce(ctx, "/me/events", ch.payload(), &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// UpdateEvent changes an event the user organized. Exchange sends the
// attendees an update (and a cancellation to anyone taken off it).
func (c *Client) UpdateEvent(ctx context.Context, id string, ch EventChange) error {
	return c.doRetry(ctx, false, http.MethodPatch, "/me/events/"+url.PathEscape(id), ch.payload(), nil)
}

// CancelEvent cancels a meeting the user organized: the attendees get a
// cancellation, with comment, and it leaves the calendar.
func (c *Client) CancelEvent(ctx context.Context, id, comment string) error {
	body := map[string]any{}
	if comment != "" {
		body["comment"] = comment
	}
	return c.postOnce(ctx, "/me/events/"+url.PathEscape(id)+"/cancel", body, nil)
}
