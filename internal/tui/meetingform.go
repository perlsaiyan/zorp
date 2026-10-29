package tui

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/perlsaiyan/zorp/internal/compose"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/markdown"
	"github.com/perlsaiyan/zorp/internal/meeting"
	"github.com/perlsaiyan/zorp/internal/render"
	"github.com/perlsaiyan/zorp/internal/store"
)

// The meeting form: the fields of a meeting, filled in inside zorp, with
// a date picker for the day and suggestions for the people. Only the
// description is written in $EDITOR. ctrl+s goes on to the review
// screen (plan.go).

type formField int

const (
	fSubject formField = iota
	fDay
	fTime
	fLength
	fInvite
	fOptional
	fWhere
	fTeams
	fDescription
	numFields
)

var fieldNames = [numFields]string{"Subject", "Day", "Time", "Length", "Invite", "Optional", "Where", "Teams", "Description"}

type meetingForm struct {
	focus   formField
	series  bool // a whole recurring series: no day, time or length
	subject textinput.Model
	dayIn   textinput.Model // typed day, e.g. "tomorrow 2pm"; empty: the grid
	day     time.Time       // local midnight
	timeIn  textinput.Model // empty: all day
	length  textinput.Model
	invite  addrField
	opt     addrField
	where   textinput.Model
	teams   bool
	desc    string
	busy    map[time.Time]bool // days with meetings, for the grid
	busyFor time.Time          // the month busy covers
}

// addrField is a list of people: chips, and a line to type the next one
// with suggestions under it.
type addrField struct {
	chips []store.Address
	input textinput.Model
	sugg  []suggestion
	cur   int // selected suggestion
	seq   int // of the latest directory lookup
}

type suggestion struct {
	store.Address
	detail string // job title, or "you've mailed them"
}

func newInput(value string) textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	st := textinput.DefaultStyles(true)
	st.Focused.Placeholder = styleFaint
	st.Blurred.Placeholder = styleFaint
	st.Cursor.Blink = false
	ti.SetStyles(st)
	ti.SetValue(value)
	ti.CursorEnd()
	return ti
}

// newForm fills a form from pl.
func newForm(pl meeting.Plan, series bool) *meetingForm {
	f := &meetingForm{
		series:  series,
		subject: newInput(pl.Subject),
		dayIn:   newInput(""),
		timeIn:  newInput(""),
		length:  newInput(meeting.FormatLength(pl)),
		where:   newInput(pl.Where),
		teams:   pl.Teams,
		desc:    pl.Description,
	}
	f.invite = addrField{chips: pl.Invite, input: newInput("")}
	f.opt = addrField{chips: pl.Optional, input: newInput("")}
	f.dayIn.Placeholder = "type a day (\"tomorrow 2pm\") or use the arrows"
	f.subject.Placeholder = "what it's about"
	f.timeIn.Placeholder = "all day"
	f.invite.input.Placeholder = "type a name or address"
	f.opt.input.Placeholder = "type a name or address"
	if !pl.Start.IsZero() {
		f.day = localDay(pl.Start)
		if !pl.AllDay {
			f.timeIn.SetValue(pl.Start.Local().Format("15:04"))
		}
	}
	f.focusField(fSubject)
	return f
}

func (f *meetingForm) inputs() []*textinput.Model {
	return []*textinput.Model{&f.subject, &f.dayIn, &f.timeIn, &f.length, &f.invite.input, &f.opt.input, &f.where}
}

func (f *meetingForm) input(field formField) *textinput.Model {
	switch field {
	case fSubject:
		return &f.subject
	case fDay:
		return &f.dayIn
	case fTime:
		return &f.timeIn
	case fLength:
		return &f.length
	case fInvite:
		return &f.invite.input
	case fOptional:
		return &f.opt.input
	case fWhere:
		return &f.where
	}
	return nil
}

