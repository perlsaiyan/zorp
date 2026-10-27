package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/tom/zorp/internal/ipc"
	"github.com/tom/zorp/internal/store"
)

// Basic ANSI colors, so zorp follows the terminal's theme.
var (
	styleBar      = lipgloss.NewStyle().Reverse(true)
	styleBold     = lipgloss.NewStyle().Bold(true)
	styleFaint    = lipgloss.NewStyle().Faint(true)
	styleCursor   = lipgloss.NewStyle().Reverse(true)
	styleCursorBg = lipgloss.NewStyle().Underline(true) // cursor of an unfocused pane
	styleError    = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	styleAccent   = lipgloss.NewStyle().Foreground(lipgloss.Color("4")).Bold(true)
	styleHeader   = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
)

// geom is the screen layout for the current size and settings.
type geom struct {
	bodyH          int // rows between the top and bottom bars
	folderW        int // 0 when the folder pane is hidden
	listW, listH   int
	previewW       int
	previewH       int // 0 when the preview is hidden
	previewOnRight bool
}

func (m *Model) geom() geom {
	g := geom{bodyH: max(m.height-2, 0)}
	rest := m.width
	if m.showFolders && m.width >= 60 {
		g.folderW = clamp(m.width/5, 16, 28)
		rest -= g.folderW + 1
	}
	g.listW, g.listH = rest, g.bodyH
	switch m.layout {
	case previewRight:
		if rest >= 80 {
			g.listW = rest * 55 / 100
			g.previewW, g.previewH = rest-g.listW-1, g.bodyH
			g.previewOnRight = true
		}
	case previewBottom:
		listH := max(g.bodyH*2/5, 3)
		if g.bodyH-listH-1 >= 3 {
			g.listH = listH
			g.previewW, g.previewH = rest, g.bodyH-listH-1
		}
	}
	return g
}

// refreshPreview resizes the viewport and re-renders the current message
// into it. reset scrolls back to the top (for a newly selected message).
func (m *Model) refreshPreview(reset bool) {
	g := m.geom()
	w, h := g.previewW, g.previewH
	if m.reading {
		w, h = m.width, g.bodyH
	}
	m.ensureVisible()
	if w <= 0 || h <= 0 {
		return
	}
	m.preview.SetWidth(w)
	m.preview.SetHeight(h)
	m.preview.SetContent(m.previewContent(w))
	if reset {
		m.preview.GotoTop()
	}
}

// ensureVisible scrolls the message list so the cursor row is on screen.
func (m *Model) ensureVisible() {
	h := m.geom().listH
	m.listLen = h
	if h <= 0 {
		return
	}
	if m.msgCur < m.msgTop {
		m.msgTop = m.msgCur
	}
	if m.msgCur >= m.msgTop+h {
		m.msgTop = m.msgCur - h + 1
	}
	m.msgTop = clamp(m.msgTop, 0, max(len(m.msgs)-h, 0))
}

func (m *Model) previewContent(w int) string {
	msg, ok := m.currentMessage()
	if !ok {
		return ""
	}
	var b strings.Builder
	field := func(name, value string) {
		if value == "" {
			return
		}
		b.WriteString(ansi.Wrap(styleHeader.Render(name+":")+" "+value, w, ""))
		b.WriteString("\n")
	}
	field("From", formatAddr(msg.From))
	field("To", formatAddrs(msg.To))
	field("Cc", formatAddrs(msg.Cc))
	field("Date", msg.ReceivedAt.Local().Format("Mon, 02 Jan 2006 15:04"))
	b.WriteString(ansi.Wrap(styleHeader.Render("Subject:")+" "+styleBold.Render(clean(msg.Subject)), w, ""))
	b.WriteString("\n")
	b.WriteString(styleFaint.Render(strings.Repeat("─", w)))
	b.WriteString("\n")

	var body string
	switch m.bodyState {
	case bodyReady:
		body = cleanBody(m.bodyText)
		if body == "" {
			body = styleFaint.Render("(no text in this message)")
		}
	case bodyFailed:
		body = cleanBody(msg.BodyPreview) + "\n\n" + styleError.Render("Couldn't load the full message: "+m.bodyErr)
	default:
		body = cleanBody(msg.BodyPreview) + "\n\n" + styleFaint.Render("Loading…")
	}
	b.WriteString(ansi.Wrap(body, w, ""))
	return b.String()
}

