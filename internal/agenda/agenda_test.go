package agenda

import (
	"strings"
	"testing"
	"time"

	"github.com/perlsaiyan/zorp/internal/store"
)

func ev(subject string, start time.Time, mins int, d store.EventDetails) store.Event {
	d.Subject = subject
	return store.Event{GraphID: subject, Start: start, End: start.Add(time.Duration(mins) * time.Minute), Details: d}
}

func TestWaybar(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.Local)
	busy := store.EventDetails{Response: "accepted", ShowAs: "busy"}
	day := []store.Event{
		ev("Holiday", time.Date(2026, 11, 2, 0, 0, 0, 0, time.Local), 24*60, store.EventDetails{AllDay: true}),
		ev("[EXTERNAL]Standup <b>", now.Add(-10*time.Minute), 30, busy),
		ev("Lunch", now.Add(2*time.Hour), 60, store.EventDetails{ShowAs: "free"}),
		ev("Review", now.Add(3*time.Hour), 60, busy),
	}
	out := Waybar(day, now, false)
	if out.Class != "now" || out.Text != "Standup &lt;b&gt; until 10:20" {
		t.Errorf("during a meeting: %+v", out)
	}
	if !strings.Contains(out.Tooltip, "all day  Holiday") || strings.Contains(out.Tooltip, "Lunch") {
		t.Errorf("tooltip = %q (all-day shown, free time left out)", out.Tooltip)
	}
	out = Waybar(day, now.Add(25*time.Minute), false)
	if out.Class != "later" || out.Text != "Review at 13:00" {
		t.Errorf("later: %+v", out)
	}
	out = Waybar(day, now.Add(2*time.Hour+50*time.Minute), true)
	if out.Class != "soon" || out.Text != "Review in 10m" || !strings.Contains(out.Tooltip, "daemon") {
		t.Errorf("soon, stale: %+v", out)
	}
	out = Waybar(day, now.Add(5*time.Hour), false)
	if out.Class != "none" || out.Text != "" {
		t.Errorf("after the last meeting: %+v", out)
	}
}

func TestText(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.Local)
	text := Text([]store.Event{
		ev("Standup", now.Add(time.Hour), 15, store.EventDetails{Response: "notResponded", JoinURL: "https://x"}),
		ev("Offsite", now.Add(24*time.Hour), 60, store.EventDetails{Location: "Room 4", Response: "tentativelyAccepted"}),
		ev("Board\x1b[2J", now.Add(72*time.Hour), 60, store.EventDetails{Response: "accepted"}),
	}, now)
	for _, want := range []string{"Today, Mon 02 Nov", "11:00–11:15  Standup  (online, not answered)", "Tomorrow, Tue 03 Nov", "(Room 4, tentative)", "Thu 05 Nov"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\x1b") {
		t.Error("escape sequence not stripped")
	}
	if Shown(ev("No", now, 30, store.EventDetails{Response: "declined"}), false) {
		t.Error("declined shown")
	}
}
