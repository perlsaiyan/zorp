package tui

import (
	"context"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/render"
	"github.com/perlsaiyan/zorp/internal/termgfx"
)

// Inline images (Kitty graphics). The terminal is detected at startup;
// when it can show images, the current message's image attachments are
// fetched through the daemon, transmitted once, and drawn in the preview
// with placeholder characters. Remote images are never loaded.

type gfxState struct {
	support termgfx.Support
	known   bool // detection finished
	cell    termgfx.CellSize
	lastID  int // last image ID handed out (IDs cycle through 1..255)

	// Images of message msgID, prepared for a pane width columns wide.
	msgID    int64
	width    int
	loading  bool
	inline   map[string]*termgfx.Image // by normalized content ID
	attached []namedImage
}

type namedImage struct {
	name string
	img  *termgfx.Image
}

// imagesMsg carries a message's images, encoded for width.
type imagesMsg struct {
	seq      int
	msgID    int64
	width    int
	inline   map[string]*termgfx.Image
	attached []namedImage
	err      error
}

// startGraphics decides from config and environment whether images can be
// shown, asking the terminal when that isn't enough.
func (m *Model) startGraphics() tea.Cmd {
	m.gfx.cell = termgfx.DefaultCell
	m.gfx.support, m.gfx.known = termgfx.FromEnv(m.cfg.UI.Graphics, os.Getenv)
	switch {
	case !m.gfx.known:
		return tea.Raw(termgfx.Query())
	case m.gfx.support.Kitty:
		return tea.Raw("\x1b[16t") // cell size in pixels, to keep aspect ratios
	}
	return nil
}

// graphicsEvent handles the terminal's answers to startGraphics' queries.
func (m *Model) graphicsEvent(msg tea.Msg) (tea.Cmd, bool) {
	switch e := msg.(type) {
	case uv.KittyGraphicsEvent:
		if e.Options.ID == termgfx.QueryID && strings.HasPrefix(string(e.Payload), "OK") {
			m.gfx.support.Kitty = true
		}
	case uv.CellSizeEvent:
		if e.Width > 0 && e.Height > 0 {
			m.gfx.cell = termgfx.CellSize{W: e.Width, H: e.Height}
			m.gfx.width = 0 // re-encode at the right aspect ratio
		}
	case uv.PrimaryDeviceAttributesEvent:
		// Every terminal answers this, after the queries before it.
		m.gfx.known = true
	default:
		return nil, false
	}
	return m.maybeLoadImages(), true
}

// previewWidth is how wide the message body is shown.
func (m *Model) previewWidth() (w, h int) {
	g := m.geom()
	if m.reading {
		return m.width, g.bodyH
	}
	return g.previewW, g.previewH
}

// maybeLoadImages starts preparing the current message's images if they
// can be shown and aren't ready for the current pane width.
func (m *Model) maybeLoadImages() tea.Cmd {
	if !m.gfx.known || !m.gfx.support.Kitty || m.bodyState != bodyReady || m.doc == nil {
		return nil
	}
	msg, ok := m.currentMessage()
	if !ok || (!msg.HasAttachments && !hasInlineImages(m.doc)) {
		return nil
	}
	w, h := m.previewWidth()
	if w <= 0 || h <= 0 {
		return nil
	}
	if m.gfx.msgID == msg.ID && (m.gfx.width == w || m.gfx.loading) {
		return nil
	}
	m.gfx.msgID, m.gfx.width, m.gfx.loading = msg.ID, w, true
	m.gfx.inline, m.gfx.attached = nil, nil

	ids := make([]int, 12) // as many as the daemon returns at most
	for i := range ids {
		m.gfx.lastID = m.gfx.lastID%255 + 1
		ids[i] = m.gfx.lastID
	}
	cfg, seq, gid, s, cell := m.cfg, m.bodySeq, msg.GraphID, m.gfx.support, m.gfx.cell
	maxRows := clamp(h-2, 3, 40)
	return func() tea.Msg {
		return loadImages(cfg, seq, msg.ID, gid, w, maxRows, ids, s, cell)
	}
}

func loadImages(cfg *config.Config, seq int, msgID int64, graphID string, width, maxRows int, ids []int, s termgfx.Support, cell termgfx.CellSize) imagesMsg {
	out := imagesMsg{seq: seq, msgID: msgID, width: width, inline: map[string]*termgfx.Image{}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var files []ipc.ImageInfo
	if out.err = callDaemon(ctx, cfg, ipc.MethodImages, ipc.MessageRef{GraphID: graphID}, &files); out.err != nil {
		return out
	}
	for i, f := range files {
		if i == len(ids) {
			break
		}
		data, err := os.ReadFile(f.Path)
		if err != nil {
			continue
		}
		img, err := termgfx.Encode(ids[i], data, width-1, maxRows, cell, s)
		if err != nil {
			continue // undecodable, or a spacer
		}
		if f.Inline && f.ContentID != "" {
			out.inline[render.ContentID(f.ContentID)] = img
		} else {
			out.attached = append(out.attached, namedImage{f.Name, img})
		}
	}
	return out
}

// imagesReady transmits the images and redraws the preview with them.
func (m *Model) imagesReady(r imagesMsg) tea.Cmd {
	if r.msgID != m.gfx.msgID {
		return nil // the selection changed meanwhile; its own load is running
	}
	if r.width != m.gfx.width {
		// Same message, but the pane was resized or the terminal reported its
		// cell size while this was loading. maybeLoadImages skipped the new
		// width because a load was running, so this result is the last one
		// coming: clear the flag and load again, or the placeholders stay.
		m.gfx.loading = false
		return m.maybeLoadImages()
	}
	m.gfx.loading = false
	if r.err != nil {
		return nil // text placeholders stay; not worth an error line
	}
	m.gfx.inline, m.gfx.attached = r.inline, r.attached
	var seq strings.Builder
	for _, img := range r.inline {
		seq.WriteString(img.Transmit)
	}
	for _, a := range r.attached {
		seq.WriteString(a.img.Transmit)
	}
	m.refreshPreview(false)
	if seq.Len() == 0 {
		return nil
	}
	return tea.Raw(seq.String())
}

// imageLines is render.Options.Image for the current message.
func (m *Model) imageLines(cid string, width int) []string {
	msg, ok := m.currentMessage()
	if !ok || msg.ID != m.gfx.msgID {
		return nil
	}
	if img := m.gfx.inline[cid]; img != nil && img.Cols <= width {
		return img.Lines()
	}
	return nil
}

// attachedImageLines shows image attachments after the body.
func (m *Model) attachedImageLines(width int) []string {
	msg, ok := m.currentMessage()
	if !ok || msg.ID != m.gfx.msgID {
		return nil
	}
	var out []string
	for _, a := range m.gfx.attached {
		if a.img.Cols > width {
			continue
		}
		out = append(out, "", styleFaint.Render(ansi.Truncate("▸ "+clean(a.name), width, "…")))
		out = append(out, a.img.Lines()...)
	}
	return out
}

func hasInlineImages(d *render.Doc) bool {
	for _, b := range d.Blocks {
		for _, r := range b.Runs {
			if r.Image != "" {
				return true
			}
		}
	}
	return false
}