func (m *Model) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = true
	v.WindowTitle = "zorp"
	if m.width <= 0 || m.height <= 0 {
		return v
	}
	if f, ok := m.currentFolder(); ok {
		v.WindowTitle = "zorp: " + f.Name
	}

	g := m.geom()
	lines := make([]string, 0, m.height)
	lines = append(lines, m.topBar())
	switch {
	case m.help:
		lines = append(lines, padLines(helpLines(), m.width, g.bodyH)...)
	case m.reading:
		lines = append(lines, padLines(strings.Split(m.preview.View(), "\n"), m.width, g.bodyH)...)
	default:
		lines = append(lines, m.panes(g)...)
	}
	lines = append(lines, m.bottomBar())
	// The top and bottom bars alone need two rows; never emit more rows than
	// the terminal has.
	if len(lines) > m.height {
		lines = lines[:m.height]
	}
	v.SetContent(strings.Join(lines, "\n"))
	return v
}

func (m *Model) panes(g geom) []string {
	if g.bodyH <= 0 {
		return nil
	}
	sep := styleFaint.Render("│")
	var folders []string
	if g.folderW > 0 {
		folders = m.folderLines(g.folderW, g.bodyH)
	}
	list := m.listLines(g.listW, g.listH)
	var preview []string
	if g.previewH > 0 {
		preview = padLines(strings.Split(m.preview.View(), "\n"), g.previewW, g.previewH)
	}

	right := list
	if g.previewH > 0 && !g.previewOnRight {
		right = append(append(list, styleFaint.Render(strings.Repeat("─", g.listW))), preview...)
	}
	out := make([]string, g.bodyH)
	for i := range out {
		var b strings.Builder
		if g.folderW > 0 {
			b.WriteString(folders[i])
			b.WriteString(sep)
		}
		b.WriteString(right[i])
		if g.previewOnRight {
			b.WriteString(sep)
			b.WriteString(preview[i])
		}
		out[i] = b.String()
	}
	return out
}

func (m *Model) folderLines(w, h int) []string {
	top := clamp(m.folderCur-h/2, 0, max(len(m.folders)-h, 0))
	lines := make([]string, h)
	for i := range lines {
		idx := top + i
		if idx >= len(m.folders) {
			lines[i] = strings.Repeat(" ", w)
			continue
		}
		f := m.folders[idx]
		name := strings.Repeat("  ", f.depth) + clean(f.Name)
		count := ""
		if f.unread > 0 {
			count = fmt.Sprintf(" %d", f.unread)
		}
		text := fit(" "+ansi.Truncate(name, w-1-ansi.StringWidth(count), "…"), w-ansi.StringWidth(count)) + count
		style := lipgloss.NewStyle()
		switch {
		case idx == m.folderCur && m.focus == paneFolders:
			style = styleCursor
		case f.ID == m.folderID:
			style = styleAccent
		case !f.synced:
			style = styleFaint
		}
		if f.unread > 0 {
			style = style.Bold(true)
		}
		lines[i] = style.Render(text)
	}
	return lines
}

