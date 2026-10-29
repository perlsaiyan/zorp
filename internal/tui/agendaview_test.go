package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/store"
)

func withCalendar(t *testing.T) *Model {
	t.Helper()
	m := newTestModel(t)
	m.cfg.Calendar = config.Calendar{Enabled: true, AfterRespond: "archive"}
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.Local)
	m.clock = func() time.Time { return now }
	at := func(day, hour int) time.Time { return time.Date(2026, 11, 2+day, hour, 0, 0, 0, time.Local) }
	accepted := store.EventDetails{Response: "accepted", ShowAs: "busy", ResponseRequested: true}
	m.st.ApplyEvents(context.Background(), m.account.ID, true, []store.Event{
		{GraphID: "past", Type: "singleInstance", Start: at(0, 9), End: at(0, 10), Details: store.EventDetails{Subject: "Breakfast", Response: "accepted"}},
		{GraphID: "series", Type: "seriesMaster", Start: at(-30, 15), End: at(-30, 16), Details: store.EventDetails{Subject: "Standup", Response: "notResponded", ShowAs: "tentative", ResponseRequested: true}},
		{GraphID: "occ1", Type: "occurrence", SeriesMasterID: "series", Start: at(0, 15), End: at(0, 16)},
		{GraphID: "occ2", Type: "occurrence", SeriesMasterID: "series", Start: at(1, 15), End: at(1, 16)},
		{GraphID: "teams", Type: "singleInstance", Start: at(1, 10), End: at(1, 11), Details: func() store.EventDetails {
			d := accepted
			d.Subject, d.JoinURL = "Review", "https://teams.example/j"
			d.Attendees = []store.Attendee{{Address: store.Address{Name: "Bo", Addr: "bo@example.com"}, Response: "declined"}}
			return d
		}()},
		{GraphID: "later", Type: "singleInstance", Start: at(9, 10), End: at(9, 11), Details: store.EventDetails{Subject: "Next week", Response: "accepted"}},
	}, nil, "", [2]time.Time{at(-30, 0), at(180, 0)}, now)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	return m
}

func TestAgendaView(t *testing.T) {
	m := withCalendar(t)
	press(m, "g", "c")
	if m.agenda == nil {
		t.Fatal("g c should open the calendar")
	}
	e, _ := m.agenda.current()
	if e.GraphID != "occ1" {
		t.Errorf("starts at %s, want the first meeting not over (occ1)", e.GraphID)
	}
	view := m.View().Content
	for _, want := range []string{"Calendar  Mon 02 Nov", "Today, Mon 02 Nov", "Tomorrow, Tue 03 Nov", "15:00–16:00  Standup", "Next week", "not answered yet"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q", want)
		}
	}
	for i, l := range strings.Split(view, "\n") {
		if w := ansi.StringWidth(l); w != 120 {
			t.Errorf("line %d is %d wide", i, w)
		}
	}

	// I on an occurrence answers the whole series.
	press(m, "I")
	if m.picker == nil || m.picker.eventID != "series" || !strings.Contains(m.picker.title, "whole series") {
		t.Fatalf("picker = %+v", m.picker)
	}
	press(m, "esc", "j", "j")
	if e, _ := m.agenda.current(); e.GraphID != "occ2" {
		t.Errorf("after j j: %s", e.GraphID)
	}
	press(m, "k")
	if !strings.Contains(m.View().Content, "Bo <bo@example.com>") {
		t.Error("details should list attendees")
	}
	if cmd := m.agendaKey("O"); cmd == nil {
		t.Error("O should open the Teams link")
	}
	press(m, "k", "k", "O")
	if m.errText == "" {
		t.Error("O without a link should say so")
	}

	press(m, "]")
	if !m.agenda.from.Equal(time.Date(2026, 11, 9, 0, 0, 0, 0, time.Local)) || len(m.agenda.events) != 1 {
		t.Errorf("next week: from %v, %d events", m.agenda.from, len(m.agenda.events))
	}
	press(m, "t")
	if e, _ := m.agenda.current(); e.GraphID != "occ1" {
		t.Errorf("t: %s", e.GraphID)
	}
	press(m, "q")
	if m.agenda != nil {
		t.Error("q should leave the calendar")
	}

	// Narrow terminals show just the list, and still fit.
	press(m, "g", "c")
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	for i, l := range strings.Split(m.View().Content, "\n") {
		if w := ansi.StringWidth(l); w != 60 {
			t.Errorf("narrow line %d is %d wide", i, w)
		}
	}
	press(m, "g", "i")
	if m.agenda != nil {
		t.Error("g i should go to the Inbox")
	}
}
