package tui

import (
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/tom/zorp/internal/ipc"
)

// picker is the "move to folder" chooser: type to filter, arrows to choose.
type picker struct {
	query   string
	cursor  int
	graphID string // message being moved
	matches []folderRow
}

func (m *Model) openPicker() {
	msg, ok := m.currentMessage()
	if !ok {
		return
	}
	m.picker = &picker{graphID: msg.GraphID}
	m.filterPicker()
}

// filterPicker keeps folders whose name contains the query's letters in
// order (a simple fuzzy match), excluding the current folder.
func (m *Model) filterPicker() {
	p := m.picker
	p.matches = p.matches[:0]
	q := strings.ToLower(p.query)
	for _, f := range m.folders {
		if f.ID != m.folderID && fuzzyMatch(strings.ToLower(f.Name), q) {
			p.matches = append(p.matches, f)
		}
	}
	p.cursor = clamp(p.cursor, 0, len(p.matches)-1)
}

func fuzzyMatch(s, q string) bool {
	for _, r := range q {
		i := strings.IndexRune(s, r)
		if i < 0 {
			return false
		}
		s = s[i+utf8.RuneLen(r):]
	}
	return true
}

func (m *Model) pickerKey(key string) tea.Cmd {
	p := m.picker
	switch key {
	case "esc", "ctrl+g":
		m.picker = nil
	case "enter":
		if p.cursor < len(p.matches) {
			dest := p.matches[p.cursor]
			m.picker = nil
			return mutateCmd(m.cfg, ipc.MutateParams{Op: ipc.OpMove, GraphIDs: []string{p.graphID}, FolderID: dest.ID})
		}
	case "up", "ctrl+p", "ctrl+k":
		p.cursor = clamp(p.cursor-1, 0, len(p.matches)-1)
	case "down", "ctrl+n", "ctrl+j", "tab":
		p.cursor = clamp(p.cursor+1, 0, len(p.matches)-1)
	case "backspace":
		if p.query != "" {
			_, size := utf8.DecodeLastRuneInString(p.query)
			p.query = p.query[:len(p.query)-size]
			m.filterPicker()
		}
	case "space":
		p.query += " "
		m.filterPicker()
	default:
		if utf8.RuneCountInString(key) == 1 {
			p.query += key
			m.filterPicker()
		}
	}
	return nil
}

func (m *Model) pickerLines(w, h int) []string {
	p := m.picker
	lines := []string{
		"",
		styleBold.Render("  Move to folder: ") + clean(p.query) + "█",
		styleFaint.Render("  type to filter · ↑/↓ choose · enter move · esc cancel"),
		"",
	}
	if m.status.SafeMode {
		lines = append(lines, styleError.Render("  Safe mode: only "+m.cfg.Safety.ScratchFolder+" is allowed"), "")
	}
	room := max(h-len(lines), 0)
	top := clamp(p.cursor-room+1, 0, max(len(p.matches)-room, 0))
	for i := top; i < len(p.matches) && len(lines) < h; i++ {
		f := p.matches[i]
		row := "    " + strings.Repeat("  ", f.depth) + clean(f.Name)
		if i == p.cursor {
			lines = append(lines, styleCursor.Render(fit(row, min(w, ansi.StringWidth(row)+4))))
		} else {
			lines = append(lines, row)
		}
	}
	if len(p.matches) == 0 {
		lines = append(lines, styleFaint.Render("    no matching folders"))
	}
	return padLines(lines, w, h)
}
