package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/store"
)

func key(m *Model, code rune, mod ...tea.KeyMod) tea.Cmd {
	k := tea.KeyPressMsg{Code: code}
	for _, md := range mod {
		k.Mod |= md
	}
	_, cmd := m.Update(k)
	return cmd
}

func formScreen(m *Model) string { return ansi.Strip(strings.Join(m.formLines(120, 40), "\n")) }

func TestPlanNewMeeting(t *testing.T) {
	m := withCalendar(t)
	m.cfg.Calendar.Teams = true
	m.cfg.Contacts.Directory = true
	m.cfg.Safety.SafeMode = true
	m.contacts = []store.Contact{
		{Address: store.Address{Name: "Alice Ng", Addr: "alice@partner.org"}, Score: 3},
		{Address: store.Address{Name: "Alan Smith", Addr: "alan@example.com"}, Score: 2},
	}
	m.openAgenda()
	press(m, "n")
	f := m.planning.form
	if f == nil || m.planning.reviewing {
		t.Fatal("n should open the form")
	}
	// Today at noon: the next hour.
	if f.day != localDay(m.now()) || f.timeIn.Value() != "13:00" || f.length.Value() != "30m" || !f.teams {
		t.Fatalf("defaults: %v %q %q %v", f.day, f.timeIn.Value(), f.length.Value(), f.teams)
	}
	typeText(m, "Roadmap")

	// The day: arrows on the grid, or a typed day.
	key(m, tea.KeyTab)
	if f.focus != fDay || !strings.Contains(formScreen(m), "November 2026") {
		t.Fatalf("focus %v:\n%s", f.focus, formScreen(m))
	}
	key(m, tea.KeyRight)
	key(m, tea.KeyDown)
	if want := localDay(m.now()).AddDate(0, 0, 8); !f.day.Equal(want) {
		t.Errorf("day = %v, want %v", f.day, want)
	}
	typeText(m, "tomorrow 10:30")
	submit(m)
	if !f.day.Equal(localDay(m.now()).AddDate(0, 0, 1)) || f.timeIn.Value() != "10:30" {
		t.Errorf("typed day: %v %q", f.day, f.timeIn.Value())
	}
	key(m, tea.KeyTab) // time
	key(m, tea.KeyUp)
	key(m, tea.KeyDown)
	key(m, tea.KeyTab) // length
	key(m, tea.KeyUp)
	if f.length.Value() != "45m" {
		t.Errorf("length = %q", f.length.Value())
	}

	// People: suggestions from the cache, then from the directory.
	key(m, tea.KeyTab)
	typeText(m, "al")
	if len(f.invite.sugg) != 2 || f.invite.sugg[0].Addr != "alice@partner.org" {
		t.Fatalf("suggestions = %+v", f.invite.sugg)
	}
	m.Update(peopleFoundMsg{field: fInvite, seq: f.invite.seq, people: []suggestion{{Address: store.Address{Name: "Alana Dir", Addr: "alana@example.com"}, detail: "Sales"}, {Address: store.Address{Addr: "ALICE@partner.org"}}}})
	if len(f.invite.sugg) != 3 || f.invite.sugg[2].detail != "Sales" {
		t.Errorf("merged = %+v", f.invite.sugg)
	}
	submit(m) // the first suggestion
	typeText(m, "bob@example.com,")
	if len(f.invite.chips) != 2 || f.invite.chips[1].Addr != "bob@example.com" || f.invite.input.Value() != "" {
		t.Fatalf("chips = %+v", f.invite.chips)
	}
	key(m, tea.KeyBackspace)
	if len(f.invite.chips) != 1 {
		t.Error("backspace on an empty line should drop the last person")
	}
	typeText(m, "bob@example.com")

	key(m, 's', tea.ModCtrl)
	p := m.planning
	if !p.reviewing || len(p.problems) > 0 {
		t.Fatalf("ctrl+s should review: %q", p.problems)
	}
	screen := ansi.Strip(strings.Join(m.planLines(120, 30), "\n"))
	for _, want := range []string{"New meeting", "guardrail won't let it go out", "Tue 03 Nov 2026 10:30–11:15", "Clashes with 10:00–11:00 Review", "alice@partner.org", "checking…"} {
		if !strings.Contains(screen, want) {
			t.Errorf("review lacks %q:\n%s", want, screen)
		}
	}
	params, changed := m.planParams()
	if !changed || params.ID != "" || *params.Subject != "Roadmap" || params.Start.Hour() != 10 || params.End.Sub(*params.Start) != 45*time.Minute ||
		len(params.Attendees.Required) != 2 || params.Online == nil || !*params.Online || params.Body != nil {
		t.Errorf("params = %+v", params)
	}

	m.Update(freeBusyMsg{key: "stale"})
	m.Update(freeBusyMsg{key: p.fbKey, res: []ipc.FreeBusy{{Addr: "alice@partner.org", Busy: []ipc.Busy{{Start: *params.Start, End: params.Start.Add(15 * time.Minute), Status: "oof"}}}}})
	screen = ansi.Strip(strings.Join(m.planLines(120, 30), "\n"))
	for _, want := range []string{"✗  Alice Ng  away 10:30–10:45", "1 person is busy then"} {
		if !strings.Contains(screen, want) {
			t.Errorf("review lacks %q:\n%s", want, screen)
		}
	}

	// e goes back to the form, ctrl+s returns.
	press(m, "e")
	if p.reviewing {
		t.Error("e should go back to the form")
	}
	key(m, 's', tea.ModCtrl)
	if cmd := m.handleKey("y"); cmd == nil || p.busy != "Sending…" {
		t.Fatalf("y should send (busy %q)", p.busy)
	}
	m.Update(planDoneMsg{new: true, err: errString("guardrail: no")})
	if m.planning == nil || m.errText != "guardrail: no" {
		t.Error("a failed send should stay on the review screen")
	}
	m.Update(planDoneMsg{new: true, res: ipc.EventSaveResult{Notified: 2}})
	if m.planning != nil || m.notice != "Invited 2 people." {
		t.Errorf("done: %q", m.notice)
	}
}

