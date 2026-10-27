// Package termgfx shows images in the terminal with the Kitty graphics
// protocol (kitty, Ghostty), using Unicode placeholders.
//
// An image is transmitted once, out of band, as a "virtual placement" of
// a given size in cells. It is then displayed by printing placeholder
// characters (U+10EEEE plus diacritics giving the row and column, in a
// foreground colour giving the image ID). Those are ordinary text to Bubble
// Tea's renderer, so redraws and scrolling can't corrupt the image, and it
// works through tmux (with allow-passthrough).
package termgfx

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // decoders for image.Decode
	_ "image/jpeg"
	"image/png"
	"os/exec"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
	"golang.org/x/image/draw"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

// Support is what the terminal can do, as far as zorp is concerned.
type Support struct {
	Kitty bool // Kitty graphics with Unicode placeholders
	Tmux  bool // inside tmux: escapes must be wrapped for passthrough
}

// FromEnv decides support from the config setting ("auto", "kitty" or
// "none") and the environment, without talking to the terminal. known is
// false when only asking the terminal can tell (see Query).
func FromEnv(mode string, getenv func(string) string) (s Support, known bool) {
	s.Tmux = getenv("TMUX") != ""
	switch mode {
	case "none":
		return s, true
	case "kitty":
		s.Kitty = true
		return s, true
	}
	term, prog := getenv("TERM"), strings.ToLower(getenv("TERM_PROGRAM"))
	if s.Tmux {
		// tmux answers queries itself, so ask it which terminal the client
		// is on, and whether it lets the image escapes through.
		client := tmuxValue("display-message", "-p", "#{client_termname}")
		pass := tmuxValue("show-options", "-gv", "allow-passthrough")
		s.Kitty = isKittyTerm(client) && (pass == "on" || pass == "all")
		return s, true
	}
	switch {
	case isKittyTerm(term), getenv("KITTY_WINDOW_ID") != "", getenv("GHOSTTY_RESOURCES_DIR") != "", prog == "ghostty":
		s.Kitty = true
		return s, true
	case strings.Contains(term, "alacritty"), prog == "wezterm", prog == "apple_terminal", prog == "vscode", term == "linux":
		// No Kitty graphics, or (WezTerm) no Unicode placeholders.
		return s, true
	}
	return s, false
}

func isKittyTerm(term string) bool {
	return strings.Contains(term, "kitty") || strings.Contains(term, "ghostty")
}

