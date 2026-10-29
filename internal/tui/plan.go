package tui

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/perlsaiyan/zorp/internal/agenda"
	"github.com/perlsaiyan/zorp/internal/compose"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/markdown"
	"github.com/perlsaiyan/zorp/internal/meeting"
	"github.com/perlsaiyan/zorp/internal/render"
	"github.com/perlsaiyan/zorp/internal/store"
)

// Meetings the user organizes: n (calendar) and M (mail) plan a new one,
// e changes one, D cancels one. A meeting is filled in on a form
// (meetingform.go) and sent from a review screen.

// planning is a meeting being written: the form, then (reviewing) the
// review screen.
type planning struct {
	edit      *ipc.EventDetail // the meeting being changed; nil for a new one
	series    bool             // edit is a whole recurring series: no times
	htmlBody  bool             // edit's description had formatting
	form      *meetingForm
	reviewing bool
	path      string       // the description file, while $EDITOR has it
	orig      meeting.Plan // the meeting as it was (edit), or the form as it started
	plan      meeting.Plan // the form's, as reviewed
	problems  []string
	busy      string

	// Invitees' free/busy during the meeting, looked up for each review.
	fbKey  string // what fb is for: the people and times
	fb     []ipc.FreeBusy
	fbErr  string
	fbDone bool
}

type (
	planReadyMsg struct {
		p   *planning
		err error
	}
	planDoneMsg struct {
		res ipc.EventSaveResult
		new bool
		err error
	}
	planDiscardMsg struct{}
	freeBusyMsg    struct {
		key string
		res []ipc.FreeBusy
		err error
	}
	eventCancelledMsg struct {
		res ipc.EventCancelResult
		err error
	}
)

func (m *Model) calendarOff() bool {
	if !m.cfg.Calendar.Enabled {
		m.errText = "calendar access is off (calendar.enabled = false)"
		return true
	}
	return false
}

// nextSlot is when a meeting planned on day starts by default: the next
// whole hour today, else 10:00.
func (m *Model) nextSlot(day time.Time) time.Time {
	now := m.now()
	if localDay(now).Equal(localDay(day)) {
		return now.Truncate(time.Hour).Add(time.Hour)
	}
	d := localDay(day)
	return time.Date(d.Year(), d.Month(), d.Day(), 10, 0, 0, 0, time.Local)
}

// planNew starts a new meeting from seed.
func (m *Model) planNew(seed meeting.Plan) tea.Cmd {
	if m.planning != nil || m.composing != nil || m.calendarOff() {
		return nil
	}
	if seed.Length == 0 {
		seed.Length = 30 * time.Minute
	}
	seed.Teams = m.cfg.Calendar.Teams
	m.planning = &planning{form: newForm(seed, false), orig: seed}
	return m.loadContacts()
}

// planFromMessage is M: a meeting with the current message's people,
// about its subject.
func (m *Model) planFromMessage() tea.Cmd {
	msg, ok := m.currentMessage()
	if !ok {
		return nil
	}
	seen := map[string]bool{strings.ToLower(m.account.UPN): true}
	var people []store.Address
	for _, list := range [][]store.Address{{msg.From}, msg.To, msg.Cc} {
		for _, a := range list {
			if k := strings.ToLower(a.Addr); k != "" && !seen[k] {
				seen[k] = true
				people = append(people, a)
			}
		}
	}
	return m.planNew(meeting.Plan{Subject: compose.BareSubject(clean(msg.Subject)), Start: m.nextSlot(m.now()), Invite: people})
}

// planEdit fetches a meeting the user organized and opens it in the
// editor. series: id is a recurring series' master.
func (m *Model) planEdit(id string, series bool) tea.Cmd {
	if m.planning != nil || m.composing != nil || m.calendarOff() {
		return nil
	}
	cfg := m.cfg
	m.notice = "Preparing…"
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var ev ipc.EventDetail
		if err := callDaemon(ctx, cfg, ipc.MethodEventGet, ipc.EventRef{ID: id}, &ev); err != nil {
			return planReadyMsg{err: err}
		}
		return editReady(ev, series)
	}
}