func TestPlanProblems(t *testing.T) {
	m := withCalendar(t)
	m.openAgenda()
	press(m, "n")
	f := m.planning.form
	f.timeIn.SetValue("half past")
	f.invite.input.SetValue("not an address")
	key(m, 's', tea.ModCtrl)
	got := strings.Join(m.planning.problems, "; ")
	for _, want := range []string{"Subject", "Time", "Invite"} {
		if !strings.Contains(got, want) {
			t.Errorf("problems lack %s: %q", want, got)
		}
	}
	m.handleKey("y")
	if m.errText == "" || m.planning.busy != "" {
		t.Error("y with problems should refuse")
	}
	// Esc on the form asks before discarding.
	press(m, "e")
	key(m, tea.KeyEscape)
	if m.confirm == nil {
		t.Fatal("esc should ask")
	}
	deliver(m, m.handleKey("y"))
	if m.planning != nil {
		t.Error("not discarded")
	}
}

func TestPlanAllDay(t *testing.T) {
	m := withCalendar(t)
	m.openAgenda()
	press(m, "n")
	f := m.planning.form
	f.subject.SetValue("Offsite")
	f.timeIn.SetValue("")
	f.length.SetValue("2d")
	pl, problems := f.plan()
	if len(problems) > 0 || !pl.AllDay || pl.Days != 2 || !pl.End().Equal(localDay(m.now()).AddDate(0, 0, 2)) {
		t.Errorf("all day: %+v %q", pl, problems)
	}
}

func TestPlanEditSendsOnlyChanges(t *testing.T) {
	m := withCalendar(t)
	start := time.Date(2026, 11, 3, 10, 0, 0, 0, time.Local)
	ev := ipc.EventDetail{ID: "e1", Type: "singleInstance", Subject: "Review", Start: start, End: start.Add(time.Hour), IsOrganizer: true, Online: true,
		Required: []ipc.Address{{Name: "Bo", Addr: "bo@example.com"}}, BodyType: "html",
		Body: "<p>Bring numbers.</p><p>________________________________________________________________________________</p><p>Microsoft Teams meeting</p>"}
	m.Update(editReady(ev, false))
	p := m.planning
	if p == nil || p.form.desc != "Bring numbers.\n" || p.form.timeIn.Value() != "10:00" || p.form.length.Value() != "1h" {
		t.Fatalf("edit form: %+v", p)
	}
	p.form.length.SetValue("45m")
	key(m, 's', tea.ModCtrl)
	params, changed := m.planParams()
	if !changed || params.ID != "e1" || params.Subject != nil || params.Attendees != nil || params.Body != nil || params.Online != nil ||
		params.End.Sub(*params.Start) != 45*time.Minute {
		t.Errorf("params = %+v", params)
	}
	if !strings.Contains(ansi.Strip(strings.Join(m.planLines(120, 30), "\n")), "sends the update to 1 person") {
		t.Error("review should say who gets the update")
	}
	press(m, "e")
	p.form.teams = false
	key(m, 's', tea.ModCtrl)
	if len(p.problems) == 0 {
		t.Error("taking Teams off should be a problem")
	}

	// A whole series: no times on the form.
	m.endPlan()
	m.Update(editReady(ev, true))
	if m.planning.form.shown(fDay) || strings.Contains(formScreen(m), "Length") {
		t.Errorf("series form:\n%s", formScreen(m))
	}
}

func TestAgendaEditAndCancelChecks(t *testing.T) {
	m := withCalendar(t)
	m.openAgenda()
	// The cursor starts on the standup, which someone else organized.
	press(m, "e")
	if !strings.Contains(m.errText, "only the organizer") {
		t.Errorf("e: %q", m.errText)
	}
	press(m, "D")
	if !strings.Contains(m.errText, "only the organizer") {
		t.Errorf("D: %q", m.errText)
	}
}

func TestPlanFromMessage(t *testing.T) {
	m := withCalendar(t)
	msg := &m.msgs[m.msgCur]
	msg.Subject = "Re: [EXTERNAL] Fw: Budget"
	msg.From = store.Address{Name: "Ann", Addr: "ann@example.com"}
	msg.To = []store.Address{{Addr: "test@example.com"}, {Addr: "ANN@example.com"}}
	msg.Cc = []store.Address{{Name: "Cy", Addr: "cy@example.com"}}
	press(m, "M")
	f := m.planning.form
	if f.subject.Value() != "Budget" || len(f.invite.chips) != 2 || f.invite.chips[1].Name != "Cy" {
		t.Errorf("form: %q %+v", f.subject.Value(), f.invite.chips)
	}
}
