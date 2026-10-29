package graph

import (
	"context"
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

// Event is a calendar item, as far as zorp uses it.
type Event struct {
	ID                string           `json:"id"`
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
