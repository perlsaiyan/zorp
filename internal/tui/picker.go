package tui

import (
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/render"
)

type pickerKind int

const (
	pickMove pickerKind = iota
	pickLink
	pickAttach
)

// choice is one row in a picker.
type choice struct {
	label    string
	detail   string // shown faint after the label, and matched too
	indent   int
	folderID int64
	url      string
	attIDs   []string
}

// picker is a type-to-filter chooser, used for "move to folder" and
// "open link".
type picker struct {
	kind    pickerKind
	title   string
	query   string
	cursor  int
	graphID string // message being moved
	all     []choice
	matches []choice
}

func (m *Model) openMovePicker() {
	msg, ok := m.currentMessage()
	if !ok {
		return
	}
	p := &picker{kind: pickMove, title: "Move to folder", graphID: msg.GraphID}
	for _, f := range m.folders {
		if f.ID != m.folderID {
			p.all = append(p.all, choice{label: clean(f.Name), indent: f.depth, folderID: f.ID})
		}
	}
	m.picker = p
	m.filterPicker()
}

func (m *Model) openLinkPicker() {
	if m.doc == nil || len(m.doc.Links) == 0 {
		m.errText = "no links in this message"
		return
	}
	p := &picker{kind: pickLink, title: "Open link"}
	for i, l := range m.doc.Links {
		label := clean(l.Text)
		if label == "" {
			label = "(no text)"
		}
		p.all = append(p.all, choice{
			label:  "[" + strconv.Itoa(i+1) + "] " + label,
			detail: clean(render.DisplayURL(l.URL)),
			url:    l.URL,
		})
	}
	m.picker = p
	m.filterPicker()
}

// filterPicker keeps choices whose label or detail contains the query's
// letters in order (a simple fuzzy match).
func (m *Model) filterPicker() {
	p := m.picker
	p.matches = p.matches[:0]
	q := strings.ToLower(p.query)
	for _, c := range p.all {
		if fuzzyMatch(strings.ToLower(c.label+" "+c.detail), q) {
			p.matches = append(p.matches, c)
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
			c := p.matches[p.cursor]
			m.picker = nil
			switch p.kind {
			case pickLink:
				return m.openURL(c.url)
			case pickAttach:
				m.notice = "Saving…"
				return saveAttachmentsCmd(m.cfg, p.graphID, c.attIDs)
			}
			return mutateCmd(m.cfg, ipc.MutateParams{Op: ipc.OpMove, GraphIDs: []string{p.graphID}, FolderID: c.folderID})
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
	action := map[pickerKind]string{pickMove: "move", pickLink: "open", pickAttach: "save"}[p.kind]
	lines := []string{
		"",
		styleBold.Render("  "+p.title+": ") + clean(p.query) + "█",
		styleFaint.Render("  type to filter · ↑/↓ choose · enter " + action + " · esc cancel"),
		"",
	}
	if p.kind == pickMove && slices.Contains(m.status.Guards, config.GuardMove) {
		lines = append(lines, styleError.Render("  Guardrail: only "+m.cfg.Safety.ScratchFolder+" is allowed (safety.allow_move)"), "")
	}
	room := max(h-len(lines), 0)
	top := clamp(p.cursor-room+1, 0, max(len(p.matches)-room, 0))
	for i := top; i < len(p.matches) && len(lines) < h; i++ {
		c := p.matches[i]
		row := "    " + strings.Repeat("  ", c.indent) + c.label
		if i == p.cursor {
			row = styleCursor.Render(fit(row, min(w, ansi.StringWidth(row)+2)))
		}
		if c.detail != "" {
			row += styleFaint.Render("  " + c.detail)
		}
		lines = append(lines, row)
	}
	if len(p.matches) == 0 {
		lines = append(lines, styleFaint.Render("    nothing matches"))
	}
	return padLines(lines, w, h)
}