// editReady is the file for changing ev.
func editReady(ev ipc.EventDetail, series bool) planReadyMsg {
	switch {
	case !ev.IsOrganizer:
		return planReadyMsg{err: errors.New("only the organizer can change this meeting")}
	case ev.Cancelled:
		return planReadyMsg{err: errors.New("this meeting was cancelled")}
	}
	pl := meeting.Plan{
		Subject: ev.Subject, Start: ev.Start.Local(), AllDay: ev.AllDay,
		Invite: fromIPC(ev.Required), Optional: fromIPC(ev.Optional), Where: ev.Location, Teams: ev.Online,
	}
	if ev.AllDay {
		pl.Start = time.Date(ev.Start.Year(), ev.Start.Month(), ev.Start.Day(), 0, 0, 0, 0, time.Local)
		pl.Days = max(int(ev.End.Sub(ev.Start).Round(24*time.Hour)/(24*time.Hour)), 1)
	} else {
		pl.Length = ev.End.Sub(ev.Start)
	}
	body := store.Body{Type: "text", Text: ev.Body}
	if ev.BodyType == "html" {
		body = store.Body{Type: "html", HTML: ev.Body}
	}
	pl.Description = normDesc(withoutJoinBlock(parseBody(body).Text(quoteWidth)))
	pl.Subject, pl.Where = strings.TrimSpace(pl.Subject), strings.TrimSpace(pl.Where)
	p := &planning{edit: &ev, series: series, htmlBody: ev.BodyType == "html", form: newForm(pl, series), orig: pl}
	return planReadyMsg{p: p}
}

// withoutJoinBlock drops the Teams join details Exchange adds to a
// meeting's description: they start at a line of underscores.
func withoutJoinBlock(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if t := strings.TrimSpace(l); len(t) >= 20 && strings.Trim(t, "_") == "" {
			lines = lines[:i]
			break
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func (m *Model) planReady(r planReadyMsg) tea.Cmd {
	m.notice = ""
	if r.err != nil {
		m.errText = r.err.Error()
		return nil
	}
	if m.planning != nil || m.composing != nil {
		return nil
	}
	m.planning = r.p
	return m.loadContacts()
}

// fbAddrs are the invitees whose free/busy the review screen shows: all
// but the user, whose own clashes are listed from the calendar.
func (m *Model) fbAddrs() []string {
	pl := m.planning.plan
	seen := map[string]bool{strings.ToLower(m.account.UPN): true}
	var out []string
	for _, list := range [][]store.Address{pl.Invite, pl.Optional} {
		for _, a := range list {
			if k := strings.ToLower(a.Addr); k != "" && !seen[k] {
				seen[k] = true
				out = append(out, a.Addr)
			}
		}
	}
	return out
}

// loadFreeBusy asks when the invitees are busy during the meeting.
func (m *Model) loadFreeBusy() tea.Cmd {
	p := m.planning
	addrs := m.fbAddrs()
	if p.plan.Start.IsZero() || len(addrs) == 0 {
		p.fbKey, p.fb, p.fbErr, p.fbDone = "", nil, "", false
		return nil
	}
	start, end := p.plan.Start, p.plan.End()
	key := strings.Join(addrs, ",") + "|" + start.String() + "|" + end.String()
	if key == p.fbKey {
		return nil // already looked up (or looking)
	}
	p.fbKey, p.fb, p.fbErr, p.fbDone = key, nil, "", false
	cfg := m.cfg
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var res []ipc.FreeBusy
		err := callDaemon(ctx, cfg, ipc.MethodFreeBusy, ipc.FreeBusyParams{Addrs: addrs, Start: start, End: end}, &res)
		return freeBusyMsg{key: key, res: res, err: err}
	}
}

func (m *Model) freeBusyLoaded(r freeBusyMsg) {
	p := m.planning
	if p == nil || r.key != p.fbKey {
		return
	}
	p.fb, p.fbDone = r.res, true
	if r.err != nil {
		p.fbErr = r.err.Error()
	}
}

// freeBusyLines are the review screen's "who's free" lines.
func (m *Model) freeBusyLines(w int) []string {
	p := m.planning
	if p.fbKey == "" {
		return nil
	}
	head := styleHeader.Render("  " + padRight("Free?", 10))
	switch {
	case !p.fbDone:
		return []string{head + " " + styleFaint.Render("checking…")}
	case p.fbErr != "":
		return []string{head + " " + styleFaint.Render(ansi.Truncate("couldn't check: "+clean(p.fbErr), max(w-13, 10), "…"))}
	}
	names := map[string]string{}
	for _, a := range append(append([]store.Address{}, p.plan.Invite...), p.plan.Optional...) {
		names[strings.ToLower(a.Addr)] = senderName(a)
	}
	var out []string
	busy := 0
	for _, fb := range p.fb {
		name := names[strings.ToLower(fb.Addr)]
		if name == "" {
			name = fb.Addr
		}
		var line string
		switch {
		case fb.Error != "":
			line = styleFaint.Render("?  " + clean(name) + "  (not shared)")
		case len(fb.Busy) == 0:
			line = "✓  " + clean(name) + styleFaint.Render("  free")
		default:
			busy++
			var spans []string
			for _, b := range fb.Busy {
				what := map[string]string{"tentative": "tentative", "oof": "away", "workingElsewhere": "elsewhere"}[b.Status]
				if what == "" {
					what = "busy"
				}
				spans = append(spans, what+" "+b.Start.Local().Format("15:04")+"–"+b.End.Local().Format("15:04"))
			}
			line = styleWarning.Render("✗  "+clean(name)) + "  " + strings.Join(spans, ", ")
		}
		if len(out) == 0 {
			out = append(out, ansi.Truncate(head+" "+line, w, "…"))
		} else {
			out = append(out, ansi.Truncate("  "+strings.Repeat(" ", 10)+" "+line, w, "…"))
		}
	}
	if busy > 0 {
		out = append(out, styleWarning.Render(ansi.Truncate("  ! "+people(busy)+" "+map[bool]string{true: "is", false: "are"}[busy == 1]+" busy then.", w, "…")))
	}
	return out
}

func (m *Model) endPlan() {
	if m.planning != nil {
		os.Remove(m.planning.path)
	}
	m.planning = nil
}

func (m *Model) planKey(key string) tea.Cmd {
	p := m.planning
	if p.busy != "" {
		return nil
	}
	switch key {
	case "y":
		if len(p.problems) > 0 {
			m.errText = "fix the problems first (e to edit)"
			return nil
		}
		params, changed := m.planParams()
		if !changed {
			m.endPlan()
			m.notice = "No changes."
			return nil
		}
		p.busy = "Sending…"
		if m.planNotified() == 0 {
			p.busy = "Saving…"
		}
		cfg, isNew := m.cfg, p.edit == nil
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			var res ipc.EventSaveResult
			err := callDaemon(ctx, cfg, ipc.MethodEventSave, params, &res)
			return planDoneMsg{res: res, new: isNew, err: err}
		}
	case "e", "esc":
		p.reviewing = false
	case "q":
		m.confirm = &confirm{prompt: "Discard this meeting? y/n", cmd: func() tea.Msg { return planDiscardMsg{} }}
	}
	return nil
}

