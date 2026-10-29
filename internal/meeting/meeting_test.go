package meeting

import (
	"strings"
	"testing"
	"time"
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

func TestParseClock(t *testing.T) {
	for in, want := range map[string][2]int{"10:00": {10, 0}, "9": {9, 0}, "2pm": {14, 0}, "2:30 pm": {14, 30}, "12am": {0, 0}, "14.30": {14, 30}, "1430": {14, 30}, "930": {9, 30}} {
		if h, mi, err := ParseClock(in); err != nil || h != want[0] || mi != want[1] {
			t.Errorf("%q = %d:%d %v", in, h, mi, err)
		}
	}
	for _, bad := range []string{"25:00", "13pm", "noon", "", "10:75"} {
		if _, _, err := ParseClock(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}
