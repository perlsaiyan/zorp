package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Buttons in the preview: a row of them under the headers, clicked with
// the mouse. Each does what a key already does.

type button struct {
	label  string
	active bool // the current state, e.g. the answer already given
	press  func() tea.Cmd
	x0, x1 int // columns it covers, [x0, x1)
}

// buttonRow is a row of buttons on one line of the preview's content.
type buttonRow struct {
	line    int
	buttons []button
}

const buttonGap = 1

// layoutButtons places buttons side by side on content line line.
func layoutButtons(line int, bs []button) buttonRow {
	x := 0
	for i := range bs {
		w := ansi.StringWidth(bs[i].text())
		bs[i].x0, bs[i].x1 = x, x+w
		x += w + buttonGap
	}
	return buttonRow{line: line, buttons: bs}
}

func (b button) text() string {
	if b.active {
		return " ✓ " + b.label + " "
	}
	return " " + b.label + " "
}

func (r buttonRow) render() string {
	parts := make([]string, len(r.buttons))
	for i, b := range r.buttons {
		style := styleCursorBg
		if b.active {
			style = styleCursor
		}
		parts[i] = style.Render(b.text())
	}
	return strings.Join(parts, strings.Repeat(" ", buttonGap))
}

// at is the button at column x of content line line.
func (r buttonRow) at(line, x int) (button, bool) {
	if line != r.line {
		return button{}, false
	}
	for _, b := range r.buttons {
		if x >= b.x0 && x < b.x1 {
			return b, true
		}
	}
	return button{}, false
}

// clickButton presses the preview button at screen cell x, y, if there
// is one there.
func (m *Model) clickButton(x, y int) (tea.Cmd, bool) {
	if len(m.buttons.buttons) == 0 || m.help || m.picker != nil || m.confirm != nil || m.prompt != nil || m.composing != nil || m.planning != nil {
		return nil, false
	}
	g := m.geom()
	var x0, y0 int
	switch {
	case m.reading:
		x0, y0 = 0, 1
	case g.previewH == 0:
		return nil, false
	case g.previewOnRight:
		x0, y0 = g.listW+1, 1
		if g.folderW > 0 {
			x0 += g.folderW + 1
		}
		if x < x0 {
			return nil, false
		}
	default:
		x0, y0 = 0, 1+g.listH+1
		if g.folderW > 0 {
			x0 = g.folderW + 1
		}
		if y < y0 || x < x0 {
			return nil, false
		}
	}
	if y < y0 || y >= y0+m.preview.Height() {
		return nil, false
	}
	b, ok := m.buttons.at(y-y0+m.preview.YOffset(), x-x0)
	if !ok {
		return nil, false
	}
	m.focus = panePreview
	return b.press(), true
}