func (f *meetingForm) addrs(field formField) *addrField {
	switch field {
	case fInvite:
		return &f.invite
	case fOptional:
		return &f.opt
	}
	return nil
}

func (f *meetingForm) shown(field formField) bool {
	return !f.series || (field != fDay && field != fTime && field != fLength)
}

func (f *meetingForm) focusField(field formField) {
	for _, in := range f.inputs() {
		in.Blur()
	}
	f.focus = field
	if in := f.input(field); in != nil {
		in.Focus()
	}
}

func (f *meetingForm) move(step int) {
	next := f.focus
	for {
		next = formField((int(next) + step + int(numFields)) % int(numFields))
		if f.shown(next) {
			break
		}
	}
	if a := f.addrs(f.focus); a != nil {
		a.commitTyped()
		a.sugg = nil
	}
	f.focusField(next)
}

// allDay reports whether the Time field says all day.
func (f *meetingForm) allDay() bool {
	v := strings.ToLower(strings.TrimSpace(f.timeIn.Value()))
	return v == "" || v == "all day" || v == "allday"
}

// plan reads the form, and what's wrong with it.
func (f *meetingForm) plan() (meeting.Plan, []string) {
	pl := meeting.Plan{
		Subject: strings.TrimSpace(f.subject.Value()), Where: strings.TrimSpace(f.where.Value()), Teams: f.teams,
		Invite: f.invite.chips, Optional: f.opt.chips, Description: f.desc,
	}
	var problems []string
	if a, err := f.invite.typed(); err != nil {
		problems = append(problems, "Invite: "+err.Error())
	} else if a != nil {
		pl.Invite = append(append([]store.Address{}, pl.Invite...), *a)
	}
	if a, err := f.opt.typed(); err != nil {
		problems = append(problems, "Optional: "+err.Error())
	} else if a != nil {
		pl.Optional = append(append([]store.Address{}, pl.Optional...), *a)
	}
	if pl.Subject == "" {
		problems = append(problems, "Subject: the meeting needs one")
	}
	if f.series {
		return pl, problems
	}
	if strings.TrimSpace(f.dayIn.Value()) != "" {
		problems = append(problems, "Day: press Enter to use the day you typed, or Esc to clear it")
	}
	if f.day.IsZero() {
		problems = append(problems, "Day: pick one")
		return pl, problems
	}
	pl.AllDay = f.allDay()
	pl.Start = f.day
	if !pl.AllDay {
		h, mi, err := meeting.ParseClock(f.timeIn.Value())
		if err != nil {
			problems = append(problems, "Time: "+err.Error())
		}
		pl.Start = time.Date(f.day.Year(), f.day.Month(), f.day.Day(), h, mi, 0, 0, time.Local)
	}
	d, days, err := meeting.ParseLength(f.length.Value(), pl.AllDay)
	if err != nil {
		problems = append(problems, "Length: "+err.Error())
	}
	pl.Length, pl.Days = d, days
	return pl, problems
}

// typed is the address being typed, if it's a whole one.
func (a *addrField) typed() (*store.Address, error) {
	v := strings.TrimSpace(a.input.Value())
	if v == "" {
		return nil, nil
	}
	as, err := compose.ParseAddresses(v)
	if err != nil || len(as) != 1 {
		return nil, fmt.Errorf("%q isn't an address (pick a suggestion, or type name@example.com)", v)
	}
	return &as[0], nil
}

// commitTyped turns a whole typed address into a chip.
func (a *addrField) commitTyped() bool {
	if ad, err := a.typed(); err == nil && ad != nil {
		a.add(*ad)
		return true
	}
	return false
}

func (a *addrField) add(ad store.Address) {
	for _, c := range a.chips {
		if strings.EqualFold(c.Addr, ad.Addr) {
			a.input.SetValue("")
			a.sugg = nil
			return
		}
	}
	a.chips = append(a.chips, ad)
	a.input.SetValue("")
	a.sugg, a.cur = nil, 0
}

