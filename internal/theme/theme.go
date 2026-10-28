// Package theme gives zorp's colours names by what they're for, and builds
// them from the terminal.
//
// By default every colour is one of the terminal's 16 ANSI colours, so
// zorp looks native in any theme, through SSH and tmux. In a 24-bit
// terminal that reports its background, foreground and palette, the
// cursor, selection and bars become soft background tints mixed from
// those colours instead. Config can override any role.
package theme

import (
	"fmt"
	"image/color"
	"slices"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	colorful "github.com/lucasb-eyer/go-colorful"
)

// Theme is a style per role.
type Theme struct {
	Bar            lipgloss.Style // top and bottom bars
	Cursor         lipgloss.Style // cursor row in the focused pane
	CursorDim      lipgloss.Style // cursor row in an unfocused pane
	Selected       lipgloss.Style // selected messages
	SelectedCursor lipgloss.Style // the cursor on a selected message
	Unread         lipgloss.Style
	Accent         lipgloss.Style // current folder, notices
	Header         lipgloss.Style // From:, To:… labels
	Link           lipgloss.Style
	Quote          [3]lipgloss.Style // quote bars by depth (cycling)
	Warning        lipgloss.Style
	Error          lipgloss.Style
	Faint          lipgloss.Style
	Bold           lipgloss.Style
}

// Mode is ui.color: how much colour to use.
const (
	ModeAuto = "auto" // ANSI colours, plus tints in a 24-bit terminal that reports its colours
	ModeTint = "tint" // tints even if the terminal doesn't claim 24-bit colour
	ModeANSI = "ansi" // ANSI colours only
	ModeNone = "none" // no colour: bold, underline and reverse only
)

// Palette is what the terminal reported. Nil colours are unknown.
type Palette struct {
	FG, BG color.Color
	ANSI   [16]color.Color
}

// Known reports whether there's enough to mix tints from.
func (p *Palette) Known() bool { return p != nil && p.FG != nil && p.BG != nil }

// Fallback hues for palette entries the terminal didn't report.
var fallbackANSI = [16]string{
	"#000000", "#cd3131", "#0dbc79", "#e5e510", "#2472c8", "#bc3fbc", "#11a8cd", "#e5e5e5",
	"#666666", "#f14c4c", "#23d18b", "#f5f543", "#3b8eea", "#d670d6", "#29b8db", "#ffffff",
}

func (p *Palette) ansi(i int) color.Color {
	if p != nil && p.ANSI[i] != nil {
		return p.ANSI[i]
	}
	return lipgloss.Color(fallbackANSI[i])
}

// Overrides are colours from config by role name (see Roles), as ANSI
// numbers ("4"), names ("blue", "bright-blue") or hex ("#89b4fa").
type Overrides map[string]string

// Roles are the role names Overrides accept. The *_bg ones set
// backgrounds, the rest text colours.
var Roles = []string{"accent", "header", "link", "warning", "error", "selected", "quote", "bar_bg", "cursor_bg", "selected_bg"}

// New builds the theme. pal is nil until the terminal has reported its
// colours; tints are only used in ModeAuto and ModeTint with a known
// palette (the caller decides whether 24-bit colour is available).
func New(mode string, tint bool, pal *Palette, over Overrides) *Theme {
	base := lipgloss.NewStyle()
	t := &Theme{
		Bar:       base.Reverse(true),
		Cursor:    base.Reverse(true),
		CursorDim: base.Underline(true),
		Unread:    base.Bold(true),
		Faint:     base.Faint(true),
		Bold:      base.Bold(true),
		Warning:   base.Bold(true),
		Error:     base.Bold(true),
		Accent:    base.Bold(true),
		Link:      base.Underline(true),
		Selected:  base.Bold(true),
	}
	for i := range t.Quote {
		t.Quote[i] = base.Faint(true)
	}
	if mode == ModeNone {
		t.SelectedCursor = t.Cursor.Bold(true)
		return t
	}

	ansi := func(i int) color.Color { return lipgloss.ANSIColor(i) }
	t.Accent = t.Accent.Foreground(ansi(4))
	t.Header = base.Foreground(ansi(4))
	t.Link = t.Link.Foreground(ansi(6))
	t.Warning = t.Warning.Foreground(ansi(3))
	t.Error = t.Error.Foreground(ansi(1))
	t.Selected = t.Selected.Foreground(ansi(5))
	t.Quote = [3]lipgloss.Style{base.Foreground(ansi(4)), base.Foreground(ansi(2)), base.Foreground(ansi(5))}
	t.SelectedCursor = t.Cursor.Foreground(ansi(5)).Bold(true)

	if tint && mode != ModeANSI && pal.Known() {
		bg, fg := pal.BG, pal.FG
		blue, magenta := pal.ansi(4), pal.ansi(5)
		t.Bar = base.Background(mix(bg, fg, 0.14)).Foreground(fg)
		t.Cursor = base.Background(mix(bg, blue, 0.38)).Foreground(fg)
		t.CursorDim = base.Background(mix(bg, fg, 0.10))
		t.Selected = base.Background(mix(bg, magenta, 0.20)).Foreground(fg)
		t.SelectedCursor = base.Background(mix(bg, magenta, 0.45)).Foreground(fg).Bold(true)
		// Quote bars a little quieter than the text.
		for i, c := range []color.Color{pal.ansi(4), pal.ansi(2), pal.ansi(5)} {
			t.Quote[i] = base.Foreground(mix(bg, c, 0.7))
		}
	}

	for role, value := range over {
		c, err := ParseColor(value)
		if err != nil {
			continue // Validate reports it
		}
		switch role {
		case "accent":
			t.Accent = t.Accent.Foreground(c)
		case "header":
			t.Header = t.Header.Foreground(c)
		case "link":
			t.Link = t.Link.Foreground(c)
		case "warning":
			t.Warning = t.Warning.Foreground(c)
		case "error":
			t.Error = t.Error.Foreground(c)
		case "selected":
			t.Selected = t.Selected.Foreground(c)
		case "quote":
			for i := range t.Quote {
				t.Quote[i] = t.Quote[i].Foreground(c)
			}
		case "bar_bg":
			t.Bar = t.Bar.Reverse(false).Background(c)
		case "cursor_bg":
			t.Cursor = t.Cursor.Reverse(false).Background(c)
		case "selected_bg":
			t.Selected = t.Selected.Background(c)
			t.SelectedCursor = t.SelectedCursor.Reverse(false).Background(mixOr(c, t.Cursor))
		}
	}
	return t
}

