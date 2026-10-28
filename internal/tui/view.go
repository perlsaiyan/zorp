package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/render"
	"github.com/perlsaiyan/zorp/internal/store"
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
		if m.folderW > 0 {
			g.folderW = clamp(m.folderW, 10, m.width/2)
		}
		rest -= g.folderW + 1
	}
	g.listW, g.listH = rest, g.bodyH
	switch m.layout {
	case previewRight:
		if rest >= 80 {
			share := 0.55
			if m.listShare > 0 {
				share = m.listShare
			}
			g.listW = clamp(int(float64(rest)*share), 20, rest-21)
			g.previewW, g.previewH = rest-g.listW-1, g.bodyH
			g.previewOnRight = true
		}
	case previewBottom:
		listH := max(g.bodyH*2/5, 3)
		if m.listHShare > 0 {
			listH = clamp(int(float64(g.bodyH)*m.listHShare), 3, max(g.bodyH-4, 3))
		}
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
	if line := m.attachedLine(); line != "" {
		field("Attached", line+styleFaint.Render("  (V open, s save)"))
	}
	b.WriteString(styleFaint.Render(strings.Repeat("─", w)))
	b.WriteString("\n")

	switch m.bodyState {
	case bodyReady:
		opts := render.Options{ShowQuoted: m.showQuoted}
		if m.gfx.support.Kitty {
			opts.Image = m.imageLines
		}
		lines := m.doc.Render(w, opts)
		if len(lines) == 0 {
			lines = []string{styleFaint.Render("(no text in this message)")}
		}
		lines = append(lines, m.attachedImageLines(w)...)
		b.WriteString(strings.Join(lines, "\n"))
	case bodyFailed:
		b.WriteString(ansi.Wrap(cleanBody(msg.BodyPreview)+"\n\n"+styleError.Render("Couldn't load the full message: "+m.bodyErr), w, ""))
	default:
		b.WriteString(ansi.Wrap(cleanBody(msg.BodyPreview)+"\n\n"+styleFaint.Render("Loading…"), w, ""))
	}
	return b.String()
}