func (m *Model) listLines(w, h int) []string {
	lines := make([]string, h)
	if len(m.msgs) == 0 {
		msg := "No messages."
		if f, ok := m.currentFolder(); ok && !f.synced {
			msg = "This folder isn't synced. Add it to sync.folders in config.toml."
		}
		for i := range lines {
			lines[i] = strings.Repeat(" ", w)
		}
		if h > 1 {
			lines[1] = styleFaint.Render(fit("  "+msg, w))
		}
		return lines
	}

	showTo := false
	if f, ok := m.currentFolder(); ok {
		showTo = f.WellKnown == "sentitems" || f.WellKnown == "drafts"
	}
	fromW := clamp(w/4, 10, 24)
	now := time.Now()
	for i := range lines {
		idx := m.msgTop + i
		if idx >= len(m.msgs) {
			lines[i] = strings.Repeat(" ", w)
			continue
		}
		msg := m.msgs[idx]
		status := ' '
		if !msg.IsRead {
			status = 'N'
		}
		flag := ' '
		switch {
		case msg.Flagged:
			flag = '*'
		case msg.Importance == "high":
			flag = '!'
		}
		attach := ' '
		if msg.HasAttachments {
			attach = '+'
		}
		who := senderName(msg.From)
		if showTo {
			who = "To: " + firstRecipient(msg.To)
		}
		subject := clean(msg.Subject)
		if subject == "" {
			subject = "(no subject)"
		}
		row := fmt.Sprintf(" %c%c%c %6s  %s  %s", status, flag, attach,
			shortDate(msg.ReceivedAt, now), fit(clean(who), fromW), subject)
		row = fit(row, w)

		style := lipgloss.NewStyle()
		if !msg.IsRead {
			style = style.Bold(true)
		}
		if idx == m.msgCur {
			if m.focus == paneList {
				style = style.Inherit(styleCursor)
			} else {
				style = style.Inherit(styleCursorBg)
			}
		}
		lines[i] = style.Render(row)
	}
	return lines
}

func (m *Model) topBar() string {
	left := " zorp"
	if m.account.UPN != "" {
		left += "  " + m.account.UPN
	}
	if f, ok := m.currentFolder(); ok {
		left += fmt.Sprintf("  │  %s  %d messages, %d unread", clean(f.Name), len(m.msgs), countUnread(m.msgs))
	}
	if m.reading && len(m.msgs) > 0 {
		left += fmt.Sprintf("  │  message %d of %d", m.msgCur+1, len(m.msgs))
	}
	right := m.syncState() + " "
	gap := m.width - ansi.StringWidth(left) - ansi.StringWidth(right)
	if gap < 1 {
		return styleBar.Render(fit(left, m.width))
	}
	return styleBar.Render(left + strings.Repeat(" ", gap) + right)
}

func (m *Model) syncState() string {
	if !m.daemonUp {
		return "daemon not running"
	}
	switch m.status.State {
	case ipc.StateSyncing:
		return "syncing…"
	case ipc.StateOffline:
		return "offline"
	case ipc.StateNeedsLogin:
		return "login needed: press L"
	case ipc.StateStarting:
		return "starting…"
	}
	if m.status.LastSync == 0 {
		return "not synced yet"
	}
	return "synced " + time.Unix(m.status.LastSync, 0).Format("15:04")
}

func (m *Model) bottomBar() string {
	if m.errText != "" {
		return styleError.Render(fit(" "+m.errText, m.width))
	}
	if m.status.LastError != "" && m.status.State != ipc.StateSyncing {
		return styleError.Render(fit(" sync: "+m.status.LastError, m.width))
	}
	var hints string
	switch {
	case m.help:
		hints = "any key: close help"
	case m.reading:
		hints = "j/k scroll  space/- page  n/p next/prev  q back  ? help"
	case m.gPending:
		hints = "go to: i inbox  s sent  d drafts  a archive  g top"
	case m.focus == paneFolders:
		hints = "j/k move  ⏎ open folder  tab pane  b hide folders  ? help  q quit"
	case m.focus == panePreview:
		hints = "j/k scroll  space/- page  tab pane  w layout  ? help  q quit"
	default:
		hints = "j/k move  ⏎ open  tab pane  g i/s/d/a folder  w layout  ^r sync  ? help  q quit"
	}
	return styleFaint.Render(fit(" "+hints, m.width))
}

