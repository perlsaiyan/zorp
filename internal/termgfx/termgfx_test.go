package termgfx

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestFromEnv(t *testing.T) {
	cases := []struct {
		mode  string
		env   map[string]string
		kitty bool
		known bool
	}{
		{"none", map[string]string{"TERM": "xterm-kitty"}, false, true},
		{"kitty", map[string]string{"TERM": "alacritty"}, true, true},
		{"auto", map[string]string{"TERM": "xterm-kitty"}, true, true},
		{"auto", map[string]string{"TERM": "xterm-ghostty"}, true, true},
		{"auto", map[string]string{"TERM": "xterm-256color", "TERM_PROGRAM": "ghostty"}, true, true},
		{"auto", map[string]string{"TERM": "alacritty"}, false, true},
		{"auto", map[string]string{"TERM": "xterm-256color", "TERM_PROGRAM": "WezTerm"}, false, true},
		{"auto", map[string]string{"TERM": "xterm-256color"}, false, false}, // ask the terminal
	}
	for _, c := range cases {
		s, known := FromEnv(c.mode, env(c.env))
		if s.Kitty != c.kitty || known != c.known {
			t.Errorf("%s %v: kitty=%v known=%v", c.mode, c.env, s.Kitty, known)
		}
	}

	defer func(f func(...string) string) { tmuxValue = f }(tmuxValue)
	for _, c := range []struct {
		client, pass string
		kitty        bool
	}{{"xterm-kitty", "on", true}, {"xterm-ghostty", "all", true}, {"xterm-kitty", "off", false}, {"alacritty", "on", false}} {
		tmuxValue = func(args ...string) string {
			if args[0] == "display-message" {
				return c.client
			}
			return c.pass
		}
		s, known := FromEnv("auto", env(map[string]string{"TMUX": "/tmp/tmux-1000/default,1,0", "TERM": "tmux-256color"}))
		if s.Kitty != c.kitty || !s.Tmux || !known {
			t.Errorf("tmux client=%s passthrough=%s: %+v", c.client, c.pass, s)
		}
	}
}

func TestFit(t *testing.T) {
	cell := CellSize{10, 20}
	for _, c := range []struct{ w, h, maxC, maxR, cols, rows int }{
		{100, 100, 80, 40, 10, 5},    // natural size: 10 cols, square → 5 rows of 20px
		{2000, 1000, 80, 40, 80, 20}, // limited by width
		{1000, 4000, 80, 40, 20, 40}, // limited by height
		{100, 100, 0, 40, 0, 0},
	} {
		if cols, rows := Fit(c.w, c.h, c.maxC, c.maxR, cell); cols != c.cols || rows != c.rows {
			t.Errorf("Fit(%d×%d in %d×%d) = %d×%d, want %d×%d", c.w, c.h, c.maxC, c.maxR, cols, rows, c.cols, c.rows)
		}
	}
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

var apcRe = regexp.MustCompile(`\x1b_G([^;\x1b]*)(?:;([^\x1b]*))?\x1b\\`)

func TestEncodeTransmitsChunkedPNG(t *testing.T) {
	im, err := Encode(7, testPNG(t, 1600, 800), 60, 30, CellSize{10, 20}, Support{Kitty: true})
	if err != nil {
		t.Fatal(err)
	}
	if im.Cols != 60 || im.Rows != 15 {
		t.Errorf("size %d×%d, want 60×15", im.Cols, im.Rows)
	}
	chunks := apcRe.FindAllStringSubmatch(im.Transmit, -1)
	if len(chunks) < 1 || strings.Join(apcRe.Split(im.Transmit, -1), "") != "" {
		t.Fatalf("transmit isn't only APC G sequences: %q…", im.Transmit[:min(80, len(im.Transmit))])
	}
	first := chunks[0][1]
	for _, want := range []string{"a=T", "f=100", "U=1", "q=2", "i=7", "c=60", "r=15"} {
		if !strings.Contains(first, want) {
			t.Errorf("first chunk options %q lack %s", first, want)
		}
	}
	var payload strings.Builder
	for i, c := range chunks {
		last := i == len(chunks)-1
		if strings.Contains(c[1], "m=0") != last {
			t.Errorf("chunk %d options %q", i, c[1])
		}
		if len(c[2]) > kitty.MaxChunkSize {
			t.Errorf("chunk %d is %d bytes", i, len(c[2]))
		}
		payload.WriteString(c[2])
	}
	data, err := base64.StdEncoding.DecodeString(payload.String())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width > 600 || cfg.Height > 300 {
		t.Errorf("sent %dx%d PNG (%v); should be scaled to the 600x300 px the cells cover", cfg.Width, cfg.Height, err)
	}
}

func TestEncodeInTmuxWrapsEscapes(t *testing.T) {
	im, err := Encode(3, testPNG(t, 64, 64), 20, 10, DefaultCell, Support{Kitty: true, Tmux: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(im.Transmit, "\x1bPtmux;\x1b\x1b_G") {
		t.Errorf("not wrapped for tmux: %q", im.Transmit[:30])
	}
}

func TestEncodeRejectsJunk(t *testing.T) {
	if _, err := Encode(1, testPNG(t, 1, 1), 80, 40, DefaultCell, Support{}); err != ErrTooSmall {
		t.Errorf("tracking pixel: %v", err)
	}
	if _, err := Encode(1, []byte("not an image"), 80, 40, DefaultCell, Support{}); err == nil {
		t.Error("garbage decoded")
	}
	if _, err := Encode(0, testPNG(t, 64, 64), 80, 40, DefaultCell, Support{}); err == nil {
		t.Error("id 0 accepted")
	}
}

func TestLines(t *testing.T) {
	im := &Image{ID: 9, Cols: 4, Rows: 3}
	lines := im.Lines()
	if len(lines) != 3 {
		t.Fatalf("%d lines", len(lines))
	}
	for r, l := range lines {
		if w := ansi.StringWidth(l); w != 4 {
			t.Errorf("line %d is %d wide", r, w)
		}
		if !strings.HasPrefix(l, "\x1b[38;5;9m") {
			t.Errorf("line %d colour: %q", r, l)
		}
		cells := []rune(ansi.Strip(l))
		if len(cells) != 12 || cells[0] != kitty.Placeholder || cells[1] != kitty.Diacritic(r) || cells[2] != kitty.Diacritic(0) || cells[11] != kitty.Diacritic(3) {
			t.Errorf("line %d cells: %U", r, cells)
		}
	}
}

func TestQuery(t *testing.T) {
	q := Query()
	if !strings.Contains(q, "\x1b_Gi=31,s=1,v=1,a=q,t=d,f=24;AAAA\x1b\\") || !strings.HasSuffix(q, "\x1b[c") {
		t.Errorf("query = %q", q)
	}
}