var tmuxValue = func(args ...string) string {
	out, err := exec.Command("tmux", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// QueryID is the image ID used by Query; a terminal with Kitty graphics
// answers with an APC G reply carrying it.
const QueryID = 31

// Query asks the terminal whether it supports Kitty graphics, then for its
// cell size in pixels, then for its primary device attributes. Every
// terminal answers the last, so its reply marks the end of the answers.
func Query() string {
	return ansi.KittyGraphics([]byte("AAAA"), "i="+strconv.Itoa(QueryID), "s=1", "v=1", "a=q", "t=d", "f=24") +
		"\x1b[16t" + ansi.RequestPrimaryDeviceAttributes
}

// CellSize is a terminal cell's size in pixels.
type CellSize struct{ W, H int }

// DefaultCell is assumed until the terminal reports its cell size.
var DefaultCell = CellSize{W: 8, H: 16}

// Image is an image ready to display: Transmit is sent to the terminal
// once, then Lines prints it.
type Image struct {
	ID         int // 1..255, the placeholders' foreground colour
	Cols, Rows int
	Transmit   string
}

// Limits on images shown.
const (
	MaxBytes  = 10 << 20 // larger images aren't fetched
	minPixels = 16       // smaller ones are spacers and tracking pixels
	maxPixels = 2000     // images are scaled down to at most this before sending
)

// ErrTooSmall marks images not worth showing (spacers, tracking pixels).
var ErrTooSmall = errors.New("image too small to show")

// Encode decodes data (PNG, JPEG, GIF, WebP, BMP) and prepares it to show
// within maxCols × maxRows cells, keeping its aspect ratio and never
// enlarging it beyond its natural size.
func Encode(id int, data []byte, maxCols, maxRows int, cell CellSize, s Support) (*Image, error) {
	if id < 1 || id > 255 {
		return nil, fmt.Errorf("image id %d out of range", id)
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w < minPixels || h < minPixels {
		return nil, ErrTooSmall
	}
	if cell.W <= 0 || cell.H <= 0 {
		cell = DefaultCell
	}
	cols, rows := Fit(w, h, maxCols, maxRows, cell)
	if cols < 1 || rows < 1 {
		return nil, ErrTooSmall
	}

	// Scale to the pixels the cells cover (capped), so we don't send more
	// than the terminal will show.
	tw, th := min(cols*cell.W, maxPixels), min(rows*cell.H, maxPixels)
	if tw < w || th < h {
		scale := min(float64(tw)/float64(w), float64(th)/float64(h))
		dw, dh := max(int(float64(w)*scale), 1), max(int(float64(h)*scale), 1)
		dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
		draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
		src = dst
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		return nil, err
	}
	return &Image{ID: id, Cols: cols, Rows: rows, Transmit: transmit(id, cols, rows, buf.Bytes(), s)}, nil
}

// Fit is the size in cells to show a w×h pixel image in, within the
// limits, keeping its aspect ratio.
func Fit(w, h, maxCols, maxRows int, cell CellSize) (cols, rows int) {
	if w <= 0 || h <= 0 || maxCols <= 0 || maxRows <= 0 {
		return 0, 0
	}
	cols = min(ceilDiv(w, cell.W), maxCols, kittyMax)
	rows = ceilDiv(h*cols*cell.W, w*cell.H)
	if rows > min(maxRows, kittyMax) {
		rows = min(maxRows, kittyMax)
		cols = max(ceilDiv(w*rows*cell.H, h*cell.W), 1)
		cols = min(cols, maxCols)
	}
	return cols, max(rows, 1)
}

// kittyMax is how many rows/columns the placeholder diacritics can address.
const kittyMax = 256

func ceilDiv(a, b int) int { return (a + b - 1) / b }

// transmit builds the escapes that send a PNG as a virtual placement of
// cols × rows cells, in chunks, quietly (q=2: no replies).
func transmit(id, cols, rows int, pngData []byte, s Support) string {
	payload := base64.StdEncoding.EncodeToString(pngData)
	var out strings.Builder
	for i := 0; i < len(payload); i += kitty.MaxChunkSize {
		end := min(i+kitty.MaxChunkSize, len(payload))
		more := "m=1"
		if end == len(payload) {
			more = "m=0"
		}
		var opts []string
		if i == 0 {
			opts = []string{"a=T", "f=100", "t=d", "U=1", "q=2", "i=" + strconv.Itoa(id),
				"c=" + strconv.Itoa(cols), "r=" + strconv.Itoa(rows), more}
		} else {
			opts = []string{"q=2", more}
		}
		seq := ansi.KittyGraphics([]byte(payload[i:end]), opts...)
		if s.Tmux {
			seq = ansi.TmuxPassthrough(seq)
		}
		out.WriteString(seq)
	}
	return out.String()
}

// Lines are the placeholder lines that display the image, each Cols wide.
// Every cell carries its row and column, so each is valid on its own,
// however the renderer splits up its output.
func (im *Image) Lines() []string {
	color := "\x1b[38;5;" + strconv.Itoa(im.ID) + "m"
	lines := make([]string, im.Rows)
	for r := range lines {
		var b strings.Builder
		b.WriteString(color)
		for c := 0; c < im.Cols; c++ {
			b.WriteRune(kitty.Placeholder)
			b.WriteRune(kitty.Diacritic(r))
			b.WriteRune(kitty.Diacritic(c))
		}
		b.WriteString("\x1b[39m")
		lines[r] = b.String()
	}
	return lines
}

// Delete frees an image in the terminal.
func Delete(id int, s Support) string {
	seq := ansi.KittyGraphics(nil, "a=d", "d=I", "i="+strconv.Itoa(id), "q=2")
	if s.Tmux {
		seq = ansi.TmuxPassthrough(seq)
	}
	return seq
}
