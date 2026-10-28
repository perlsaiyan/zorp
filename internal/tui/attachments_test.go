package tui

import (
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/store"
	"github.com/perlsaiyan/zorp/internal/termgfx"
)

func TestAttachedLineInPreview(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	cur, _ := m.currentMessage()
	m.st.SetAttachments(context.Background(), m.account.ID, cur.GraphID, []store.Attachment{
		{GraphID: "a", Name: "Q3 \x1b[2Jreport.pdf", Kind: "file", Size: 2 << 20},
		{GraphID: "b", Name: "image001.png", Kind: "file", IsInline: true},
		{GraphID: "c", Name: "Lunch?", Kind: "item"},
	})
	m.bodyID = 0
	deliver(m, m.selectMessage())
	got := m.previewContent(100)
	if strings.Contains(got, "\x1b[2J") {
		t.Error("escape in attachment name printed")
	}
	got = ansi.Strip(got)
	if !strings.Contains(got, "Attached: Q3 [2Jreport.pdf (2.0 MB), Lunch? (email)") || strings.Contains(got, "image001") || strings.Contains(got, "\x1b[2J") {
		t.Errorf("preview:\n%s", got)
	}

	// V lists them from the cache, no daemon needed; inline images last.
	press(m, "V")
	if m.picker == nil || m.picker.kind != pickOpen || len(m.picker.all) != 3 || m.picker.all[2].label != "image001.png" {
		t.Fatalf("picker = %+v (err %q)", m.picker, m.errText)
	}
}

func TestViewerShowsTextSafely(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	path := filepath.Join(t.TempDir(), "notes.csv")
	os.WriteFile(path, []byte("a,b\tc\n\x1b]52;c;cHduZWQ=\x07evil,"+strings.Repeat("x", 100)+"\n"), 0o600)
	r := openedMsg{graphID: "g", att: ipc.AttachmentInfo{ID: "a1", Name: "notes.csv"},
		res: ipc.OpenAttachmentResult{Name: "notes.csv", ContentType: "text/csv", Kind: "file", Size: 120, Path: path}}
	prepareView(context.Background(), &r, 59, 17, 1, m.gfx.support, m.gfx.cell)
	if r.kind != viewText {
		t.Fatalf("kind = %v", r.kind)
	}
	m.Update(r)
	if m.viewer == nil {
		t.Fatalf("no viewer (err %q)", m.errText)
	}
	view := m.View().Content
	if strings.Contains(view, "\x1b]52") || !strings.Contains(ansi.Strip(view), "a,b     c") {
		t.Errorf("view:\n%s", view)
	}
	for _, l := range strings.Split(view, "\n") {
		if ansi.StringWidth(l) > 60 {
			t.Errorf("line too wide: %q", l)
		}
	}
	press(m, "q")
	if m.viewer != nil {
		t.Error("q didn't close the viewer")
	}
}

func TestPrepareViewKinds(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, data, 0o600)
		return p
	}
	cases := []struct {
		name, ct string
		data     []byte
		want     viewKind
	}{
		{"a.txt", "text/plain", []byte("hello"), viewText},
		{"page.html", "text/html", []byte("<p>hi</p>"), viewDoc},
		{"blob", "application/octet-stream", []byte("plain enough"), viewText},
		{"doc.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", []byte("PK\x03\x04\x00\x00"), viewExternal},
		{"x.bin", "application/octet-stream", []byte{0, 1, 2}, viewExternal},
		{"pic.png", "image/png", []byte("not really"), viewExternal}, // no Kitty graphics
	}
	for _, c := range cases {
		r := openedMsg{res: ipc.OpenAttachmentResult{Name: c.name, ContentType: c.ct, Path: write(c.name, c.data)}}
		prepareView(context.Background(), &r, 80, 20, 1, m0.gfx.support, m0.gfx.cell)
		if r.kind != c.want {
			t.Errorf("%s: kind %v, want %v", c.name, r.kind, c.want)
		}
	}
	// An attached email is rendered.
	r := openedMsg{res: ipc.OpenAttachmentResult{Kind: "item", Message: &ipc.AttachedMessage{Subject: "Hi", BodyType: "html", Body: "<p>Noon?</p>"}}}
	prepareView(context.Background(), &r, 80, 20, 1, m0.gfx.support, m0.gfx.cell)
	if r.kind != viewDoc || !strings.Contains(r.doc.PlainText(), "Noon?") {
		t.Errorf("email: %+v", r)
	}
}