// formOpen reports whether keys go to the meeting form.
func (m *Model) formOpen() bool {
	return m.planning != nil && m.planning.form != nil && !m.planning.reviewing && m.confirm == nil && !m.help && m.planning.busy == ""
}

// formMsg handles keys and pastes while the form is open.
func (m *Model) formMsg(msg tea.Msg) tea.Cmd {
	f := m.planning.form
	k, isKey := msg.(tea.KeyPressMsg)
	if !isKey {
		return m.formInput(msg)
	}
	key := k.String()
	a := f.addrs(f.focus)
	gridKeys := f.focus == fDay && f.dayIn.Value() == ""
	switch key {
	case "ctrl+c":
		return tea.Quit
	case "ctrl+s":
		return m.reviewForm()
	case "ctrl+e":
		return m.editDescription()
	case "esc":
		switch {
		case a != nil && len(a.sugg) > 0:
			a.sugg = nil
		case f.focus == fDay && f.dayIn.Value() != "":
			f.dayIn.SetValue("")
		default:
			m.confirm = &confirm{prompt: "Discard this meeting? y/n", cmd: func() tea.Msg { return planDiscardMsg{} }}
		}
		return nil
	case "tab":
		if a != nil && len(a.sugg) > 0 && a.input.Value() != "" {
			a.add(a.sugg[a.cur].Address)
			return nil
		}
		f.move(1)
		return m.formFocusChanged()
	case "shift+tab":
		f.move(-1)
		return m.formFocusChanged()
	case "enter":
		switch {
		case a != nil && len(a.sugg) > 0:
			a.add(a.sugg[a.cur].Address)
			return nil
		case a != nil && a.input.Value() != "":
			if !a.commitTyped() {
				_, err := a.typed()
				m.errText = err.Error()
			}
			return nil
		case f.focus == fDay && !gridKeys:
			m.applyTypedDay()
			return nil
		case f.focus == fTeams:
			f.teams = !f.teams
			return nil
		case f.focus == fDescription:
			return m.editDescription()
		}
		f.move(1)
		return m.formFocusChanged()
	case "space":
		if f.focus == fTeams {
			f.teams = !f.teams
			return nil
		}
	case "up", "down":
		dir := 1
		if key == "up" {
			dir = -1
		}
		switch {
		case a != nil && len(a.sugg) > 0:
			a.cur = clamp(a.cur+dir, 0, len(a.sugg)-1)
			return nil
		case gridKeys:
			f.setDay(f.pickDay(m.now()).AddDate(0, 0, 7*dir))
			return nil
		case f.focus == fTime:
			f.stepTime(-dir) // up is later
			return nil
		case f.focus == fLength:
			f.stepLength(-dir) // up is longer
			return nil
		}
		f.move(dir)
		return m.formFocusChanged()
	case "left", "right":
		if gridKeys {
			dir := 1
			if key == "left" {
				dir = -1
			}
			f.setDay(f.pickDay(m.now()).AddDate(0, 0, dir))
			return nil
		}
	case "pgup", "pgdown":
		if gridKeys {
			dir := 1
			if key == "pgup" {
				dir = -1
			}
			f.setDay(f.pickDay(m.now()).AddDate(0, dir, 0))
			return nil
		}
	case "home":
		if gridKeys {
			f.setDay(localDay(m.now()))
			return nil
		}
	case "backspace":
		if a != nil && a.input.Value() == "" && len(a.chips) > 0 {
			a.chips = a.chips[:len(a.chips)-1]
			return nil
		}
	case ",", ";":
		if a != nil {
			if !a.commitTyped() && len(a.sugg) > 0 {
				a.add(a.sugg[a.cur].Address)
			}
			return nil
		}
	}
	if f.focus == fTeams || f.focus == fDescription {
		return nil
	}
	return m.formInput(msg)
}

