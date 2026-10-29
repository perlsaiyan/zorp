package tui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/perlsaiyan/zorp/internal/agenda"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/store"
)

// Meeting invites (I). Exchange already puts an invite on the calendar as
// tentative; answering it, and taking cancelled meetings off the
// calendar, is up to the client.

// meetingState is the calendar's side of the previewed meeting message.
type meetingState struct {
	forID   string // graph ID of the meeting message
	info    ipc.MeetingInfo
	err     string
	known   bool     // info or err has arrived
	clashes []string // other meetings at the same time, from the cached calendar
}

type meetingLoadedMsg struct {
	seq     int
	id      string
	info    ipc.MeetingInfo
	clashes []string
	err     error
}

type respondDoneMsg struct {
	answer string
	res    ipc.RespondResult
	err    error
}

// meetingOf is the meeting message a row stands for: the newest one in
// its conversation, in the conversation view.
func (m *Model) meetingOf(msg store.Message) (store.Message, bool) {
	for _, mm := range m.members(msg) {
		if mm.Meeting != nil {
			return mm, true
		}
	}
	return store.Message{}, false
}

// answerable says whether I can do anything with a meeting message.
func answerable(mt *store.Meeting) bool {
	return mt != nil && (mt.Kind == store.MeetingRequest || mt.Kind == store.MeetingCancelled)
}

// loadMeeting asks the daemon how the current row's meeting stands on the
// calendar.
func (m *Model) loadMeeting() tea.Cmd {
	m.meeting = nil
	cur, ok := m.currentMessage()
	if !ok || !m.cfg.Calendar.Enabled {
		return nil
	}
	msg, ok := m.meetingOf(cur)
	if !ok || !answerable(msg.Meeting) {
		return nil
	}
	m.meeting = &meetingState{forID: msg.GraphID}
	cfg, st, acct, seq, id, mt := m.cfg, m.st, m.account.ID, m.bodySeq, msg.GraphID, *msg.Meeting
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var info ipc.MeetingInfo
		err := callDaemon(ctx, cfg, ipc.MethodMeeting, ipc.MessageRef{GraphID: id}, &info)
		r := meetingLoadedMsg{seq: seq, id: id, info: info, err: err}
		if err == nil && mt.Kind == store.MeetingRequest && !mt.AllDay && mt.End.After(mt.Start) {
			r.clashes = clashes(ctx, st, acct, mt, info.EventID)
		}
		return r
	}
}

// clashes lists the cached meetings that overlap mt, other than its own
// event (or, for a series, its occurrences).
func clashes(ctx context.Context, st *store.Store, acct int64, mt store.Meeting, own string) []string {
	evs, err := st.EventsBetween(ctx, acct, mt.Start, mt.End)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range evs {
		if e.Details.AllDay || !e.Details.Busy() || (own != "" && (e.GraphID == own || e.SeriesMasterID == own)) {
			continue
		}
		out = append(out, agenda.When(e)+" "+agenda.Subject(e))
	}
	return out
}

func (m *Model) meetingLoaded(r meetingLoadedMsg) {
	if r.seq != m.bodySeq || m.meeting == nil || m.meeting.forID != r.id {
		return
	}
	m.meeting.info, m.meeting.known, m.meeting.clashes = r.info, true, r.clashes
	if r.err != nil {
		m.meeting.err = r.err.Error()
	}
	m.refreshPreview(false)
}

// meetingFields are the preview's header lines for a meeting message.
func (m *Model) meetingFields(cur store.Message) [][2]string {
	msg, ok := m.meetingOf(cur)
	if !ok {
		return nil
	}
	mt := msg.Meeting
	when := meetingTime(mt)
	if mt.Recurring {
		when += styleFaint.Render("  (recurring)")
	}
	fields := [][2]string{{"Meeting", when}}
	if mt.Location != "" {
		fields = append(fields, [2]string{"Where", clean(mt.Location)})
	}
	var state string
	switch mt.Kind {
	case store.MeetingRequest:
		switch {
		case mt.OutOfDate:
			state = styleWarning.Render("out of date: a newer invite replaced this one")
		case m.meeting == nil || !m.meeting.known:
			state = styleFaint.Render("checking your calendar…")
		case m.meeting.err != "":
			state = styleError.Render(clean(m.meeting.err))
		case !m.meeting.info.Found:
			state = "not on your calendar any more"
		case m.meeting.info.Cancelled:
			state = styleWarning.Render("cancelled since") + styleFaint.Render("  (I removes it from your calendar)")
		default:
			state = answerText(m.meeting.info.Response) + styleFaint.Render("  (I to answer)")
		}
	case store.MeetingCancelled:
		switch {
		case m.meeting == nil || !m.meeting.known:
			state = styleWarning.Render("cancelled")
		case m.meeting.err != "":
			state = styleWarning.Render("cancelled") + "  " + styleError.Render(clean(m.meeting.err))
		case m.meeting.info.Found:
			state = styleWarning.Render("cancelled, still on your calendar") + styleFaint.Render("  (I removes it)")
		default:
			state = styleWarning.Render("cancelled") + styleFaint.Render(", off your calendar")
		}
	case store.MeetingAccepted, store.MeetingTentative, store.MeetingDeclined:
		verb := map[string]string{store.MeetingAccepted: "accepted", store.MeetingTentative: "tentatively accepted", store.MeetingDeclined: "declined"}[mt.Kind]
		state = clean(senderName(msg.From)) + " " + verb
	}
	if state != "" {
		fields = append(fields, [2]string{"Status", state})
	}
	if m.meeting != nil && len(m.meeting.clashes) > 0 {
		fields = append(fields, [2]string{"Clashes", styleWarning.Render(clean(strings.Join(m.meeting.clashes, "; ")))})
	}
	if m.meeting != nil && m.meeting.info.JoinURL != "" {
		fields = append(fields, [2]string{"Join", clean(m.meeting.info.JoinURL)})
	}
	return fields
}