func helpLines() []string {
	return []string{
		"",
		"  zorp keys (read-only for now)",
		"",
		"  Anywhere",
		"    tab / shift+tab   next / previous pane",
		"    g i, g s, g d, g a   go to Inbox, Sent, Drafts, Archive",
		"    w                 preview: right, bottom, hidden",
		"    b                 show/hide folder pane",
		"    ctrl+r            sync now",
		"    L                 log in again (when the session expired)",
		"    ?                 this help",
		"    q                 back / quit",
		"",
		"  Message list",
		"    j/k, arrows       move",
		"    ctrl+d / ctrl+u   half page down / up",
		"    pgdn / pgup       page down / up",
		"    g g / G           first / last message",
		"    enter, v          open message full screen",
		"",
		"  Reading a message",
		"    j/k               scroll",
		"    space / -         page down / up",
		"    n / p             next / previous message",
		"    q, esc            back to the list",
	}
}

// fit truncates or pads s to exactly w cells.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(s, w, "…")
	if pad := w - ansi.StringWidth(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// padLines fits each line to w and returns exactly h lines.
func padLines(lines []string, w, h int) []string {
	out := make([]string, h)
	for i := range out {
		if i < len(lines) {
			out[i] = fit(lines[i], w)
		} else {
			out[i] = strings.Repeat(" ", w)
		}
	}
	return out
}

// invisible reports characters that are dropped from untrusted text:
// control characters (including the ESC that starts terminal escape
// sequences) and Unicode format characters (soft hyphens, zero-width joiners,
// bidi overrides), which terminals disagree about the width of and which
// marketing mail uses as invisible padding.
func invisible(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u034F'
}

// clean makes untrusted single-line text (subjects, names) safe to print.
// Newlines and tabs become spaces.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case invisible(r):
			return -1
		}
		return r
	}, s)
}

// cleanBody is clean for multi-line text: newlines are kept, tabs expanded,
// trailing space trimmed, and runs of blank lines (often left behind by
// invisible padding) collapsed to one.
func cleanBody(s string) string {
	s = strings.ReplaceAll(s, "\t", "    ")
	s = strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if invisible(r) {
			return -1
		}
		return r
	}, s)
	lines := strings.Split(s, "\n")
	out := lines[:0]
	blank := 0
	for _, l := range lines {
		l = strings.TrimRight(l, " \u00a0")
		if strings.TrimSpace(l) == "" {
			blank++
			if blank > 1 {
				continue
			}
			l = ""
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func shortDate(t, now time.Time) string {
	t = t.Local()
	switch {
	case t.Year() == now.Year() && t.YearDay() == now.YearDay():
		return t.Format("15:04")
	case t.Year() == now.Year():
		return t.Format("Jan 02")
	default:
		return t.Format("2006-01")
	}
}

func senderName(a store.Address) string {
	if a.Name != "" {
		return a.Name
	}
	if a.Addr != "" {
		return a.Addr
	}
	return "(unknown sender)"
}

func firstRecipient(as []store.Address) string {
	if len(as) == 0 {
		return "(no recipients)"
	}
	s := senderName(as[0])
	if len(as) > 1 {
		s += fmt.Sprintf(" +%d", len(as)-1)
	}
	return s
}

func formatAddr(a store.Address) string {
	switch {
	case a.Name != "" && a.Addr != "" && a.Name != a.Addr:
		return clean(a.Name) + " <" + clean(a.Addr) + ">"
	case a.Addr != "":
		return clean(a.Addr)
	default:
		return clean(a.Name)
	}
}

func formatAddrs(as []store.Address) string {
	parts := make([]string, 0, len(as))
	for _, a := range as {
		parts = append(parts, formatAddr(a))
	}
	return strings.Join(parts, ", ")
}

func countUnread(msgs []store.Message) int {
	n := 0
	for _, m := range msgs {
		if !m.IsRead {
			n++
		}
	}
	return n
}