// formInput passes a key or paste to the focused text field.
func (m *Model) formInput(msg tea.Msg) tea.Cmd {
	f := m.planning.form
	in := f.input(f.focus)
	if in == nil {
		return nil
	}
	before := in.Value()
	var cmd tea.Cmd
	*in, cmd = in.Update(msg)
	if a := f.addrs(f.focus); a != nil && in.Value() != before {
		return tea.Batch(cmd, m.suggest(a))
	}
	return cmd
}

// pickDay is the day the grid is on.
func (f *meetingForm) pickDay(now time.Time) time.Time {
	if f.day.IsZero() {
		return localDay(now)
	}
	return f.day
}

func (f *meetingForm) setDay(d time.Time) {
	f.day = localDay(d)
}

// applyTypedDay reads the Day field's text: a day, and maybe a time.
func (m *Model) applyTypedDay() {
	f := m.planning.form
	start, allDay, err := meeting.ParseWhen(f.dayIn.Value(), m.now())
	if err != nil {
		m.errText = "Day: " + err.Error()
		return
	}
	f.setDay(start)
	if !allDay {
		f.timeIn.SetValue(start.Format("15:04"))
		if _, _, err := meeting.ParseLength(f.length.Value(), false); err != nil {
			f.length.SetValue("30m")
		}
	}
	f.dayIn.SetValue("")
}

// stepTime moves the time by 15 minutes, or starts it at 9:00.
func (f *meetingForm) stepTime(dir int) {
	h, mi, err := meeting.ParseClock(f.timeIn.Value())
	if f.allDay() || err != nil {
		f.timeIn.SetValue("09:00")
	} else {
		t := time.Date(2000, 1, 1, h, mi, 0, 0, time.UTC).Add(time.Duration(dir) * 15 * time.Minute).Truncate(15 * time.Minute)
		f.timeIn.SetValue(t.Format("15:04"))
	}
	if _, _, err := meeting.ParseLength(f.length.Value(), false); err != nil {
		f.length.SetValue("30m")
	}
	f.timeIn.CursorEnd()
}

// stepLength changes the length by 15 minutes, or a day when all day.
func (f *meetingForm) stepLength(dir int) {
	allDay := f.allDay()
	d, days, err := meeting.ParseLength(f.length.Value(), allDay)
	pl := meeting.Plan{AllDay: allDay}
	switch {
	case allDay && err == nil:
		pl.Days = max(days+dir, 1)
	case allDay:
		pl.Days = 1
	case err == nil:
		pl.Length = max(d+time.Duration(dir)*15*time.Minute, 15*time.Minute)
	default:
		pl.Length = 30 * time.Minute
	}
	f.length.SetValue(meeting.FormatLength(pl))
	f.length.CursorEnd()
}

// formFocusChanged loads what the newly focused field shows.
func (m *Model) formFocusChanged() tea.Cmd {
	f := m.planning.form
	if f.focus == fDay {
		m.loadBusyDays()
	}
	return nil
}

// loadBusyDays marks the days in the grid's month that have meetings.
func (m *Model) loadBusyDays() {
	f := m.planning.form
	d := f.pickDay(m.now())
	month := time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, time.Local)
	if month.Equal(f.busyFor) {
		return
	}
	f.busyFor, f.busy = month, map[time.Time]bool{}
	evs, err := m.st.EventsBetween(m.ctx, m.account.ID, month.AddDate(0, 0, -7), month.AddDate(0, 1, 14))
	if err != nil {
		return
	}
	for _, e := range evs {
		if e.Details.Busy() && !e.Details.AllDay {
			f.busy[localDay(e.Start)] = true
		}
	}
}

// reviewForm is ctrl+s: on to the review screen.
func (m *Model) reviewForm() tea.Cmd {
	p := m.planning
	pl, problems := p.form.plan()
	p.plan, p.problems, p.reviewing = pl, problems, true
	if p.edit != nil && p.edit.Online && !pl.Teams {
		p.problems = append(p.problems, "Teams: the Teams meeting can't be taken off again")
	}
	return m.loadFreeBusy()
}

