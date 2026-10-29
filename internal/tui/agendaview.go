package tui

import (
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/perlsaiyan/zorp/internal/agenda"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/store"
)

// The calendar (g c): two weeks of meetings from the cached calendar, by
// day, with the selected one's details beside them.

// agendaDays is how many days the view shows from its first day.
const agendaDays = 14

type agendaView struct {
	from   time.Time // first day shown, local midnight
	events []store.Event
	cur    int // selected event
	top    int // first list row shown
	err    string
}

func localDay(t time.Time) time.Time {
	t = t.Local()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
}

func (m *Model) openAgenda() {
	m.agenda = &agendaView{from: localDay(m.now())}
	m.loadAgenda("")
	// Start at the first meeting that hasn't ended.
	for i, e := range m.agenda.events {
		if e.End.After(m.now()) {
			m.agenda.cur = i
			break
		}
	}
}

func (m *Model) now() time.Time {
	if m.clock != nil {
		return m.clock()
	}
	return time.Now()
}

// loadAgenda reads the shown days from the cache, keeping the selection
// on keep (an event's graph ID) if it's still there.
func (m *Model) loadAgenda(keep string) {
	a := m.agenda
	evs, err := m.st.EventsBetween(m.ctx, m.account.ID, a.from, a.from.AddDate(0, 0, agendaDays))
	a.events, a.err = evs, ""
	if err != nil {
		a.err = err.Error()
	}
	a.cur = clamp(a.cur, 0, max(len(evs)-1, 0))
	for i, e := range evs {
		if keep != "" && e.GraphID == keep {
			a.cur = i
		}
	}
}

func (a *agendaView) current() (store.Event, bool) {
	if a.cur < 0 || a.cur >= len(a.events) {
		return store.Event{}, false
	}
	return a.events[a.cur], true
}

func (m *Model) agendaKey(key string) tea.Cmd {
	a := m.agenda
	switch key {
	case "q", "esc":
		m.agenda = nil
		m.refreshPreview(false)
	case "j", "down":
		a.cur = clamp(a.cur+1, 0, max(len(a.events)-1, 0))
	case "k", "up":
		a.cur = clamp(a.cur-1, 0, max(len(a.events)-1, 0))
	case "[", "]":
		days := 7
		if key == "[" {
			days = -7
		}
		a.from, a.cur, a.top = a.from.AddDate(0, 0, days), 0, 0
		m.loadAgenda("")
	case "t":
		m.openAgenda()
	case "g":
		m.gPending = true
	case "?":
		m.help = true
	case "O":
		e, ok := a.current()
		switch {
		case !ok:
		case e.Details.JoinURL != "":
			return m.openURL(e.Details.JoinURL)
		case strings.HasPrefix(e.Details.Location, "https://"):
			return m.openURL(e.Details.Location) // Zoom and others put the link here
		default:
			m.errText = "this meeting has no join link"
		}
	case "I":
		return m.answerEvent()
	}
	return nil
}

// answerEvent is I in the calendar: answer the selected meeting, or
// remove it if it was cancelled.
func (m *Model) answerEvent() tea.Cmd {
	e, ok := m.agenda.current()
	switch {
	case !ok:
		return nil
	case !m.cfg.Calendar.Enabled:
		m.errText = "calendar access is off (calendar.enabled = false)"
		return nil
	case e.Details.IsOrganizer:
		m.errText = "you organized this meeting"
		return nil
	}
	// A series is answered as a whole, through its master.
	id := e.GraphID
	if e.Type == "occurrence" && e.SeriesMasterID != "" {
		id = e.SeriesMasterID
	}
	if e.Details.Cancelled {
		m.confirm = &confirm{prompt: "Remove the cancelled meeting from your calendar? y/n", cmd: m.respond("", id, ipc.AnswerRemove, "", false)}
		return nil
	}
	title := "Answer " + strconv.Quote(agenda.Subject(e))
	if id != e.GraphID {
		title += " (the whole series)"
	}
	m.openAnswerPicker(&picker{kind: pickAnswer, title: title, eventID: id}, e.Details.ResponseRequested)
	return nil
}

// agendaLines draws the calendar: the list, and details when there's room.
func (m *Model) agendaLines(w, h int) []string {
	a := m.agenda
	listW, detailW := w, 0
	if w >= 90 {
		listW = max(w*11/20, 46)
		detailW = w - listW - 1
	}
	list := m.agendaList(listW, h)
	if detailW <= 0 {
		return padLines(list, w, h)
	}
	var detail []string
	if e, ok := a.current(); ok {
		detail = eventDetail(e, detailW)
	}
	out := make([]string, 0, h)
	for i := 0; i < h; i++ {
		l, r := "", ""
		if i < len(list) {
			l = list[i]
		}
		if i < len(detail) {
			r = detail[i]
		}
		out = append(out, fit(l, listW)+styleFaint.Render("│")+fit(r, detailW))
	}
	return out
}