// meetingButtons are the preview's buttons for a meeting message: answer
// it, remove it once cancelled, join it. Only with the mouse on, and once
// the calendar has said how the meeting stands.
func (m *Model) meetingButtons(cur store.Message) []button {
	msg, ok := m.meetingOf(cur)
	if !ok || !m.cfg.UI.Mouse || !answerable(msg.Meeting) || m.meeting == nil || m.meeting.forID != msg.GraphID || !m.meeting.known || m.meeting.err != "" {
		return nil
	}
	info := m.meeting.info
	var bs []button
	switch {
	case !info.Found:
	case msg.Meeting.Kind == store.MeetingCancelled || info.Cancelled:
		bs = append(bs, button{label: "Remove from calendar", press: m.meetingKey})
	case !info.Organizer:
		for _, a := range []struct{ answer, label, response string }{
			{ipc.AnswerAccept, "Accept", "accepted"},
			{ipc.AnswerTentative, "Tentative", "tentativelyAccepted"},
			{ipc.AnswerDecline, "Decline", "declined"},
		} {
			bs = append(bs, button{label: a.label, active: info.Response == a.response, press: func() tea.Cmd {
				// Telling the organizer goes through the comment prompt, so
				// it's never one click; I answers without telling them.
				return m.answerPicked(&picker{graphID: msg.GraphID}, choice{label: a.label, answer: a.answer, send: msg.Meeting.ResponseRequested})
			}})
		}
	}
	if url := info.JoinURL; url != "" {
		bs = append(bs, button{label: "Join", press: func() tea.Cmd { return m.openURL(url) }})
	}
	return bs
}

func answerText(response string) string {
	switch response {
	case "accepted":
		return "you accepted"
	case "tentativelyAccepted":
		return "you said tentative"
	case "declined":
		return "you declined"
	case "organizer":
		return "you organized it"
	}
	return styleAccent.Render("not answered yet")
}