// editDescription writes the description in $EDITOR.
func (m *Model) editDescription() tea.Cmd {
	p := m.planning
	dir := m.cfg.Paths.ComposeDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		m.errText = err.Error()
		return nil
	}
	file, err := os.CreateTemp(dir, "meeting-"+time.Now().Format("20060102-150405")+"-*.md")
	if err == nil {
		_, err = file.WriteString(p.form.desc)
		if cerr := file.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		m.errText = "writing the description file: " + err.Error()
		return nil
	}
	p.path = file.Name()
	return tea.ExecProcess(editorCommand(m.cfg, p.path), func(err error) tea.Msg { return descEditedMsg{err} })
}

type descEditedMsg struct{ err error }

func (m *Model) descEdited(err error) {
	p := m.planning
	if p == nil || p.path == "" {
		return
	}
	defer func() { os.Remove(p.path); p.path = "" }()
	if err != nil {
		m.errText = "editor: " + err.Error() + " (set compose.editor or $EDITOR)"
	}
	data, rerr := os.ReadFile(p.path)
	if rerr != nil {
		m.errText = rerr.Error()
		return
	}
	p.form.desc = normDesc(string(data))
}

// normDesc trims a description to its text and one final newline.
func normDesc(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if s == "" {
		return ""
	}
	return s + "\n"
}

// formLines draws the form.
func (m *Model) formLines(w, h int) []string {
	p := m.planning
	f := p.form
	title := "New meeting"
	if p.edit != nil {
		title = "Change meeting"
		if p.series {
			title += " (the whole series)"
		}
	}
	lines := []string{"", styleBold.Render("  " + title), ""}
	valW := max(w-17, 20)
	label := func(field formField) string {
		l := padRight(fieldNames[field], 11)
		if f.focus == field {
			return styleAccent.Render("› " + l)
		}
		return styleHeader.Render("  " + l)
	}
	row := func(field formField, value string) {
		lines = append(lines, ansi.Truncate(label(field)+" "+value, w, "…"))
	}
	hint := func(s string) string { return styleFaint.Render("  " + s) }
	for field := fSubject; field < numFields; field++ {
		if !f.shown(field) {
			continue
		}
		in := f.input(field)
		if in != nil {
			in.SetWidth(valW)
		}
		switch field {
		case fSubject, fWhere:
			row(field, in.View())
		case fDay:
			day := styleFaint.Render("not picked")
			if !f.day.IsZero() {
				day = f.day.Format("Mon 02 Jan 2006")
			}
			if f.focus != fDay {
				row(field, day)
				continue
			}
			row(field, day+hint("arrows move · pgup/pgdn month · home today"))
			lines = append(lines, "              "+f.dayIn.View())
			lines = append(lines, m.dayGrid()...)
		case fTime:
			v := in.View()
			if f.focus == fTime {
				v += hint("↑/↓ 15 min · empty: all day")
			}
			row(field, v)
		case fLength:
			v := in.View()
			if f.focus == fLength {
				v += hint("↑/↓ change · 30m, 1h30m, or 2d all day")
			}
			row(field, v)
		case fInvite, fOptional:
			a := f.addrs(field)
			lines = append(lines, addrLines(label(field)+" ", a, f.focus == field, w)...)
		case fTeams:
			box := "[ ]"
			if f.teams {
				box = "[x]"
			}
			v := box + " a Teams link"
			if f.focus == fTeams {
				v += hint("space toggles")
			}
			row(field, v)
		case fDescription:
			v := styleFaint.Render("none")
			if f.desc != "" {
				v = strconv.Itoa(strings.Count(f.desc, "\n")) + " lines"
			}
			if f.focus == fDescription {
				v += hint("Enter writes it in $EDITOR")
			}
			row(field, v)
			if f.desc != "" {
				bodyW := max(w-16, 10)
				var body []string
				if defaultFormat(m.cfg) == compose.FormatMarkdown {
					body = render.FromHTML(markdown.ToHTML(f.desc)).Render(bodyW, render.Options{ShowQuoted: true, Styles: &renderStyles})
				} else {
					body = strings.Split(ansi.Wrap(cleanBody(f.desc), bodyW, ""), "\n")
				}
				for i, l := range body {
					if i == 6 {
						lines = append(lines, styleFaint.Render("              …"))
						break
					}
					lines = append(lines, "              "+l)
				}
			}
		}
	}
	return padLines(lines, w, h)
}

