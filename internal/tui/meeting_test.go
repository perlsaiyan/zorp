package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/store"
)

// withInvite makes the cursor's message a meeting invite.
func withInvite(t *testing.T, kind string) (*Model, string) {
	t.Helper()
	m := newTestModel(t)
	m.cfg.Calendar = config.Calendar{Enabled: true, AfterRespond: "archive"}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	start := time.Date(2026, 11, 3, 14, 0, 0, 0, time.Local)
	m.msgs[m.msgCur].Meeting = &store.Meeting{Kind: kind, Start: start, End: start.Add(time.Hour), Location: "Room 4", ResponseRequested: true}
	m.bodyID = 0
	m.selectMessage()
	return m, m.msgs[m.msgCur].GraphID
}

func TestInviteHeaderAndAnswer(t *testing.T) {
	m, id := withInvite(t, store.MeetingRequest)
	content := m.previewContent(100)
	for _, want := range []string{"Meeting:", "Tue 03 Nov 2026 14:00–15:00", "Where:", "Room 4", "checking your calendar"} {
		if !strings.Contains(content, want) {
			t.Errorf("preview lacks %q", want)
		}
	}
	m.Update(meetingLoadedMsg{seq: m.bodySeq, id: id, info: ipc.MeetingInfo{Found: true, Response: "notResponded", JoinURL: "https://teams.example/j"}})
	content = m.previewContent(100)
	if !strings.Contains(content, "not answered yet") || !strings.Contains(content, "https://teams.example/j") {
		t.Errorf("after loading:\n%s", content)
	}

	press(m, "I")
	if m.picker == nil || m.picker.kind != pickAnswer || len(m.picker.matches) != 6 {
		t.Fatalf("I should open the answer picker: %+v", m.picker)
	}
	// Accept, telling the organizer: asks for a comment first.
	if cmd := m.handleKey("enter"); cmd != nil || m.prompt == nil {
		t.Fatal("telling the organizer should ask for a comment")
	}
	typeText(m, "see you")
	if cmd := submit(m); cmd == nil || m.prompt != nil || m.promptDone != nil {
		t.Fatal("enter should send the answer")
	}
	if m.leaving != id {
		t.Errorf("leaving = %q, want the invite (it's archived next)", m.leaving)
	}

	m.Update(respondDoneMsg{answer: ipc.AnswerAccept, res: ipc.RespondResult{After: "archive"}})
	if !strings.Contains(m.notice, "Accepted; archived") {
		t.Errorf("notice = %q", m.notice)
	}
	m.Update(respondDoneMsg{answer: ipc.AnswerAccept, err: errString("guardrail: no")})
	if m.errText != "guardrail: no" || m.leaving != "" {
		t.Errorf("error: %q, leaving %q", m.errText, m.leaving)
	}
}

func TestCancelledMeetingRemoval(t *testing.T) {
	m, id := withInvite(t, store.MeetingCancelled)
	m.Update(meetingLoadedMsg{seq: m.bodySeq, id: id, info: ipc.MeetingInfo{Found: true, Cancelled: true}})
	if !strings.Contains(m.previewContent(100), "still on your calendar") {
		t.Error("cancellation should say it's still on the calendar")
	}
	press(m, "I")
	if m.confirm == nil || !strings.Contains(m.confirm.prompt, "Remove the cancelled meeting") || m.confirm.leaving != id {
		t.Fatalf("confirm = %+v", m.confirm)
	}
}

func TestIOnPlainMail(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	press(m, "I")
	if m.picker != nil || m.errText == "" {
		t.Error("I on plain mail should just say so")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestInviteClashes(t *testing.T) {
	m := newTestModel(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)
	window := [2]time.Time{at.AddDate(0, 0, -30), at.AddDate(0, 0, 180)}
	busy := store.EventDetails{Subject: "Standup", Response: "accepted", ShowAs: "busy"}
	m.st.ApplyEvents(ctx, m.account.ID, true, []store.Event{
		{GraphID: "own", Type: "singleInstance", Start: at, End: at.Add(time.Hour), Details: store.EventDetails{Subject: "The invite itself", ShowAs: "tentative"}},
		{GraphID: "a", Type: "singleInstance", Start: at.Add(30 * time.Minute), End: at.Add(90 * time.Minute), Details: busy},
		{GraphID: "b", Type: "singleInstance", Start: at.Add(-time.Hour), End: at, Details: busy}, // ends as it starts
		{GraphID: "c", Type: "singleInstance", Start: at, End: at.Add(time.Hour), Details: store.EventDetails{Subject: "Gym", ShowAs: "free"}},
	}, nil, "", window, at)
	got := clashes(ctx, m.st, m.account.ID, store.Meeting{Start: at, End: at.Add(time.Hour)}, "own")
	if len(got) != 1 || !strings.HasSuffix(got[0], "Standup") {
		t.Errorf("clashes = %q", got)
	}
}

func TestInviteButtons(t *testing.T) {
	m, id := withInvite(t, store.MeetingRequest)
	m.cfg.UI.Mouse = true
	m.Update(meetingLoadedMsg{seq: m.bodySeq, id: id, info: ipc.MeetingInfo{Found: true, Response: "tentativelyAccepted", JoinURL: "https://teams.example/j"}})
	var labels []string
	for _, b := range m.buttons.buttons {
		labels = append(labels, b.text())
	}
	if got := strings.Join(labels, "|"); got != " Accept | ✓ Tentative | Decline | Join " {
		t.Fatalf("buttons = %q", got)
	}
	g := m.geom()
	if !g.previewOnRight {
		t.Fatal("test expects the preview on the right")
	}
	x0 := g.folderW + 1 + g.listW + 1
	click := func(x int) tea.Cmd {
		return m.handleMouse(tea.MouseClickMsg{X: x, Y: 1 + m.buttons.line, Button: tea.MouseLeft})
	}
	// The gap between buttons does nothing.
	click(x0 + m.buttons.buttons[0].x1)
	if m.prompt != nil {
		t.Fatal("clicking between buttons shouldn't press one")
	}
	// Accept tells the organizer, so it asks for a comment first.
	click(x0 + m.buttons.buttons[0].x0 + 2)
	if m.prompt == nil || !strings.HasPrefix(m.prompt.Prompt, "Accept") {
		t.Fatal("Accept should ask for a comment")
	}
	if cmd := submit(m); cmd == nil || m.leaving != id {
		t.Fatalf("enter should send the answer (leaving %q)", m.leaving)
	}

	// A cancelled meeting offers removal.
	m.Update(meetingLoadedMsg{seq: m.bodySeq, id: id, info: ipc.MeetingInfo{Found: true, Cancelled: true}})
	if len(m.buttons.buttons) != 1 || m.buttons.buttons[0].label != "Remove from calendar" {
		t.Fatalf("buttons = %+v", m.buttons.buttons)
	}
	click(x0 + 1)
	if m.confirm == nil || !strings.Contains(m.confirm.prompt, "Remove the cancelled meeting") {
		t.Fatalf("confirm = %+v", m.confirm)
	}

	// No buttons without the mouse: the keys still work.
	m.confirm = nil
	m.cfg.UI.Mouse = false
	m.refreshPreview(false)
	if len(m.buttons.buttons) != 0 {
		t.Error("buttons shown with the mouse off")
	}
}