// meetingTime formats a meeting's time in local time.
func meetingTime(mt *store.Meeting) string {
	start, end := mt.Start.Local(), mt.End.Local()
	if mt.Start.IsZero() {
		return "(no time given)"
	}
	day := "Mon 02 Jan 2006"
	if mt.AllDay {
		// All-day events end at midnight after their last day.
		last := end.Add(-time.Minute)
		if end.IsZero() || !last.After(start) || sameDay(start, last) {
			return start.Format(day) + ", all day"
		}
		return start.Format(day) + " – " + last.Format(day) + ", all day"
	}
	if end.IsZero() || !end.After(start) {
		return start.Format(day + " 15:04")
	}
	if sameDay(start, end) {
		return start.Format(day+" 15:04") + "–" + end.Format("15:04")
	}
	return start.Format(day+" 15:04") + " – " + end.Format(day+" 15:04")
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// meetingKey is I: answer the invite, or remove a cancelled meeting.
func (m *Model) meetingKey() tea.Cmd {
	cur, ok := m.currentMessage()
	if !ok {
		return nil
	}
	msg, ok := m.meetingOf(cur)
	switch {
	case !ok:
		m.errText = "not a meeting invite"
		return nil
	case !m.cfg.Calendar.Enabled:
		m.errText = "calendar access is off (calendar.enabled = false)"
		return nil
	case !answerable(msg.Meeting):
		m.errText = "nothing to answer: this is a reply to your meeting"
		return nil
	}
	known := m.meeting != nil && m.meeting.forID == msg.GraphID && m.meeting.known && m.meeting.err == ""
	cancelled := msg.Meeting.Kind == store.MeetingCancelled || (known && m.meeting.info.Cancelled)
	if cancelled {
		if known && !m.meeting.info.Found {
			m.errText = "the meeting is already off your calendar"
			return nil
		}
		m.confirm = &confirm{
			prompt:  "Remove the cancelled meeting from your calendar" + m.afterNote() + "? y/n",
			cmd:     m.respond(msg.GraphID, "", ipc.AnswerRemove, "", false),
			leaving: m.leavingAfter(msg.GraphID),
		}
		return nil
	}
	if known && m.meeting.info.Organizer {
		m.errText = "you organized this meeting"
		return nil
	}
	m.openAnswerPicker(&picker{kind: pickAnswer, title: "Answer the invite" + m.afterNote(), graphID: msg.GraphID}, msg.Meeting.ResponseRequested)
	return nil
}

// openAnswerPicker offers accept, tentative and decline, each with and
// without telling the organizer when they asked for answers.
func (m *Model) openAnswerPicker(p *picker, responseRequested bool) {
	tell, quiet := "and tell the organizer", "without telling the organizer"
	if !responseRequested {
		tell = "(the organizer didn't ask for answers)"
	}
	for _, a := range []struct{ answer, label string }{{ipc.AnswerAccept, "Accept"}, {ipc.AnswerTentative, "Tentative"}, {ipc.AnswerDecline, "Decline"}} {
		if responseRequested {
			p.all = append(p.all, choice{label: a.label, detail: tell, answer: a.answer, send: true})
			p.all = append(p.all, choice{label: a.label, detail: quiet, answer: a.answer})
		} else {
			p.all = append(p.all, choice{label: a.label, detail: tell, answer: a.answer})
		}
	}
	m.picker = p
	m.filterPicker()
}

// afterNote says what happens to the message afterwards.
func (m *Model) afterNote() string {
	switch m.cfg.Calendar.AfterRespond {
	case "archive":
		return " (then archive it)"
	case "delete":
		return " (then delete it)"
	}
	return ""
}

func (m *Model) leavingAfter(graphID string) string {
	if m.cfg.Calendar.AfterRespond == "keep" {
		return ""
	}
	return graphID
}

// answerPicked handles a choice in the answer picker: answers that tell
// the organizer first ask for an optional comment.
func (m *Model) answerPicked(p *picker, c choice) tea.Cmd {
	leave := func() {
		if p.graphID != "" {
			m.leaving = m.leavingAfter(p.graphID)
		}
	}
	if !c.send {
		leave()
		return m.respond(p.graphID, p.eventID, c.answer, "", false)
	}
	prompt := "Comment: "
	if c.label != "" {
		prompt = c.label + ", comment: "
	}
	m.openTextPrompt(prompt, "optional note to the organizer · Enter sends · Esc cancels", func(text string) tea.Cmd {
		leave()
		return m.respond(p.graphID, p.eventID, c.answer, text, true)
	})
	return nil
}

// respond answers the invite graphID, or (from the calendar) the event
// eventID.
func (m *Model) respond(graphID, eventID, answer, comment string, send bool) tea.Cmd {
	m.notice = "Answering…"
	if answer == ipc.AnswerRemove {
		m.notice = "Removing the meeting…"
	}
	cfg := m.cfg
	p := ipc.RespondParams{GraphID: graphID, EventID: eventID, Answer: answer, Comment: strings.TrimSpace(comment), Send: send}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var res ipc.RespondResult
		err := callDaemon(ctx, cfg, ipc.MethodRespond, p, &res)
		return respondDoneMsg{answer: answer, res: res, err: err}
	}
}

func (m *Model) respondDone(r respondDoneMsg) tea.Cmd {
	m.notice = ""
	if r.err != nil {
		m.leaving = ""
		m.errText = r.err.Error()
		return nil
	}
	done := map[string]string{ipc.AnswerAccept: "Accepted", ipc.AnswerTentative: "Answered tentative", ipc.AnswerDecline: "Declined", ipc.AnswerRemove: "Removed from your calendar"}[r.answer]
	switch {
	case r.res.AfterError != "":
		m.leaving = ""
		m.errText = done + ", but " + m.cfg.Calendar.AfterRespond + " failed: " + r.res.AfterError
	case r.res.After == "archive":
		m.notice = done + "; archived the invite"
	case r.res.After == "delete":
		m.notice = done + "; deleted the invite"
	default:
		m.notice = done
	}
	if m.agenda != nil {
		return nil // the calendar refreshes when the daemon has synced the change
	}
	return m.loadMeeting()
}
