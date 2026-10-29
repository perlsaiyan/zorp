package tui

import (
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/store"
)

// startPlan runs a command that prepares a meeting file, as the editor
// would see it.
func startPlan(t *testing.T, m *Model, cmd func() tea.Msg) string {
	t.Helper()
	m.cfg.Paths.ComposeDir = t.TempDir()
	msg := cmd()
	if msg == nil {
		t.Fatalf("nothing to plan: %q", m.errText)
	}
	m.Update(msg)
	if m.planning == nil {
		t.Fatalf("no meeting file: %q", m.errText)
	}
	data, err := os.ReadFile(m.planning.path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writePlan(t *testing.T, m *Model, text string) {
	t.Helper()
	if err := os.WriteFile(m.planning.path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	m.planEditorDone(nil)
}

func TestPlanNewMeeting(t *testing.T) {
	m := withCalendar(t)
	m.cfg.Calendar.Teams = true
	m.cfg.Safety.SafeMode = true
	m.openAgenda()
	text := startPlan(t, m, func() tea.Msg { return m.handleKey("n")() })
	// Today at noon: the next hour.
	if !strings.Contains(text, "When: Mon 02 Nov 2026 13:00\n") || !strings.Contains(text, "Length: 30m\n") || !strings.Contains(text, "Teams: yes\n") {
		t.Fatalf("template:\n%s", text)
	}

	// Nothing written: discarded.
	m.planEditorDone(nil)
	if m.planning != nil || !strings.Contains(m.notice, "discarded") {
		t.Errorf("unchanged: %q", m.notice)
	}

	text = startPlan(t, m, func() tea.Msg { return m.handleKey("n")() })
	text = strings.Replace(text, "Subject: \n", "Subject: Roadmap\n", 1)
	text = strings.Replace(text, "When: Mon 02 Nov 2026 13:00", "When: tomorrow 10:30", 1)
	text = strings.Replace(text, "Invite: \n", "Invite: Alice <alice@partner.org>, test@example.com\n", 1)
	writePlan(t, m, text+"Agenda to follow.\n")
	p := m.planning
	if len(p.problems) > 0 {
		t.Fatalf("problems: %q", p.problems)
	}
	screen := ansi.Strip(strings.Join(m.planLines(120, 30), "\n"))
	for _, want := range []string{"New meeting", "guardrail won't let it go out", "Tue 03 Nov 2026 10:30–11:00", "Clashes with 10:00–11:00 Review", "Guardrail", "alice@partner.org", "Agenda to follow."} {
		if !strings.Contains(screen, want) {
			t.Errorf("review lacks %q:\n%s", want, screen)
		}
	}
	params, changed := m.planParams()
	if !changed || params.ID != "" || *params.Subject != "Roadmap" || params.Start.Hour() != 10 || params.End.Sub(*params.Start) != 30*time.Minute ||
		len(params.Attendees.Required) != 2 || params.Online == nil || !*params.Online || *params.Body != "Agenda to follow.\n" {
		t.Errorf("params = %+v", params)
	}

	if cmd := m.handleKey("y"); cmd == nil || p.busy != "Sending…" {
		t.Fatalf("y should send (busy %q)", p.busy)
	}
	m.Update(planDoneMsg{new: true, err: errString("guardrail: no")})
	if m.planning == nil || m.errText != "guardrail: no" {
		t.Error("a failed send should stay on the review screen")
	}
	m.Update(planDoneMsg{new: true, res: ipc.EventSaveResult{Notified: 1}})
	if m.planning != nil || m.notice != "Invited 1 person." {
		t.Errorf("done: %q", m.notice)
	}
}

func TestPlanProblems(t *testing.T) {
	m := withCalendar(t)
	m.openAgenda()
	text := startPlan(t, m, func() tea.Msg { return m.handleKey("n")() })
	writePlan(t, m, strings.Replace(text, "When: Mon 02 Nov 2026 13:00", "When: someday", 1))
	if got := strings.Join(m.planning.problems, "; "); !strings.Contains(got, "Subject") || !strings.Contains(got, "When") {
		t.Errorf("problems = %q", got)
	}
	m.handleKey("y")
	if m.errText == "" || m.planning.busy != "" {
		t.Error("y with problems should refuse")
	}
}

func TestPlanEditSendsOnlyChanges(t *testing.T) {
	m := withCalendar(t)
	m.cfg.Paths.ComposeDir = t.TempDir()
	start := time.Date(2026, 11, 3, 10, 0, 0, 0, time.Local)
	ev := ipc.EventDetail{ID: "e1", Type: "singleInstance", Subject: "Review", Start: start, End: start.Add(time.Hour), IsOrganizer: true, Online: true,
		Required: []ipc.Address{{Name: "Bo", Addr: "bo@example.com"}}, BodyType: "html",
		Body: "<p>Bring numbers.</p><p>________________________________________________________________________________</p><p>Microsoft Teams meeting</p>"}
	m.planReady(editReady(ev, false))
	text, _ := os.ReadFile(m.planning.path)
	if !strings.Contains(string(text), "Bring numbers.") || strings.Contains(string(text), "Teams meeting\n") {
		t.Fatalf("edit template:\n%s", text)
	}
	writePlan(t, m, strings.Replace(string(text), "Length: 1h", "Length: 45m", 1))
	params, changed := m.planParams()
	if !changed || params.ID != "e1" || params.Subject != nil || params.Attendees != nil || params.Body != nil || params.Online != nil ||
		params.End.Sub(*params.Start) != 45*time.Minute {
		t.Errorf("params = %+v", params)
	}
	if !strings.Contains(ansi.Strip(strings.Join(m.planLines(120, 30), "\n")), "sends the update to 1 person") {
		t.Error("review should say who gets the update")
	}
	writePlan(t, m, strings.Replace(string(text), "\nTeams: yes\n", "\nTeams: no\n", 1))
	if len(m.planning.problems) == 0 {
		t.Error("taking Teams off should be a problem")
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
	text := startPlan(t, m, func() tea.Msg { return m.handleKey("M")() })
	for _, want := range []string{"Subject: Budget\n", "Invite: Ann <ann@example.com>, Cy <cy@example.com>\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("template lacks %q:\n%s", want, text)
		}
	}
}