// addrLines draws a people field: its chips, the line being typed, and
// suggestions under it when focused.
func addrLines(prefix string, a *addrField, focused bool, w int) []string {
	var chips []string
	for _, c := range a.chips {
		chips = append(chips, styleCursorBg.Render(" "+clean(senderName(c))+" "))
	}
	head := strings.Join(chips, " ")
	if focused || len(chips) == 0 {
		if head != "" {
			head += " "
		}
		a.input.SetWidth(max(w-ansi.StringWidth(prefix)-ansi.StringWidth(head)-2, 12))
		head += a.input.View()
	}
	lines := []string{ansi.Truncate(prefix+head, w, "…")}
	if !focused {
		return lines
	}
	for i, s := range a.sugg {
		text := clean(senderName(s.Address))
		if !strings.EqualFold(s.Name, s.Addr) && s.Name != "" {
			text += "  " + styleFaint.Render(clean(s.Addr))
		}
		if s.detail != "" {
			text += styleFaint.Render("  · " + clean(s.detail))
		}
		line := ansi.Truncate("              "+text, w-2, "…")
		if i == a.cur {
			line = styleCursor.Render(fit(line, min(w-2, max(ansi.StringWidth(line)+2, 40))))
		}
		lines = append(lines, line)
	}
	return lines
}

// dayGrid is the month around the picked day.
func (m *Model) dayGrid() []string {
	f := m.planning.form
	m.loadBusyDays()
	d := f.pickDay(m.now())
	today := localDay(m.now())
	first := time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, time.Local)
	pad := "              "
	lines := []string{pad + styleBold.Render(d.Format("January 2006")), pad + styleFaint.Render("Mo  Tu  We  Th  Fr  Sa  Su")}
	start := first.AddDate(0, 0, -((int(first.Weekday()) + 6) % 7)) // back to Monday
	for week := 0; week < 6; week++ {
		var b strings.Builder
		b.WriteString(pad)
		for i := 0; i < 7; i++ {
			day := start.AddDate(0, 0, week*7+i)
			mark := " "
			if f.busy[day] {
				mark = "·"
			}
			cell := lipgloss.NewStyle()
			switch {
			case day.Equal(d):
				cell = styleCursor
			case day.Month() != d.Month() || day.Before(today):
				cell = styleFaint
			case day.Equal(today):
				cell = styleAccent
			}
			b.WriteString(cell.Render(padLeft(strconv.Itoa(day.Day()), 2) + mark))
			b.WriteString(" ")
		}
		lines = append(lines, b.String())
		if start.AddDate(0, 0, (week+1)*7).Month() != d.Month() && week >= 3 {
			break
		}
	}
	return lines
}

func padLeft(s string, n int) string {
	if w := ansi.StringWidth(s); w < n {
		return strings.Repeat(" ", n-w) + s
	}
	return s
}

// formHints is the bottom bar while the form is open.
func (m *Model) formHints() string {
	return "tab/shift+tab fields  ctrl+s review  ctrl+e description  esc discard"
}

