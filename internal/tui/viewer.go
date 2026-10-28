package tui

import (
	"bytes"
	"context"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/markdown"
	"github.com/perlsaiyan/zorp/internal/render"
	"github.com/perlsaiyan/zorp/internal/store"
	"github.com/perlsaiyan/zorp/internal/termgfx"
)

// Viewing attachments. Text, HTML, attached emails and (with Kitty
// graphics) images are shown full screen in zorp; PDFs too, as text, when
// pdftotext is installed. Anything else goes to the desktop's default app,
// or, over SSH, is saved to ui.save_dir.

type viewKind int

const (
	viewExternal viewKind = iota // not shown in zorp
	viewText                     // shown as is: code, CSV, logs
	viewDoc                      // rendered: HTML, attached emails
	viewImage
)

// maxViewBytes is the largest file shown in zorp.
const maxViewBytes = 8 << 20

// openedMsg is an attachment fetched and prepared for viewing.
type openedMsg struct {
	graphID string
	att     ipc.AttachmentInfo
	res     ipc.OpenAttachmentResult
	kind    viewKind
	text    string
	doc     *render.Doc
	img     *termgfx.Image
	note    string // shown after the name, e.g. "text of the PDF"
	err     error
}

// viewer is an attachment shown full screen.
type viewer struct {
	openedMsg
	vp viewport.Model
}

// openAttachmentCmd fetches an attachment through the daemon and prepares
// it for the viewer, sized for the screen.
func (m *Model) openAttachmentCmd(graphID string, att ipc.AttachmentInfo) tea.Cmd {
	cfg, s, cell := m.cfg, m.gfx.support, m.gfx.cell
	w, h := max(m.width-1, 1), max(m.geom().bodyH-2, 1)
	m.gfx.lastID = m.gfx.lastID%255 + 1
	id := m.gfx.lastID
	m.notice = "Opening " + clean(att.Name) + "…"
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		r := openedMsg{graphID: graphID, att: att}
		r.err = callDaemon(ctx, cfg, ipc.MethodOpenAttach, ipc.SaveAttachmentParams{GraphID: graphID, AttachmentID: att.ID}, &r.res)
		if r.err == nil {
			prepareView(ctx, &r, w, h, id, s, cell)
		}
		return r
	}
}

// prepareView decides how to show an opened attachment.
func prepareView(ctx context.Context, r *openedMsg, w, h, id int, s termgfx.Support, cell termgfx.CellSize) {
	if msg := r.res.Message; msg != nil {
		r.kind = viewDoc
		r.doc = parseBody(store.Body{Type: msg.BodyType, HTML: msg.Body, Text: msg.Body})
		return
	}
	st, err := os.Stat(r.res.Path)
	if err != nil || st.Size() > maxViewBytes {
		return
	}
	data, err := os.ReadFile(r.res.Path)
	if err != nil {
		return
	}
	ct, _, _ := mime.ParseMediaType(r.res.ContentType)
	ext := strings.ToLower(filepath.Ext(r.res.Name))
	switch {
	case strings.HasPrefix(ct, "image/") || imageExts[ext]:
		if !s.Kitty {
			return
		}
		if img, err := termgfx.Encode(id, data, w, h, cell, s); err == nil {
			r.kind, r.img = viewImage, img
		}
	case ct == "application/pdf" || ext == ".pdf":
		if text, err := pdfText(ctx, r.res.Path); err == nil && strings.TrimSpace(text) != "" {
			r.kind, r.text, r.note = viewText, text, "text of the PDF; o opens the PDF itself"
		}
	case ct == "text/html" || ext == ".html" || ext == ".htm":
		r.kind, r.doc = viewDoc, render.FromHTML(string(data))
	case (ct == "text/markdown" || ext == ".md" || ext == ".markdown") && utf8.Valid(data):
		r.kind, r.doc = viewDoc, render.FromHTML(markdown.ToHTML(string(data)))
	case looksText(ct, ext, data):
		r.kind, r.text = viewText, strings.ToValidUTF8(string(data), "�")
	}
}

var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true}

var textTypes = map[string]bool{
	"application/json": true, "application/xml": true, "application/javascript": true,
	"application/x-yaml": true, "application/yaml": true, "application/x-sh": true,
	"application/csv": true, "application/sql": true, "application/x-subrip": true,
}

var textExts = map[string]bool{
	".txt": true, ".csv": true, ".tsv": true, ".log": true, ".json": true, ".xml": true,
	".yaml": true, ".yml": true, ".md": true, ".ini": true, ".conf": true, ".cfg": true,
	".toml": true, ".sql": true, ".ics": true, ".vcf": true, ".go": true, ".py": true,
	".js": true, ".ts": true, ".sh": true, ".ps1": true, ".c": true, ".h": true, ".diff": true,
	".patch": true, ".eml": true,
}

// looksText reports whether data can be shown as text: it says it is text
// (or nothing in particular) and has no NUL bytes. Text that claims to be
// text may have stray invalid UTF-8; anything else must be valid.
func looksText(ct, ext string, data []byte) bool {
	sample := data[:min(len(data), 8192)]
	if bytes.IndexByte(sample, 0) >= 0 {
		return false
	}
	switch {
	case strings.HasPrefix(ct, "text/"), textTypes[ct], textExts[ext]:
		return true
	case ct == "", ct == "application/octet-stream":
		return utf8.Valid(data)
	}
	return false
}