func (m *Model) agendaList(w, h int) []string {
	a := m.agenda
	if a.err != "" {
		return []string{styleError.Render(" " + clean(a.err))}
	}
	if len(a.events) == 0 {
		msg := " Nothing on the calendar these two weeks."
		if m.cfg.Calendar.Enabled {
			msg += styleFaint.Render("  ([ ] other weeks)")
		} else {
			msg = " The calendar is off (calendar.enabled = false)."
		}
		return []string{"", msg}
	}
	now := m.now()
	var rows []string
	curRow, day := 0, time.Time{}
	for i, e := range a.events {
		start := e.Start.Local()
		if start.Before(a.from) {
			start = a.from // began before the shown days
		}
		if d := localDay(start); !d.Equal(day) {
			day = d
			if len(rows) > 0 {
				rows = append(rows, "")
			}
			rows = append(rows, styleBold.Render(" "+dayLabel(d, localDay(now))))
		}
		if i == a.cur {
			curRow = len(rows)
		}
		rows = append(rows, m.agendaRow(e, i == a.cur, now, w))
	}
	// Keep the selected row (and its day heading) in view.
	if curRow < a.top+1 {
		a.top = max(curRow-1, 0)
	}
	if curRow >= a.top+h {
		a.top = curRow - h + 1
	}
	a.top = clamp(a.top, 0, max(len(rows)-h, 0))
	return rows[a.top:min(len(rows), a.top+h)]
}

func dayLabel(d, today time.Time) string {
	switch {
	case d.Equal(today):
		return "Today, " + d.Format("Mon 02 Jan")
	case d.Equal(today.AddDate(0, 0, 1)):
		return "Tomorrow, " + d.Format("Mon 02 Jan")
	}
	return d.Format("Mon 02 Jan")
}

func (m *Model) agendaRow(e store.Event, cursor bool, now time.Time, w int) string {
	mark := " "
	switch agenda.Status(e) {
	case "not answered":
		mark = "?"
	case "tentative":
		mark = "~"
	}
	if e.Details.JoinURL != "" || strings.HasPrefix(e.Details.Location, "https://") {
		mark += "◦"
	} else {
		mark += " "
	}
	row := "  " + mark + " " + padRight(agenda.When(e), 11) + "  " + agenda.Subject(e)
	if s := agenda.Status(e); s == "declined" || s == "cancelled" {
		row += "  (" + s + ")"
	}
	row = ansi.Truncate(row, w, "…")
	switch {
	case cursor:
		return styleCursor.Render(fit(row, w))
	case !e.Details.Busy() || !e.End.After(now):
		return styleFaint.Render(row)
	}
	return row
}

func padRight(s string, n int) string {
	if w := ansi.StringWidth(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

// eventDetail is the selected meeting's details, wrapped to w.
func eventDetail(e store.Event, w int) []string {
	d := e.Details
	var lines []string
	// Every line is indented one space from the divider, wrapped ones too.
	add := func(s string) {
		for _, l := range strings.Split(ansi.Wrap(s, w-2, ""), "\n") {
			lines = append(lines, " "+l)
		}
	}
	field := func(name, value string) {
		if value != "" {
			add(styleHeader.Render(name+":") + " " + value)
		}
	}
	add(styleBold.Render(agenda.Subject(e)))
	lines = append(lines, "")
	field("When", meetingTime(&store.Meeting{Start: e.Start, End: e.End, AllDay: d.AllDay}))
	if e.Type == "occurrence" || e.Type == "exception" {
		lines[len(lines)-1] += styleFaint.Render("  (recurring)")
	}
	field("Where", clean(d.Location))
	if d.IsOrganizer {
		field("Organizer", "you")
	} else {
		field("Organizer", formatAddr(d.Organizer))
	}
	status := answerText(d.Response)
	if d.Cancelled {
		status = styleWarning.Render("cancelled") + styleFaint.Render("  (I removes it)")
	} else if !d.IsOrganizer {
		status += styleFaint.Render("  (I to answer)")
	}
	field("Status", status)
	if d.JoinURL != "" {
		field("Join", clean(d.JoinURL)+styleFaint.Render("  (O)"))
	} else if strings.HasPrefix(d.Location, "https://") {
		field("Join", styleFaint.Render("the link in Where (O)"))
	}
	if n := len(d.Attendees); n > 0 {
		counts := map[string]int{}
		for _, a := range d.Attendees {
			counts[a.Response]++
		}
		var parts []string
		for _, r := range []struct{ key, label string }{{"accepted", "accepted"}, {"tentativelyAccepted", "tentative"}, {"declined", "declined"}, {"none", "no answer"}, {"notResponded", "no answer"}} {
			if c := counts[r.key]; c > 0 {
				parts = append(parts, strconv.Itoa(c)+" "+r.label)
			}
		}
		field("Invited", strconv.Itoa(n)+styleFaint.Render("  "+strings.Join(parts, ", ")))
		for i, a := range d.Attendees {
			if i == 12 {
				add(styleFaint.Render("  and " + strconv.Itoa(n-12) + " more"))
				break
			}
			add("  " + formatAddr(a.Address) + styleFaint.Render("  "+attendeeAnswer(a)))
		}
	}
	if p := strings.TrimSpace(d.Preview); p != "" {
		lines = append(lines, "")
		for _, para := range strings.Split(cleanBody(p), "\n") {
			add(para)
		}
	}
	return lines
}

func attendeeAnswer(a store.Attendee) string {
	switch a.Response {
	case "accepted":
		return "accepted"
	case "tentativelyAccepted":
		return "tentative"
	case "declined":
		return "declined"
	case "organizer":
		return "organizer"
	}
	return ""
}
