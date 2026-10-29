package tui

import (
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
