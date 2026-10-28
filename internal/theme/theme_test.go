package theme

import (
	"image/color"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func TestParseColor(t *testing.T) {
	for _, ok := range []string{"4", "200", "blue", "bright-magenta", "#89b4fa", " Cyan "} {
		if _, err := ParseColor(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"256", "-1", "bleu", "#12345", "rgb(1,2,3)"} {
		if _, err := ParseColor(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if c, _ := ParseColor("bright-blue"); c != lipgloss.ANSIColor(12) {
		t.Errorf("bright-blue = %v", c)
	}
}

func TestParseOSC4(t *testing.T) {
	i, c, ok := ParseOSC4("\x1b]4;5;rgb:ffff/8080/0000\x07")
	if !ok || i != 5 || c != (color.RGBA{255, 128, 0, 255}) {
		t.Errorf("got %d %v %v", i, c, ok)
	}
	if _, c, ok := ParseOSC4("\x1b]4;1;rgb:f/0/8\x1b\\"); !ok || c != (color.RGBA{255, 0, 136, 255}) {
		t.Errorf("short form: %v %v", c, ok)
	}
	for _, bad := range []string{"\x1b]4;16;rgb:0/0/0\x07", "\x1b]10;rgb:0/0/0\x07", "\x1b]4;3;#ffffff\x07", "\x1b]4;3;rgb:zz/0/0\x07"} {
		if _, _, ok := ParseOSC4(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestModes(t *testing.T) {
	render := func(s lipgloss.Style) string { return s.Render("x") }
	ansi := New(ModeAuto, false, nil, nil)
	if !strings.Contains(render(ansi.Selected), "38;5;5m") { // palette magenta
		t.Errorf("ANSI selected = %q", render(ansi.Selected))
	}
	none := New(ModeNone, true, nil, nil)
	if strings.Contains(render(none.Selected), "38;") || strings.Contains(render(none.Header), "38;") {
		t.Errorf("none mode has colour: %q %q", render(none.Selected), render(none.Header))
	}

	pal := &Palette{FG: color.RGBA{220, 220, 220, 255}, BG: color.RGBA{30, 30, 46, 255}}
	tinted := New(ModeAuto, true, pal, nil)
	if !strings.Contains(render(tinted.Cursor), "48;2;") || !strings.Contains(render(tinted.Selected), "48;2;") {
		t.Errorf("no 24-bit backgrounds: %q %q", render(tinted.Cursor), render(tinted.Selected))
	}
	if render(tinted.Cursor) == render(tinted.Selected) || render(tinted.Selected) == render(tinted.SelectedCursor) {
		t.Error("cursor, selected and selected cursor should differ")
	}
	// ui.color = "ansi" never tints; nor does an unknown palette.
	if strings.Contains(render(New(ModeANSI, true, pal, nil).Cursor), "48;2;") || strings.Contains(render(New(ModeAuto, true, &Palette{}, nil).Cursor), "48;2;") {
		t.Error("tinted without a palette or in ansi mode")
	}

	over := New(ModeAuto, false, nil, Overrides{"accent": "#ff0000", "cursor_bg": "236"})
	if !strings.Contains(render(over.Accent), "38;2;255;0;0") || !strings.Contains(render(over.Cursor), "48;5;236") {
		t.Errorf("overrides: %q %q", render(over.Accent), render(over.Cursor))
	}
}

func TestCheck(t *testing.T) {
	if err := Check(Overrides{"accent": "blue", "selected_bg": "#334455"}); err != nil {
		t.Error(err)
	}
	err := Check(Overrides{"acent": "blue", "link": "nope"})
	if err == nil || !strings.Contains(err.Error(), `unknown role "acent"`) || !strings.Contains(err.Error(), "link:") {
		t.Errorf("err = %v", err)
	}
}