// pdfText extracts a PDF's text with pdftotext (poppler), if installed.
func pdfText(ctx context.Context, path string) (string, error) {
	exe, err := exec.LookPath("pdftotext")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "-layout", "-q", "-enc", "UTF-8", path, "-").Output()
	if len(out) > maxViewBytes {
		out = out[:maxViewBytes]
	}
	return string(out), err
}

// opened shows a prepared attachment, or hands it on when zorp can't.
func (m *Model) opened(r openedMsg) tea.Cmd {
	m.notice = ""
	if r.err != nil {
		m.errText = "open: " + r.err.Error()
		return nil
	}
	if r.kind == viewExternal {
		return m.openExternally(r.graphID, r.att, r.res.Path)
	}
	m.viewer = &viewer{openedMsg: r, vp: viewport.New()}
	m.layoutViewer()
	if r.img != nil {
		return tea.Raw(r.img.Transmit)
	}
	return nil
}

// openExternally opens a file attachment's copy with the desktop's
// default app, or saves it when there's no desktop here.
func (m *Model) openExternally(graphID string, att ipc.AttachmentInfo, path string) tea.Cmd {
	if path != "" && canOpenLocally(m.cfg) {
		m.notice = "Opening " + clean(att.Name) + " in its app…"
		return func() tea.Msg { return openDoneMsg{path, xdgOpen(path)} }
	}
	m.notice = "Saving " + clean(att.Name) + "…"
	cmd := saveAttachmentsCmd(m.cfg, graphID, []string{att.ID})
	return func() tea.Msg {
		r := cmd().(savedMsg)
		r.why = "zorp can't show it, and there's no desktop here to open it on"
		return r
	}
}

func (m *Model) layoutViewer() {
	v := m.viewer
	w, h := m.width, m.geom().bodyH
	if w <= 0 || h <= 0 {
		return
	}
	v.vp.SetWidth(w)
	v.vp.SetHeight(h)
	v.vp.SetContent(strings.Join(v.lines(w), "\n"))
}

func (m *Model) closeViewer() tea.Cmd {
	v := m.viewer
	m.viewer = nil
	if v.img != nil {
		return tea.Raw(termgfx.Delete(v.img.ID, m.gfx.support))
	}
	return nil
}

// lines renders the attachment for a screen w cells wide.
func (v *viewer) lines(w int) []string {
	about := humanSize(int64(v.res.Size))
	if v.res.Message != nil {
		about = "attached email"
	}
	if v.note != "" {
		about += "; " + v.note
	}
	out := []string{
		ansi.Truncate(styleBold.Render(clean(v.res.Name))+styleFaint.Render("  "+about), w, "…"),
		styleFaint.Render(strings.Repeat("─", w)),
	}
	if msg := v.res.Message; msg != nil {
		field := func(name, value string) {
			if value != "" {
				out = append(out, strings.Split(ansi.Wrap(styleHeader.Render(name+":")+" "+value, w, ""), "\n")...)
			}
		}
		field("From", formatAddr(fromIPC([]ipc.Address{msg.From})[0]))
		field("To", formatAddrs(fromIPC(msg.To)))
		field("Cc", formatAddrs(fromIPC(msg.Cc)))
		if t, err := time.Parse(time.RFC3339, msg.Date); err == nil {
			field("Date", t.Local().Format("Mon, 02 Jan 2006 15:04"))
		}
		field("Subject", styleBold.Render(clean(msg.Subject)))
		out = append(out, styleFaint.Render(strings.Repeat("─", w)))
	}
	switch v.kind {
	case viewImage:
		out = append(out, v.img.Lines()...)
	case viewDoc:
		out = append(out, v.doc.Render(w, render.Options{ShowQuoted: true, Styles: &renderStyles})...)
	case viewText:
		out = append(out, textLines(v.text, w)...)
	}
	return out
}

// textLines shows text as it is (no reflowing, for code and tables), made
// safe to print and hard-wrapped at w.
func textLines(text string, w int) []string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var out []string
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		line = clean(expandTabs(line))
		out = append(out, strings.Split(ansi.Hardwrap(line, w, true), "\n")...)
	}
	return out
}

func expandTabs(s string) string {
	if !strings.Contains(s, "\t") {
		return s
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		if r == '\t' {
			n := 8 - col%8
			b.WriteString(strings.Repeat(" ", n))
			col += n
			continue
		}
		b.WriteRune(r)
		col++
	}
	return b.String()
}

func (m *Model) viewerKey(key string) tea.Cmd {
	v := m.viewer
	switch key {
	case "q", "esc", "h", "left":
		return m.closeViewer()
	case "s":
		m.notice = "Saving…"
		return saveAttachmentsCmd(m.cfg, v.graphID, []string{v.att.ID})
	case "o":
		if v.res.Path == "" {
			m.errText = "an attached email opens here; s saves it as .eml"
			return nil
		}
		return m.openExternally(v.graphID, v.att, v.res.Path)
	case "?":
		m.help = true
	default:
		scrollViewport(&v.vp, key)
	}
	return nil
}