// mixOr darkens or lightens c toward the cursor's background, if it has one.
func mixOr(c color.Color, cursor lipgloss.Style) color.Color {
	if _, none := cursor.GetBackground().(lipgloss.NoColor); none {
		return c
	}
	return mix(c, cursor.GetBackground(), 0.5)
}

// mix blends a toward b by t (0 is a, 1 is b), in a perceptual space.
func mix(a, b color.Color, t float64) color.Color {
	ca, ok1 := colorful.MakeColor(a)
	cb, ok2 := colorful.MakeColor(b)
	if !ok1 || !ok2 {
		return b
	}
	return ca.BlendLab(cb, t).Clamped()
}

var names = map[string]int{
	"black": 0, "red": 1, "green": 2, "yellow": 3, "blue": 4, "magenta": 5, "cyan": 6, "white": 7,
}

// Check reports overrides with an unknown role or a colour that doesn't parse.
func Check(over Overrides) error {
	var errs []string
	for role, value := range over {
		if !slices.Contains(Roles, role) {
			errs = append(errs, fmt.Sprintf("unknown role %q (roles: %s)", role, strings.Join(Roles, ", ")))
		} else if _, err := ParseColor(value); err != nil {
			errs = append(errs, role+": "+err.Error())
		}
	}
	if len(errs) == 0 {
		return nil
	}
	slices.Sort(errs)
	return fmt.Errorf("[theme] %s", strings.Join(errs, "; "))
}

// ParseColor reads a config colour: an ANSI number 0–255, a name ("blue",
// "bright-blue"), or "#rrggbb".
func ParseColor(s string) (color.Color, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 || n > 255 {
			return nil, fmt.Errorf("colour %d isn't 0–255", n)
		}
		if n < 16 {
			return lipgloss.ANSIColor(n), nil
		}
		return lipgloss.Color(s), nil
	}
	if strings.HasPrefix(s, "#") {
		c, err := colorful.Hex(s)
		if err != nil {
			return nil, fmt.Errorf("colour %q isn't #rrggbb", s)
		}
		return c, nil
	}
	bright := strings.HasPrefix(s, "bright-") || strings.HasPrefix(s, "bright ")
	if i, ok := names[strings.TrimPrefix(strings.TrimPrefix(s, "bright-"), "bright ")]; ok {
		if bright {
			i += 8
		}
		return lipgloss.ANSIColor(i), nil
	}
	return nil, fmt.Errorf("colour %q isn't a number, a name or #rrggbb", s)
}

// ParseOSC4 reads a terminal's palette report ("4;N;rgb:rrrr/gggg/bbbb"),
// as found in an unknown-OSC event.
func ParseOSC4(seq string) (index int, c color.Color, ok bool) {
	i := strings.Index(seq, "]4;")
	if i < 0 {
		return 0, nil, false
	}
	rest := seq[i+3:]
	n, spec, found := strings.Cut(rest, ";")
	if !found {
		return 0, nil, false
	}
	index, err := strconv.Atoi(n)
	if err != nil || index < 0 || index > 15 {
		return 0, nil, false
	}
	spec = strings.TrimRight(spec, "\x07\x1b\\")
	c = parseXColor(spec)
	return index, c, c != nil
}

// parseXColor reads "rgb:r/g/b" with 1–4 hex digits per channel.
func parseXColor(s string) color.Color {
	s, ok := strings.CutPrefix(s, "rgb:")
	if !ok {
		return nil
	}
	parts := strings.Split(s, "/")
	if len(parts) != 3 {
		return nil
	}
	var v [3]uint8
	for i, p := range parts {
		if len(p) < 1 || len(p) > 4 {
			return nil
		}
		n, err := strconv.ParseUint(p, 16, 16)
		if err != nil {
			return nil
		}
		max := uint64(1)<<(4*len(p)) - 1
		v[i] = uint8(n * 255 / max)
	}
	return color.RGBA{v[0], v[1], v[2], 255}
}

// PaletteQuery asks the terminal for palette entries 0–15 (OSC 4).
func PaletteQuery() string {
	var b strings.Builder
	for i := range 16 {
		fmt.Fprintf(&b, "\x1b]4;%d;?\x07", i)
	}
	return b.String()
}