// planParams is what to send: everything for a new meeting, the changes
// for an existing one.
func (m *Model) planParams() (ipc.EventSaveParams, bool) {
	p := m.planning
	pl, orig := p.plan, p.orig
	out := ipc.EventSaveParams{Format: defaultFormat(m.cfg)}
	if p.edit != nil {
		out.ID = p.edit.ID
	}
	isNew := p.edit == nil
	changed := false
	if isNew || pl.Subject != orig.Subject {
		out.Subject, changed = &pl.Subject, true
	}
	if !p.series && (isNew || !pl.Start.Equal(orig.Start) || !pl.End().Equal(orig.End()) || pl.AllDay != orig.AllDay) {
		start, end := pl.Start, pl.End()
		out.Start, out.End, out.AllDay, changed = &start, &end, &pl.AllDay, true
	}
	if (isNew && pl.Where != "") || (!isNew && pl.Where != orig.Where) {
		out.Location, changed = &pl.Where, true
	}
	if isNew || !sameAddrs(pl.Invite, orig.Invite) || !sameAddrs(pl.Optional, orig.Optional) {
		out.Attendees, changed = &ipc.Attendees{Required: toIPC(pl.Invite), Optional: toIPC(pl.Optional)}, true
	}
	if (isNew && pl.Teams) || (!isNew && pl.Teams && !orig.Teams) {
		out.Online, changed = &pl.Teams, true
	}
	if (isNew && pl.Description != "") || (!isNew && pl.Description != orig.Description) {
		out.Body, changed = &pl.Description, true
	}
	return out, changed
}

