package meeting

import (
	"strings"
	"testing"
	"time"

	"github.com/perlsaiyan/zorp/internal/store"
)

// Thursday 29 Oct 2026, 15:10.
var now = time.Date(2026, 10, 29, 15, 10, 0, 0, time.UTC)

func TestParseWhen(t *testing.T) {
	at := func(mo time.Month, d, h, mi int) time.Time { return time.Date(2026, mo, d, h, mi, 0, 0, time.UTC) }
	for _, tc := range []struct {
		in     string
		want   time.Time
		allDay bool
	}{
		{"Fri 30 Oct 2026 10:00", at(10, 30, 10, 0), false},
		{"2026-10-30 14:30", at(10, 30, 14, 30), false},
		{"tomorrow 2pm", at(10, 30, 14, 0), false},
		{"tomorrow at 2 pm", at(10, 30, 14, 0), false},
		{"mon 9:30", at(11, 2, 9, 30), false},
		{"thu 16:00", at(10, 29, 16, 0), false}, // later today
		{"thu 9:00", at(11, 5, 9, 0), false},    // passed: next week
		{"Oct 30 12am", at(10, 30, 0, 0), false},
		{"3rd Nov", at(11, 3, 0, 0), true},
		{"30 Oct", at(10, 30, 0, 0), true},
		{"1 Jan", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), true}, // the next one
		{"16:30", at(10, 29, 16, 30), false},
		{"fri", at(10, 30, 0, 0), true},
	} {
		got, allDay, err := ParseWhen(tc.in, now)
		if err != nil || !got.Equal(tc.want) || allDay != tc.allDay {
			t.Errorf("%q = %v %v %v, want %v %v", tc.in, got, allDay, err, tc.want, tc.allDay)
		}
	}
	for in, want := range map[string]string{
		"Mon 30 Oct 2026 10:00": "is a Friday",
		"30 Feb":                "doesn't have",
		"25:00 tomorrow":        "isn't a time",
		"next week":             "don't understand",
		"30":                    "a day and a month",
		"":                      "give a day",
		"tomorrow 30 Oct":       "one way",
	} {
		if _, _, err := ParseWhen(in, now); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", in, err, want)
		}
	}
}

func TestParseLength(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"30m": 30 * time.Minute, "1h": time.Hour, "1h30": 90 * time.Minute, "1 hour 15 min": 75 * time.Minute,
		"45": 45 * time.Minute, "1.5h": 90 * time.Minute,
	} {
		if d, _, err := ParseLength(in, false); err != nil || d != want {
			t.Errorf("%q = %v %v", in, d, err)
		}
	}
	if _, days, err := ParseLength("2 days", true); err != nil || days != 2 {
		t.Errorf("2 days = %d %v", days, err)
	}
	for _, bad := range []string{"2d", "0m", "26h", "soon"} {
		if _, _, err := ParseLength(bad, false); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	if _, _, err := ParseLength("1h", true); err == nil {
		t.Error("hours on an all-day meeting should fail")
	}
}

func TestFileRoundTrip(t *testing.T) {
	p := Plan{
		Subject: "Roadmap review", Start: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC), Length: 45 * time.Minute,
		Invite:   []store.Address{{Name: "Alice Example", Addr: "alice@example.com"}, {Addr: "bob@example.com"}},
		Optional: []store.Address{{Addr: "carol@example.com"}}, Where: "Room 4", Teams: true,
		Description: "Let's settle the Q1 plan.\n",
	}
	text := File(p, FileOptions{})
	for _, want := range []string{"When: Fri 30 Oct 2026 10:00\n", "Length: 45m\n", "Teams: yes\n", "Invite: Alice Example <alice@example.com>, bob@example.com\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("file lacks %q:\n%s", want, text)
		}
	}
	got, err := Parse(text, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != p.Subject || !got.Start.Equal(p.Start) || got.Length != p.Length || got.Attendees() != 3 || got.Where != p.Where || !got.Teams || got.Description != p.Description {
		t.Errorf("round trip: %+v", got)
	}
	if !got.End().Equal(p.Start.Add(45 * time.Minute)) {
		t.Errorf("end = %v", got.End())
	}

	// An all-day meeting, with the length left out.
	got, err = Parse("Subject: Offsite\nWhen: 3 Nov\nLength:\n", now)
	if err != nil || !got.AllDay || got.Days != 1 || !got.End().Equal(time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("all day: %+v %v", got, err)
	}
	if _, err := Parse("Subject: x\nWhen: someday\nColour: red\n", now); err == nil || !strings.Contains(err.Error(), "When:") || !strings.Contains(err.Error(), "Colour") {
		t.Errorf("err = %v", err)
	}
	if f := File(p, FileOptions{Editing: true, NoTime: true}); strings.Contains(f, "When:") || !strings.Contains(f, "recurring") {
		t.Errorf("series file:\n%s", f)
	}
}
