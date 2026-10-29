// Package meeting holds a planned meeting and reads the ways its time
// and length are typed: days ("Fri 30 Oct 2026", "tomorrow", "mon"),
// clock times ("10:00", "2pm") and lengths ("30m", "1h30", "2d").
package meeting

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/perlsaiyan/zorp/internal/store"
)

// Plan is the content of a meeting file.
type Plan struct {
	Subject     string
	Start       time.Time     // local time; midnight for an all-day meeting. Zero if When was left out.
	AllDay      bool          //
	Length      time.Duration // a timed meeting's
	Days        int           // an all-day meeting's
	Invite      []store.Address
	Optional    []store.Address
	Where       string
	Teams       bool
	Description string
}

// End is when the meeting ends: for an all-day one, midnight after its
// last day.
func (p Plan) End() time.Time {
	if p.AllDay {
		return p.Start.AddDate(0, 0, p.Days)
	}
	return p.Start.Add(p.Length)
}

// Attendees is how many people are invited.
func (p Plan) Attendees() int { return len(p.Invite) + len(p.Optional) }

// When formats a meeting's start.
func When(start time.Time, allDay bool) string {
	if allDay {
		return start.Format("Mon 02 Jan 2006")
	}
	return start.Format("Mon 02 Jan 2006 15:04")
}

// FormatLength formats a meeting's length, as ParseLength reads it.
func FormatLength(p Plan) string {
	if p.AllDay {
		return strconv.Itoa(max(p.Days, 1)) + "d"
	}
	d := p.Length.Round(time.Minute)
	h, mins := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case h == 0:
		return strconv.Itoa(mins) + "m"
	case mins == 0:
		return strconv.Itoa(h) + "h"
	}
	return fmt.Sprintf("%dh%dm", h, mins)
}

var (
	months = map[string]time.Month{
		"jan": 1, "january": 1, "feb": 2, "february": 2, "mar": 3, "march": 3, "apr": 4, "april": 4,
		"may": 5, "jun": 6, "june": 6, "jul": 7, "july": 7, "aug": 8, "august": 8,
		"sep": 9, "sept": 9, "september": 9, "oct": 10, "october": 10, "nov": 11, "november": 11, "dec": 12, "december": 12,
	}
	weekdays = map[string]time.Weekday{
		"sun": 0, "sunday": 0, "mon": 1, "monday": 1, "tue": 2, "tues": 2, "tuesday": 2,
		"wed": 3, "wednesday": 3, "thu": 4, "thur": 4, "thurs": 4, "thursday": 4,
		"fri": 5, "friday": 5, "sat": 6, "saturday": 6,
	}
	isoDate   = regexp.MustCompile(`^(\d{4})-(\d{1,2})-(\d{1,2})$`)
	clockTime = regexp.MustCompile(`^(\d{1,2})(?:[:.](\d{2}))?(am|pm)?$`)
	hoursMins = regexp.MustCompile(`^\d+h\d+$`)
)