var m0 Model // zero graphics state: no Kitty

func TestWalkFilesSkipsHiddenAndSortsNewestFirst(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"old.txt", "Downloads/new.pdf", ".ssh/id_rsa", "proj/node_modules/x.js", "proj/go/main.go", "go/pkg/y.go", "proj/.env"} {
		os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o700)
		os.WriteFile(filepath.Join(root, p), []byte("x"), 0o600)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(root, "old.txt"), old, old)
	var got []string
	for _, f := range walkFiles(root) {
		rel, _ := filepath.Rel(root, f.path)
		got = append(got, rel)
	}
	if strings.Join(got, ",") != "Downloads/new.pdf,proj/go/main.go,old.txt" && strings.Join(got, ",") != "proj/go/main.go,Downloads/new.pdf,old.txt" {
		t.Errorf("files = %v", got)
	}
}

func TestAttachFileFromPicker(t *testing.T) {
	m := newTestModel(t)
	m.cfg.Paths.ComposeDir = t.TempDir()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c := prepareCompose(t, m, "c")
	editFile(t, c, func(s string) string {
		return strings.Replace(s, "To: \n", "To: me@example.com\n", 1) + "\nSee the attached report.\n"
	})
	m.editorDone(nil)
	if !strings.Contains(ansi.Strip(strings.Join(m.composeLines(100, 28), "\n")), "mentions an attachment") {
		t.Error("no forgotten-attachment warning")
	}
	file := filepath.Join(t.TempDir(), "report.pdf")
	os.WriteFile(file, []byte("%PDF"), 0o600)
	m.Update(filesMsg{files: []pickedFile{{path: file, size: 4, mod: time.Now()}}})
	if m.picker == nil || m.picker.kind != pickFile {
		t.Fatalf("no file picker (err %q)", m.errText)
	}
	press(m, "r", "e", "p", "enter")
	if m.picker != nil || len(m.composing.attach) != 1 || m.composing.attach[0] != file {
		t.Fatalf("attach = %v, problems %v", m.composing.attach, m.composing.problems)
	}
	if strings.Contains(ansi.Strip(strings.Join(m.composeLines(100, 28), "\n")), "mentions an attachment") {
		t.Error("warning stayed after attaching")
	}
}

func TestPrepareViewImageWithKitty(t *testing.T) {
	var buf strings.Builder
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 200, 100))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "chart.png")
	os.WriteFile(path, []byte(buf.String()), 0o600)
	r := openedMsg{res: ipc.OpenAttachmentResult{Name: "chart.png", ContentType: "image/png", Path: path}}
	prepareView(context.Background(), &r, 80, 20, 7, termgfx.Support{Kitty: true}, termgfx.DefaultCell)
	if r.kind != viewImage || r.img == nil || r.img.ID != 7 || r.img.Cols > 80 || r.img.Rows > 20 {
		t.Fatalf("image view: %+v", r)
	}
	md := filepath.Join(t.TempDir(), "notes.md")
	os.WriteFile(md, []byte("# Title\n\n<script>x</script>\n\n**bold**\n"), 0o600)
	r = openedMsg{res: ipc.OpenAttachmentResult{Name: "notes.md", ContentType: "application/octet-stream", Path: md}}
	prepareView(context.Background(), &r, 80, 20, 7, termgfx.Support{}, termgfx.DefaultCell)
	if r.kind != viewDoc || strings.Contains(r.doc.PlainText(), "script") || !strings.Contains(r.doc.PlainText(), "bold") {
		t.Errorf("markdown view: %v %q", r.kind, r.doc.PlainText())
	}
}
