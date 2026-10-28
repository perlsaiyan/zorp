package tui

import (
	"os"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/perlsaiyan/zorp/internal/render"
	"github.com/perlsaiyan/zorp/internal/theme"
)

// Colours (see package theme). The styles are package variables, set
// from the current theme, so every view uses the same roles.
var (
	styleBar, styleBold, styleFaint                    lipgloss.Style
	styleCursor, styleCursorBg                         lipgloss.Style // cursor row: focused, unfocused pane
	styleSelected, styleSelectedCursor, styleUnread    lipgloss.Style
	styleError, styleWarning, styleAccent, styleHeader lipgloss.Style
	renderStyles                                       render.Styles
)

func init() { applyTheme(theme.New(theme.ModeAuto, false, nil, nil)) }

func applyTheme(t *theme.Theme) {
	styleBar, styleBold, styleFaint = t.Bar, t.Bold, t.Faint
	styleCursor, styleCursorBg = t.Cursor, t.CursorDim
	styleSelected, styleSelectedCursor, styleUnread = t.Selected, t.SelectedCursor, t.Unread
	styleError, styleWarning, styleAccent, styleHeader = t.Error, t.Warning, t.Accent, t.Header
	renderStyles = render.Styles{Link: t.Link, Quote: t.Quote, Faint: t.Faint}
}

// colors is what the terminal told us about its colours.
type colors struct {
	trueColor bool
	asked     bool // asked the terminal whether it does 24-bit colour
	pal       theme.Palette
}

// colorMode is ui.color, defaulting to auto.
func (m *Model) colorMode() string {
	if m.cfg.UI.Color == "" {
		return theme.ModeAuto
	}
	return m.cfg.UI.Color
}

// wantTints reports whether to ask the terminal for its colours.
func (m *Model) wantTints() bool {
	switch m.colorMode() {
	case theme.ModeTint:
		return true
	case theme.ModeAuto:
		return m.colors.trueColor
	}
	return false
}

// rebuildTheme applies the theme for the current settings and what the
// terminal reported.
func (m *Model) rebuildTheme() {
	applyTheme(theme.New(m.colorMode(), m.wantTints(), &m.colors.pal, m.cfg.Theme))
	m.refreshPreview(false)
	if m.viewer != nil {
		m.layoutViewer()
	}
}

// colorEvent handles the colour profile and the terminal's colour reports.
func (m *Model) colorEvent(msg tea.Msg) (tea.Cmd, bool) {
	switch e := msg.(type) {
	case tea.ColorProfileMsg:
		m.colors.trueColor = e.Profile == colorprofile.TrueColor
		m.rebuildTheme()
		if !m.wantTints() {
			if m.colorMode() == theme.ModeAuto && !m.colors.asked {
				// Over SSH, COLORTERM rarely comes along; ask the terminal
				// itself. A yes arrives as another ColorProfileMsg.
				m.colors.asked = true
				return tea.Batch(tea.RequestCapability("RGB"), tea.RequestCapability("Tc")), true
			}
			return nil, true
		}
		cmds := []tea.Cmd{tea.RequestBackgroundColor, tea.RequestForegroundColor}
		// tmux answers palette queries with its own idea of the colours, if
		// at all; there the fallback hues are used.
		if os.Getenv("TMUX") == "" {
			cmds = append(cmds, tea.Raw(theme.PaletteQuery()))
		}
		return tea.Batch(cmds...), true
	case tea.BackgroundColorMsg:
		m.colors.pal.BG = e.Color
	case tea.ForegroundColorMsg:
		m.colors.pal.FG = e.Color
	case uv.UnknownOscEvent:
		i, c, ok := theme.ParseOSC4(string(e))
		if !ok {
			return nil, false
		}
		m.colors.pal.ANSI[i] = c
	default:
		return nil, false
	}
	m.rebuildTheme()
	return nil, true
}
