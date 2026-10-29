// Package agenda formats the cached calendar for `zorp agenda`: as text,
// as JSON, and as a Waybar custom module.
//
// Subjects and places come from other people. They're stripped of
// control characters, and escaped for Pango markup in Waybar output.
package agenda

import (
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/perlsaiyan/zorp/internal/notify"
	"github.com/perlsaiyan/zorp/internal/render"
	"github.com/perlsaiyan/zorp/internal/store"
)

// Shown reports whether an event belongs in the agenda: declined and
// cancelled meetings are left out unless all is set.
func Shown(e store.Event, all bool) bool {
	return all || (!e.Details.Cancelled && e.Details.Response != "declined")
}

// Subject is an event's subject for display, without gateway tags.
func Subject(e store.Event) string {
	s := strings.Join(strings.Fields(render.Sanitize(e.Details.Subject, false)), " ")
	s, _ = notify.Untag(s)
	if s == "" {
		return "(no subject)"
	}
	return s
}

func clean(s string) string { return strings.Join(strings.Fields(render.Sanitize(s, false)), " ") }

// When is an event's time on its day, e.g. "14:00–14:30" or "all day".
func When(e store.Event) string {
	if e.Details.AllDay {
		return "all day"
	}
	return e.Start.Local().Format("15:04") + "–" + e.End.Local().Format("15:04")
}

// Status is the user's answer, when it's worth showing.
func Status(e store.Event) string {
	switch {
	case e.Details.Cancelled:
		return "cancelled"
	case e.Details.Response == "declined":
		return "declined"
	case e.Details.Response == "tentativelyAccepted":
		return "tentative"
	case e.Details.Response == "notResponded" || (e.Details.Response == "none" && !e.Details.IsOrganizer):
		return "not answered"
	}
	return ""
}

// Text lists events by day.
func Text(events []store.Event, now time.Time) string {
	if len(events) == 0 {
		return "Nothing on the calendar.\n"
	}
	var b strings.Builder
	day := ""
	for _, e := range events {
		start := e.Start.Local()
		if start.Before(now) {
			start = now // ongoing, or all-day since earlier
		}
		if d := dayName(start, now); d != day {
			if day != "" {
				b.WriteString("\n")
			}
			day = d
			b.WriteString(d + "\n")
		}
		line := fmt.Sprintf("  %-11s  %s", When(e), Subject(e))
		var extra []string
		if loc := clean(e.Details.Location); loc != "" && !strings.HasPrefix(loc, "http") {
			extra = append(extra, loc)
		}
		if e.Details.JoinURL != "" || strings.HasPrefix(e.Details.Location, "http") {
			extra = append(extra, "online")
		}
		if s := Status(e); s != "" {
			extra = append(extra, s)
		}
		if len(extra) > 0 {
			line += "  (" + strings.Join(extra, ", ") + ")"
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func dayName(t, now time.Time) string {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, now.Location()); {
	case d.Equal(today):
		return "Today, " + t.Format("Mon 02 Jan")
	case d.Equal(today.AddDate(0, 0, 1)):
		return "Tomorrow, " + t.Format("Mon 02 Jan")
	}
	return t.Format("Mon 02 Jan")
}

// Item is an event in `zorp agenda --json`.
type Item struct {
	ID        string    `json:"id"`
	Subject   string    `json:"subject"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	AllDay    bool      `json:"all_day"`
	Location  string    `json:"location,omitempty"`
	JoinURL   string    `json:"join_url,omitempty"`
	Organizer string    `json:"organizer,omitempty"`
	Response  string    `json:"response,omitempty"`
	ShowAs    string    `json:"show_as,omitempty"`
	Cancelled bool      `json:"cancelled,omitempty"`
}

// Items converts events for JSON output.
func Items(events []store.Event) []Item {
	out := make([]Item, 0, len(events))
	for _, e := range events {
		out = append(out, Item{
			ID: e.GraphID, Subject: Subject(e), Start: e.Start, End: e.End, AllDay: e.Details.AllDay,
			Location: clean(e.Details.Location), JoinURL: e.Details.JoinURL, Organizer: clean(e.Details.Organizer.Addr),
			Response: e.Details.Response, ShowAs: e.Details.ShowAs, Cancelled: e.Details.Cancelled,
		})
	}
	return out
}

// WaybarOutput is a Waybar custom module's JSON ("return-type": "json").
// Text is empty when nothing is coming up today, which hides the module.
type WaybarOutput struct {
	Text    string `json:"text"`
	Tooltip string `json:"tooltip"`
	// Class is "now" (in a meeting), "soon" (one starts within 15
	// minutes), "later" (one is coming today) or "none".
	Class string `json:"class"`
	Alt   string `json:"alt"`
}

// Soon is how near a meeting must be for the "soon" class.
const Soon = 15 * time.Minute

// Waybar sums up the rest of today: the meeting going on, else the next
// one. events should cover at least today; all-day events only appear
// in the tooltip.
func Waybar(events []store.Event, now time.Time, stale bool) WaybarOutput {
	esc := html.EscapeString
	endOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
	out := WaybarOutput{Class: "none", Alt: "none"}
	var tip []string
	var current, next *store.Event
	for i := range events {
		e := &events[i]
		if !e.End.After(now) || !e.Start.Before(endOfDay) || !e.Details.Busy() {
			continue
		}
		tip = append(tip, esc(When(*e)+"  "+Subject(*e)))
		if e.Details.AllDay {
			continue
		}
		switch {
		case !e.Start.After(now) && current == nil:
			current = e
		case e.Start.After(now) && next == nil:
			next = e
		}
	}
	switch {
	case current != nil:
		out.Class = "now"
		out.Text = esc(short(Subject(*current), 40)) + " until " + current.End.Local().Format("15:04")
	case next != nil:
		out.Class = "later"
		wait := next.Start.Sub(now)
		if wait <= Soon {
			out.Class = "soon"
		}
		if wait < time.Hour {
			out.Text = esc(short(Subject(*next), 40)) + " in " + fmt.Sprintf("%dm", int(wait.Round(time.Minute)/time.Minute))
		} else {
			out.Text = esc(short(Subject(*next), 40)) + " at " + next.Start.Local().Format("15:04")
		}
	}
	out.Alt = out.Class
	if len(tip) == 0 {
		tip = append(tip, "Nothing else today")
	}
	if stale {
		tip = append(tip, "", "(the calendar hasn't synced lately: is the zorp daemon running?)")
	}
	out.Tooltip = strings.Join(tip, "\n")
	return out
}

func short(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