// suggest updates a people field's suggestions from the mail cache, and
// asks the directory after a pause in typing.
func (m *Model) suggest(a *addrField) tea.Cmd {
	q := strings.TrimSpace(a.input.Value())
	a.seq++
	a.sugg, a.cur = m.localSuggestions(q, a.chips), 0
	if !m.cfg.Contacts.Directory || len([]rune(q)) < 2 {
		return nil
	}
	seq, field := a.seq, m.planning.form.focus
	return tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return lookupDueMsg{field: field, seq: seq, query: q} })
}

type (
	lookupDueMsg struct {
		field formField
		seq   int
		query string
	}
	peopleFoundMsg struct {
		field  formField
		seq    int
		people []suggestion
		err    error
	}
	contactsMsg struct {
		contacts []store.Contact
		err      error
	}
)

// lookupDue sends a directory lookup if the typing has stopped.
func (m *Model) lookupDue(r lookupDueMsg) tea.Cmd {
	if m.planning == nil || m.planning.form == nil {
		return nil
	}
	a := m.planning.form.addrs(r.field)
	if a == nil || a.seq != r.seq {
		return nil
	}
	cfg := m.cfg
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var res []ipc.Person
		err := callDaemon(ctx, cfg, ipc.MethodPeople, ipc.PeopleParams{Query: r.query, Top: 8}, &res)
		out := make([]suggestion, 0, len(res))
		for _, p := range res {
			out = append(out, suggestion{Address: store.Address{Name: p.Name, Addr: p.Addr}, detail: p.Title})
		}
		return peopleFoundMsg{field: r.field, seq: r.seq, people: out, err: err}
	}
}

// peopleFound merges directory results under the cached ones.
func (m *Model) peopleFound(r peopleFoundMsg) {
	if m.planning == nil || m.planning.form == nil {
		return
	}
	a := m.planning.form.addrs(r.field)
	if a == nil || a.seq != r.seq || r.err != nil {
		return // a failed lookup leaves the cached suggestions
	}
	seen := map[string]bool{}
	for _, s := range a.sugg {
		seen[strings.ToLower(s.Addr)] = true
	}
	for _, c := range a.chips {
		seen[strings.ToLower(c.Addr)] = true
	}
	for _, s := range r.people {
		if k := strings.ToLower(s.Addr); !seen[k] && len(a.sugg) < maxSuggestions {
			seen[k] = true
			a.sugg = append(a.sugg, s)
		}
	}
}

const maxSuggestions = 8

// loadContacts ranks the people in the mail cache, once per form.
func (m *Model) loadContacts() tea.Cmd {
	st, acct, self, now := m.st, m.account.ID, m.account.UPN, m.now()
	return func() tea.Msg {
		cs, err := st.Contacts(context.Background(), acct, self, now)
		return contactsMsg{contacts: cs, err: err}
	}
}

// localSuggestions are the cached people matching q, best first: by the
// start of a word in the name, or of the address.
func (m *Model) localSuggestions(q string, skip []store.Address) []suggestion {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	taken := map[string]bool{}
	for _, c := range skip {
		taken[strings.ToLower(c.Addr)] = true
	}
	var out []suggestion
	for _, c := range m.contacts {
		if taken[strings.ToLower(c.Addr)] || !contactMatches(c.Address, q) {
			continue
		}
		out = append(out, suggestion{Address: c.Address, detail: "you've had mail with them"})
		if len(out) == 5 {
			break // leave room for the directory
		}
	}
	return out
}

func contactMatches(a store.Address, q string) bool {
	addr := strings.ToLower(a.Addr)
	if strings.HasPrefix(addr, q) {
		return true
	}
	name := strings.ToLower(a.Name)
	if strings.HasPrefix(name, q) {
		return true
	}
	for _, w := range strings.FieldsFunc(name, func(r rune) bool { return r == ' ' || r == ',' || r == '.' || r == '-' }) {
		if strings.HasPrefix(w, q) {
			return true
		}
	}
	return false
}