func sameAddrs(a, b []store.Address) bool {
	key := func(as []store.Address) []string {
		out := make([]string, 0, len(as))
		for _, x := range as {
			out = append(out, strings.ToLower(x.Addr))
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(key(a), key(b))
}

// planNotified is how many people saving the meeting would send mail to:
// everyone invited, and anyone taken off.
func (m *Model) planNotified() int {
	p := m.planning
	seen := map[string]bool{strings.ToLower(m.account.UPN): true}
	n := 0
	for _, list := range [][]store.Address{p.plan.Invite, p.plan.Optional, p.orig.Invite, p.orig.Optional} {
		for _, a := range list {
			if k := strings.ToLower(a.Addr); !seen[k] {
				seen[k] = true
				n++
			}
		}
	}
	return n
}

func (m *Model) planDone(r planDoneMsg) tea.Cmd {
	p := m.planning
	if p == nil {
		return nil
	}
	p.busy = ""
	if r.err != nil {
		m.errText = r.err.Error() // nothing changed; stay on the review screen
		return nil
	}
	m.endPlan()
	switch {
	case r.new && r.res.Notified > 0:
		m.notice = "Invited " + people(r.res.Notified) + "."
	case r.new:
		m.notice = "Put it on your calendar."
	case r.res.Notified > 0:
		m.notice = "Sent the update to " + people(r.res.Notified) + "."
	default:
		m.notice = "Changed it."
	}
	return nil
}

func people(n int) string {
	if n == 1 {
		return "1 person"
	}
	return strconv.Itoa(n) + " people"
}

// planLines is the review screen.
func (m *Model) planLines(w, h int) []string {
	p := m.planning
	pl := p.plan
	field := func(name, value string) string {
		return ansi.Truncate(styleHeader.Render("  "+padRight(name+":", 10))+" "+value, w, "…")
	}
	title := "New meeting"
	if p.edit != nil {
		title = "Change meeting"
		if p.series {
			title += " (the whole series)"
		}
	}
	var blocked []string
	for _, list := range [][]store.Address{pl.Invite, pl.Optional} {
		for _, a := range list {
			if !m.cfg.Safety.MaySendTo(a.Addr, m.account.UPN) {
				blocked = append(blocked, a.Addr)
			}
		}
	}
	notified := m.planNotified()
	status := "ready to save"
	switch {
	case p.busy != "":
		status = p.busy
	case len(p.problems) > 0:
		status = "needs fixing"
	case len(blocked) > 0:
		status = "the send guardrail won't let it go out"
	case notified > 0 && p.edit == nil:
		status = "y sends the invite to " + people(notified)
	case notified > 0:
		status = "y sends the update to " + people(notified)
	case p.edit == nil:
		status = "y puts it on your calendar; nobody else is invited"
	}
	lines := []string{"", styleBold.Render("  "+title) + styleFaint.Render("  ·  "+status), ""}
	lines = append(lines, field("Subject", clean(pl.Subject)))
	switch {
	case p.series:
		lines = append(lines, field("When", styleFaint.Render("recurring, unchanged")))
	case !pl.Start.IsZero():
		end := pl.End()
		when := meetingTime(&store.Meeting{Start: pl.Start, End: end, AllDay: pl.AllDay})
		if !pl.AllDay {
			when += styleFaint.Render("  (" + meeting.FormatLength(pl) + ")")
		}
		lines = append(lines, field("When", when))
	}
	if len(pl.Invite) > 0 {
		lines = append(lines, field("Invite", clean(compose.FormatAddresses(pl.Invite))))
	}
	if len(pl.Optional) > 0 {
		lines = append(lines, field("Optional", clean(compose.FormatAddresses(pl.Optional))))
	}
	if pl.Where != "" {
		lines = append(lines, field("Where", clean(pl.Where)))
	}
	teams := "no"
	if pl.Teams {
		teams = "yes"
		if p.edit == nil || !p.edit.Online {
			teams += styleFaint.Render("  (a link is added when it's saved)")
		}
	}
	lines = append(lines, field("Teams", teams))
	lines = append(lines, m.freeBusyLines(w)...)
	lines = append(lines, "")

	warn := func(s string) { lines = append(lines, styleWarning.Render(ansi.Truncate("  ! "+s, w, "…"))) }
	for _, pr := range p.problems {
		lines = append(lines, styleError.Render(ansi.Truncate("  ✗ "+clean(pr), w, "…")))
	}
	if !pl.Start.IsZero() && pl.End().Before(m.now()) {
		warn("That's in the past.")
	}
	if !pl.Start.IsZero() && !pl.AllDay {
		if cl := clashes(m.ctx, m.st, m.account.ID, store.Meeting{Start: pl.Start, End: pl.End()}, m.planOwnID()); len(cl) > 0 {
			warn("Clashes with " + clean(strings.Join(cl, "; ")))
		}
	}
	if p.edit != nil && p.htmlBody && pl.Description != p.orig.Description {
		warn("The new description replaces the old one and its formatting (the Teams link stays on the meeting).")
	}
	if len(blocked) > 0 {
		lines = append(lines, styleError.Render(ansi.Truncate("  Guardrail: can't send invites to "+clean(strings.Join(blocked, ", "))+" (safety.allow_send / send_allow).", w, "…")))
	}
	lines = append(lines, styleFaint.Render("  "+strings.Repeat("─", max(min(w-4, 72), 0))))
	bodyW := max(w-4, 10)
	if defaultFormat(m.cfg) == compose.FormatMarkdown {
		for _, l := range render.FromHTML(markdown.ToHTML(pl.Description)).Render(bodyW, render.Options{ShowQuoted: true, Styles: &renderStyles}) {
			lines = append(lines, "  "+l)
		}
	} else {
		for _, l := range strings.Split(ansi.Wrap(cleanBody(pl.Description), bodyW, ""), "\n") {
			lines = append(lines, "  "+l)
		}
	}
	return padLines(lines, w, h)
}

// planOwnID is the meeting being changed, left out of its own clashes.
func (m *Model) planOwnID() string {
	if p := m.planning; p != nil && p.edit != nil {
		if p.edit.SeriesMasterID != "" {
			return p.edit.SeriesMasterID
		}
		return p.edit.ID
	}
	return ""
}

// editEvent is e in the calendar: change a meeting the user organized.
func (m *Model) editEvent() tea.Cmd {
	e, ok := m.agenda.current()
	switch {
	case !ok:
		return nil
	case !e.Details.IsOrganizer:
		m.errText = "only the organizer can change this meeting (I answers it)"
		return nil
	case e.Details.Cancelled:
		m.errText = "this meeting was cancelled"
		return nil
	}
	return m.whichOfSeries(e, "Change", func(id string, series bool) tea.Cmd { return m.planEdit(id, series) })
}

// cancelEvent is D in the calendar: cancel a meeting the user organized,
// telling everyone invited.
func (m *Model) cancelEvent() tea.Cmd {
	e, ok := m.agenda.current()
	switch {
	case !ok:
		return nil
	case m.calendarOff():
		return nil
	case !e.Details.IsOrganizer:
		m.errText = "only the organizer can cancel this meeting (I declines it)"
		return nil
	case e.Details.Cancelled:
		m.errText = "this meeting was already cancelled (I removes it)"
		return nil
	}
	return m.whichOfSeries(e, "Cancel", func(id string, series bool) tea.Cmd {
		what := strconv.Quote(agenda.Subject(e))
		if series {
			what += " (every occurrence)"
		}
		n := 0
		for _, a := range e.Details.Attendees {
			if !strings.EqualFold(a.Addr, m.account.UPN) && a.Type != "resource" {
				n++
			}
		}
		if n == 0 {
			m.confirm = &confirm{prompt: "Delete " + what + " from your calendar? y/n", cmd: m.cancelCmd(id, "")}
			return nil
		}
		m.openTextPrompt("Cancel, note to attendees: ", "optional · Enter goes on · Esc stops", func(text string) tea.Cmd {
			m.confirm = &confirm{prompt: "Cancel " + what + " and tell " + people(n) + "? y/n", cmd: m.cancelCmd(id, text)}
			return nil
		})
		return nil
	})
}

// whichOfSeries runs do on e, asking first, for an occurrence of a
// series, whether it's meant for that one or the whole series.
func (m *Model) whichOfSeries(e store.Event, verb string, do func(id string, series bool) tea.Cmd) tea.Cmd {
	if e.SeriesMasterID == "" || (e.Type != "occurrence" && e.Type != "exception") {
		return do(e.GraphID, e.Type == "seriesMaster")
	}
	p := &picker{kind: pickAction, title: verb + " " + strconv.Quote(agenda.Subject(e))}
	p.all = []choice{
		{label: "This one", detail: e.Start.Local().Format("Mon 02 Jan 15:04"), run: func() tea.Cmd { return do(e.GraphID, false) }},
		{label: "The whole series", detail: "every occurrence", run: func() tea.Cmd { return do(e.SeriesMasterID, true) }},
	}
	m.picker = p
	m.filterPicker()
	return nil
}

func (m *Model) cancelCmd(id, comment string) tea.Cmd {
	cfg := m.cfg
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var res ipc.EventCancelResult
		err := callDaemon(ctx, cfg, ipc.MethodEventCancel, ipc.EventCancelParams{ID: id, Comment: comment}, &res)
		return eventCancelledMsg{res: res, err: err}
	}
}

func (m *Model) eventCancelled(r eventCancelledMsg) {
	switch {
	case r.err != nil:
		m.errText = r.err.Error()
	case r.res.Notified > 0:
		m.notice = "Cancelled; told " + people(r.res.Notified) + "."
	default:
		m.notice = "Deleted it from your calendar."
	}
}