func (m *Model) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = true
	if m.cfg.UI.Mouse {
		v.MouseMode = tea.MouseModeCellMotion
	}
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
		lines = append(lines, padLines(m.helpLines(), m.width, g.bodyH)...)
	case m.picker != nil:
		lines = append(lines, m.pickerLines(m.width, g.bodyH)...)
	case m.viewer != nil:
		lines = append(lines, padLines(strings.Split(m.viewer.vp.View(), "\n"), m.width, g.bodyH)...)
	case m.composing != nil:
		lines = append(lines, m.composeLines(m.width, g.bodyH)...)
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
		if s := m.search; s != nil {
			switch {
			case s.running && s.kql != "":
				msg = "Searching the server…"
			case s.running:
				msg = "Searching…"
			case s.kql != "":
				msg = "No matches on the server either."
			default:
				msg = "No matches. Start the search with ? to search the server (in: and is: work only locally)."
			}
		}
		if f, ok := m.currentFolder(); ok && f.synced && m.cfg.Sync.InitialDays > 0 && m.search == nil {
			msg = "No messages in the last " + strconv.Itoa(m.cfg.Sync.InitialDays) + " days (sync.initial_days)."
		}
		if f, ok := m.currentFolder(); ok && !f.synced && m.search == nil {
			msg = "This folder isn't synced. Press + to sync it."
			if f.SyncOverride != nil && *f.SyncOverride {
				msg = "Syncing this folder…"
			}
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
	if f, ok := m.currentFolder(); ok && m.search == nil {
		showTo = f.WellKnown == "sentitems" || f.WellKnown == "drafts"
	}
	fromW := clamp(w/4, 10, 24)
	folderW := 0
	if m.search != nil {
		folderW = clamp(w/8, 6, 14)
	}
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
		where := ""
		if folderW > 0 {
			where = fit(clean(m.folderName(msg.FolderID)), folderW) + "  "
		}
		mark := ' '
		if m.selected[msg.GraphID] {
			mark = '✓'
		}
		row := fmt.Sprintf("%c%c%c%c %6s  %s%s  %s", mark, status, flag, attach,
			shortDate(msg.ReceivedAt, now), where, fit(clean(who), fromW), subject)
		row = fit(row, w)

		style := lipgloss.NewStyle()
		if !msg.IsRead {
			style = style.Bold(true)
		}
		if mark != ' ' {
			style = style.Inherit(styleAccent)
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
	if s := m.search; s != nil {
		where := "search"
		if s.kql != "" {
			where = "server search"
		}
		left += "  │  " + where + ": " + clean(strings.TrimPrefix(s.text, "?"))
		if !s.running {
			n := strconv.Itoa(len(m.msgs))
			if s.more {
				n = "newest " + n
			}
			left += "  " + n + " found"
			if s.note != "" {
				left += " (" + s.note + ")"
			}
		}
	} else if f, ok := m.currentFolder(); ok {
		left += fmt.Sprintf("  │  %s  %d messages, %d unread", clean(f.Name), len(m.msgs), countUnread(m.msgs))
	}
	if m.reading && len(m.msgs) > 0 {
		left += fmt.Sprintf("  │  message %d of %d", m.msgCur+1, len(m.msgs))
	}
	if n := len(m.selected); n > 0 {
		left += fmt.Sprintf("  │  %d selected", n)
	}
	right := m.syncState() + " "
	if m.status.Pending > 0 {
		right = fmt.Sprintf("%d pending  ", m.status.Pending) + right
	}
	if len(m.status.Guards) > 0 {
		right = "guarded: " + strings.Join(m.status.Guards, " ") + "  " + right
	}
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
	if m.prompt != nil {
		return fit(m.prompt.View(), m.width)
	}
	if m.confirm != nil {
		return styleError.Render(fit(" "+m.confirm.prompt, m.width))
	}
	if m.errText != "" {
		return styleError.Render(fit(" "+m.errText, m.width))
	}
	if m.notice != "" {
		return styleAccent.Render(fit(" "+m.notice, m.width))
	}
	if m.status.LastError != "" && m.status.State != ipc.StateSyncing {
		return styleError.Render(fit(" sync: "+m.status.LastError, m.width))
	}
	var hints string
	switch {
	case m.help:
		hints = "any key: close help"
	case m.picker != nil:
		hints = "type to filter  ↑/↓ choose  enter select  esc cancel"
	case m.viewer != nil:
		hints = "j/k scroll  space/- page  s save  o open in its app  q back"
	case m.composing != nil:
		hints = "y send  d save to Drafts  e edit again  a attach a file  q discard"
	case m.reading:
		hints = "j/k scroll  space/- page  n/p next/prev  r reply  R all  F fwd  z quotes  O links  V attachments  s save att  u unread  f flag  d " + m.dLabel() + "  q back"
	case m.gPending:
		hints = "go to: i inbox  s sent  d drafts  a archive  g top"
	case len(m.selected) > 0 && m.focus == paneList:
		hints = strconv.Itoa(len(m.selected)) + " selected:  a archive  d " + m.dLabel() + "  m move  u read/unread  f flag  space toggle  * all  esc clear"
	case m.focus == paneFolders:
		hints = "j/k move  ⏎ open folder  + sync  - stop syncing  tab pane  b hide folders  ? help  q quit"
	case m.focus == panePreview:
		hints = "j/k scroll  space/- page  tab pane  w layout  ? help  q quit"
	case m.search != nil:
		hints = "j/k move  ⏎ open  space select  * select all  / edit search  esc back to folder  d " + m.dLabel() + "  m move  ? help"
	default:
		hints = "j/k move  ⏎ open  / search  c compose  r reply  R all  F fwd  u unread  f flag  d " + m.dLabel() + "  m move  s save att  ? help  q quit"
		if f, ok := m.currentFolder(); ok && f.WellKnown == "drafts" && m.search == nil {
			hints = "j/k move  ⏎ open  e edit draft  c compose  / search  d " + m.dLabel() + "  ? help  q quit"
		}
	}
	return styleFaint.Render(fit(" "+hints, m.width))
}

func (m *Model) dLabel() string {
	if m.cfg.Keys.DeleteArchives {
		return "archive"
	}
	return "delete"
}

func (m *Model) helpLines() []string {
	return []string{
		"",
		"  zorp keys",
		"",
		"  Anywhere",
		"    tab / shift+tab   next / previous pane",
		"    g i, g s, g d, g a   go to Inbox, Sent, Drafts, Archive",
		"    /                 search (esc in the results goes back to the folder)",
		"    c                 write a new message",
		"    w                 preview: right, bottom, hidden",
		"    b                 show/hide folder pane",
		"    + / -             start / stop syncing the folder",
		"    mouse             click to select, wheel to scroll, drag dividers to resize",
		"                      (Shift+drag selects text; ui.mouse = false turns it off)",
		"    ctrl+r            sync now",
		"    L                 log in again (when the session expired)",
		"    ?                 this help",
		"    q                 back / quit",
		"",
		"  Messages (list, preview or reader)",
		"    u                 toggle read / unread",
		"    f                 toggle flag",
		"    d                 " + m.dLabel() + "  (keys.delete_archives in config)",
		"    X                 delete (moves to Deleted Items), after confirming",
		"    a                 archive",
		"    m                 move to folder (type to filter)",
		"    r / R             reply / reply to all",
		"    F                 forward",
		"    e                 edit a draft (in Drafts)",
		"    V                 open an attachment: text, PDFs (with pdftotext), attached",
		"                      emails and images show here; others open in their app,",
		"                      or are saved when there's no desktop (over SSH)",
		"    s                 save attachments (to ui.save_dir; attached emails as .eml)",
		"    z                 show/hide quoted replies and signature",
		"    O                 pick a link to open in the browser",
		"    o                 open the message in the browser (remote images blocked)",
		"",
		"  Message list",
		"    j/k, arrows       move",
		"    ctrl+d / ctrl+u   half page down / up",
		"    pgdn / pgup       page down / up",
		"    g g / G           first / last message",
		"    enter, v          open message full screen (marks it read)",
		"    space             select / unselect, and move down",
		"    *                 select all listed (again: clear); esc clears",
		"                      a, d, X, m, u, f then act on all selected",
		"",
		"  Search  (all folders; words match prefixes, all must match)",
		`    budget "q3 report"   words and phrases in subject, sender or body`,
		"    -word             leave out messages with this word",
		"    from: to: subject:   e.g. from:alice  to:bob@example.com  subject:invoice",
		"    in:archive        one folder        is:unread  is:read  is:flagged",
		"    has:attachment    after:2026-01-31  before:7d  (d, w, m, y)",
		"    ?query            search the server (whole mailbox, Outlook syntax);",
		"                      also tried automatically when nothing cached matches",
		"",
		"  Writing  (opens $EDITOR; headers on top, a blank line, then the message)",
		"    Attach: ~/file.pdf   one line per file; delete an Attached: line to drop it",
		"    after the editor: y send, d save to Drafts, e edit again, a attach a file",
		"                      (newest first; type to filter), q discard",
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

// clean makes untrusted single-line text (subjects, names) safe to print.
func clean(s string) string { return render.Sanitize(s, false) }

// cleanBody is clean for multi-line text such as body previews.
func cleanBody(s string) string { return strings.TrimSpace(render.Sanitize(s, true)) }

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