// ParseWhen reads a When line in now's location: a day, and a time unless
// it's all day.
func ParseWhen(s string, now time.Time) (time.Time, bool, error) {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == ' ' || r == ',' || r == '\t' })
	// "2 pm" is one token.
	var toks []string
	for _, f := range fields {
		if (f == "am" || f == "pm") && len(toks) > 0 {
			toks[len(toks)-1] += f
			continue
		}
		toks = append(toks, f)
	}
	loc := now.Location()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	var (
		date         time.Time
		wd           = time.Weekday(-1)
		month        time.Month
		day, year    int
		hour, minute = -1, 0
		haveDate     bool
	)
	for _, t := range toks {
		if m := isoDate.FindStringSubmatch(t); m != nil {
			y, _ := strconv.Atoi(m[1])
			mo, _ := strconv.Atoi(m[2])
			d, _ := strconv.Atoi(m[3])
			if mo < 1 || mo > 12 || d < 1 || d > 31 {
				return time.Time{}, false, fmt.Errorf("%q isn't a date", t)
			}
			date, haveDate = time.Date(y, time.Month(mo), d, 0, 0, 0, 0, loc), true
			continue
		}
		if mo, ok := months[strings.TrimSuffix(t, ".")]; ok {
			month = mo
			continue
		}
		if w, ok := weekdays[strings.TrimSuffix(t, ".")]; ok {
			wd = w
			continue
		}
		switch t {
		case "today":
			date, haveDate = today, true
			continue
		case "tomorrow":
			date, haveDate = today.AddDate(0, 0, 1), true
			continue
		case "at", "on", "next":
			continue
		}
		if m := clockTime.FindStringSubmatch(t); m != nil && (m[2] != "" || m[3] != "") {
			h, _ := strconv.Atoi(m[1])
			mi, _ := strconv.Atoi(m[2])
			switch m[3] {
			case "am", "pm":
				if h < 1 || h > 12 {
					return time.Time{}, false, fmt.Errorf("%q isn't a time", t)
				}
				h %= 12
				if m[3] == "pm" {
					h += 12
				}
			}
			if h > 23 || mi > 59 {
				return time.Time{}, false, fmt.Errorf("%q isn't a time", t)
			}
			hour, minute = h, mi
			continue
		}
		if n, err := strconv.Atoi(strings.TrimRight(t, "stndrh")); err == nil {
			switch {
			case len(t) == 4 && n >= 2000:
				year = n
			case n >= 1 && n <= 31:
				day = n
			default:
				return time.Time{}, false, fmt.Errorf("%q isn't a day or a time", t)
			}
			continue
		}
		return time.Time{}, false, fmt.Errorf("don't understand %q", t)
	}

	explicit := month != 0 || day != 0 || year != 0
	switch {
	case haveDate && explicit:
		return time.Time{}, false, errors.New("give the day one way, e.g. \"30 Oct\" or \"tomorrow\"")
	case haveDate:
	case month != 0 && day != 0:
		y := year
		if y == 0 {
			y = now.Year()
		}
		date = time.Date(y, month, day, 0, 0, 0, 0, loc)
		if date.Month() != month {
			return time.Time{}, false, fmt.Errorf("%s doesn't have a %d", month, day)
		}
		if year == 0 && date.Before(today) {
			date = date.AddDate(1, 0, 0)
		}
		haveDate = true
	case explicit:
		return time.Time{}, false, errors.New("give a day and a month, e.g. \"30 Oct\"")
	}
	switch {
	case wd >= 0 && haveDate:
		if date.Weekday() != wd {
			return time.Time{}, false, fmt.Errorf("%s is a %s", date.Format("02 Jan 2006"), date.Weekday())
		}
	case wd >= 0:
		// The next one: today if it hasn't passed yet.
		date = today.AddDate(0, 0, (int(wd)-int(today.Weekday())+7)%7)
		if date.Equal(today) && (hour < 0 || !time.Date(date.Year(), date.Month(), date.Day(), hour, minute, 0, 0, loc).After(now)) {
			date = date.AddDate(0, 0, 7)
		}
		haveDate = true
	case !haveDate && hour >= 0:
		date, haveDate = today, true
	}
	if !haveDate {
		return time.Time{}, false, errors.New("give a day, e.g. \"Fri 30 Oct 2026 10:00\" or \"tomorrow 2pm\"")
	}
	if hour < 0 {
		return date, true, nil
	}
	return time.Date(date.Year(), date.Month(), date.Day(), hour, minute, 0, 0, loc), false, nil
}

// ParseClock reads a time of day: "10:00", "9", "2pm", "2:30 pm",
// "14.30" or "1430".
func ParseClock(s string) (hour, minute int, err error) {
	v := strings.ToLower(strings.Join(strings.Fields(s), ""))
	if len(v) >= 3 && len(v) <= 4 && strings.Trim(v, "0123456789") == "" {
		v = v[:len(v)-2] + ":" + v[len(v)-2:]
	}
	m := clockTime.FindStringSubmatch(v)
	if m == nil {
		return 0, 0, fmt.Errorf("%q isn't a time, e.g. 10:00 or 2pm", s)
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	if m[3] != "" {
		if h < 1 || h > 12 {
			return 0, 0, fmt.Errorf("%q isn't a time", s)
		}
		h %= 12
		if m[3] == "pm" {
			h += 12
		}
	}
	if h > 23 || mi > 59 {
		return 0, 0, fmt.Errorf("%q isn't a time", s)
	}
	return h, mi, nil
}

// ParseLength reads a meeting's length: a duration for a timed meeting, days
// for an all-day one.
func ParseLength(s string, allDay bool) (time.Duration, int, error) {
	v := strings.ToLower(strings.Join(strings.Fields(s), ""))
	for _, r := range []struct{ long, short string }{
		{"minutes", "m"}, {"minute", "m"}, {"mins", "m"}, {"min", "m"},
		{"hours", "h"}, {"hour", "h"}, {"hrs", "h"}, {"hr", "h"}, {"days", "d"}, {"day", "d"},
	} {
		v = strings.ReplaceAll(v, r.long, r.short)
	}
	if n, ok := strings.CutSuffix(v, "d"); ok {
		days, err := strconv.Atoi(n)
		switch {
		case err != nil || days < 1:
			return 0, 0, fmt.Errorf("%q isn't a number of days", s)
		case !allDay:
			return 0, 0, errors.New("days are for all-day meetings; give a time in When, or use hours")
		case days > 31:
			return 0, 0, errors.New("that's over a month")
		}
		return 0, days, nil
	}
	if allDay {
		return 0, 0, errors.New("an all-day meeting's length is in days, e.g. 1d (or give a time in When)")
	}
	if hoursMins.MatchString(v) {
		v += "m"
	}
	if _, err := strconv.Atoi(v); err == nil {
		v += "m" // a bare number is minutes
	}
	d, err := time.ParseDuration(v)
	switch {
	case err != nil || d <= 0:
		return 0, 0, fmt.Errorf("%q isn't a length, e.g. 30m or 1h30m", s)
	case d < 5*time.Minute:
		return 0, 0, errors.New("that's under five minutes")
	case d > 24*time.Hour:
		return 0, 0, errors.New("that's over a day; make it all-day (a day alone in When)")
	}
	return d, 0, nil
}
