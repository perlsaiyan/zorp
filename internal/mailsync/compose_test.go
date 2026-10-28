package mailsync

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	pngenc "image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/perlsaiyan/zorp/internal/compose"
	"github.com/perlsaiyan/zorp/internal/graph"
	"github.com/perlsaiyan/zorp/internal/markdown"
)

// composeGraph records compose requests and answers them.
type composeGraph struct {
	mu        sync.Mutex
	calls     []string
	bodies    map[string]map[string]any
	failSend  bool
	changeKey string
}

func newComposeEngine(t *testing.T, cg *composeGraph) *Engine {
	cg.bodies = map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cg.mu.Lock()
		defer cg.mu.Unlock()
		call := r.Method + " " + r.URL.Path
		cg.calls = append(cg.calls, call)
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		cg.bodies[call] = body
		switch {
		case r.Method == "POST" && (r.URL.Path == "/me/messages" || strings.HasSuffix(r.URL.Path, "/createReply") ||
			strings.HasSuffix(r.URL.Path, "/createReplyAll") || strings.HasSuffix(r.URL.Path, "/createForward")):
			io.WriteString(w, `{"id":"draft1"}`)
		case r.Method == "GET" && r.URL.Path == "/me/messages/draft1/attachments":
			io.WriteString(w, `{"value":[
				{"id":"a1","name":"report.pdf","isInline":false},
				{"id":"a2","name":"image001.png","isInline":true},
				{"id":"a3","name":"notes.txt","isInline":false}]}`)
		case strings.HasSuffix(r.URL.Path, "/send") && cg.failSend:
			w.WriteHeader(http.StatusBadGateway)
		case r.Method == "GET" && r.URL.Path == "/me/messages/draft1":
			json.NewEncoder(w).Encode(map[string]any{
				"id": "draft1", "changeKey": cg.changeKey, "isDraft": true, "subject": "Re: x",
				"body": map[string]string{"contentType": "html", "content": `<html><body><div id="divRplyFwdMsg">From: Alice</div><p>original <b>formatted</b></p></body></html>`},
			})
		case r.Method == "GET" && r.URL.Path == "/me/messages/att-msg/attachments":
			io.WriteString(w, `{"value":[
				{"@odata.type":"#microsoft.graph.fileAttachment","id":"att1","name":"../../etc/passwd","size":5},
				{"@odata.type":"#microsoft.graph.itemAttachment","id":"att2","name":"Lunch?","size":900},
				{"@odata.type":"#microsoft.graph.referenceAttachment","id":"att3","name":"plan.docx"}]}`)
		case r.Method == "GET" && r.URL.Path == "/me/messages/att-msg/attachments/att1/$value":
			io.WriteString(w, "hello")
		case r.Method == "GET" && r.URL.Path == "/me/messages/att-msg/attachments/att2/$value":
			io.WriteString(w, "Subject: Lunch?\r\n\r\nNoon?\r\n")
		case r.Method == "GET" && r.URL.Path == "/me/messages/att-msg/attachments/att2":
			io.WriteString(w, `{"item":{"@odata.type":"#microsoft.graph.message","subject":"Lunch?",
				"from":{"emailAddress":{"name":"Al","address":"al@example.com"}},
				"body":{"contentType":"html","content":"<p>Noon?</p>"}}}`)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(srv.Close)
	g := graph.New(staticToken{}, graph.WithBaseURL(srv.URL))
	return New(g, nil, Options{DraftDir: t.TempDir()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

var to = []graph.EmailAddress{{Name: "Me", Address: "me@example.com"}}

func TestComposeNewWithoutAttachmentsUsesSendMail(t *testing.T) {
	cg := &composeGraph{}
	e := newComposeEngine(t, cg)
	if _, err := e.Compose(context.Background(), Outgoing{Kind: compose.New, Send: true, Draft: graph.Draft{Subject: "s", Body: "b", To: to}}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(cg.calls, ",") != "POST /me/sendMail" {
		t.Errorf("calls = %v", cg.calls)
	}
	msg := cg.bodies["POST /me/sendMail"]["message"].(map[string]any)
	if msg["subject"] != "s" || msg["body"].(map[string]any)["contentType"] != "text" {
		t.Errorf("sendMail body = %v", msg)
	}
}

func TestComposeReplyThreadsThenSends(t *testing.T) {
	cg := &composeGraph{}
	e := newComposeEngine(t, cg)
	_, err := e.Compose(context.Background(), Outgoing{Kind: compose.ReplyAll, Ref: "orig", Send: true, Draft: graph.Draft{Subject: "Re: x", Body: "b", To: to}})
	if err != nil {
		t.Fatal(err)
	}
	want := "POST /me/messages/orig/createReplyAll,PATCH /me/messages/draft1,POST /me/messages/draft1/send"
	if strings.Join(cg.calls, ",") != want {
		t.Errorf("calls = %v", cg.calls)
	}
}

func TestComposeForwardPrunesAndAttaches(t *testing.T) {
	cg := &composeGraph{}
	e := newComposeEngine(t, cg)
	path := filepath.Join(t.TempDir(), "new.txt")
	os.WriteFile(path, []byte("hi"), 0o600)
	id, err := e.Compose(context.Background(), Outgoing{Kind: compose.Forward, Ref: "orig", Draft: graph.Draft{Subject: "Fw: x"},
		Keep: []string{"report.pdf"}, Attach: []string{path}})
	if err != nil || id != "draft1" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	want := "POST /me/messages/orig/createForward,PATCH /me/messages/draft1,GET /me/messages/draft1/attachments," +
		"DELETE /me/messages/draft1/attachments/a2,DELETE /me/messages/draft1/attachments/a3,POST /me/messages/draft1/attachments"
	if strings.Join(cg.calls, ",") != want {
		t.Errorf("calls =\n%v\nwant\n%v", strings.Join(cg.calls, ","), want)
	}
	att := cg.bodies["POST /me/messages/draft1/attachments"]
	if att["name"] != "new.txt" || att["contentBytes"] != "aGk=" || !strings.HasPrefix(att["contentType"].(string), "text/plain") {
		t.Errorf("attachment = %v", att)
	}
}

func TestComposeBadAttachmentChangesNothing(t *testing.T) {
	cg := &composeGraph{}
	e := newComposeEngine(t, cg)
	for _, p := range []string{"/no/such/file", "relative.txt", t.TempDir()} {
		if _, err := e.Compose(context.Background(), Outgoing{Kind: compose.New, Send: true, Attach: []string{p}}); err == nil {
			t.Errorf("%s: no error", p)
		}
	}
	if len(cg.calls) != 0 {
		t.Errorf("calls = %v", cg.calls)
	}
}

func TestComposeSendFailureKeepsDraft(t *testing.T) {
	cg := &composeGraph{failSend: true}
	e := newComposeEngine(t, cg)
	_, err := e.Compose(context.Background(), Outgoing{Kind: compose.Reply, Ref: "orig", Send: true, Draft: graph.Draft{To: to}})
	var kept *DraftKept
	if !errors.As(err, &kept) || kept.ID != "draft1" || !strings.Contains(err.Error(), "saved in Drafts") {
		t.Fatalf("err = %v", err)
	}
	if n := strings.Count(strings.Join(cg.calls, ","), "/send"); n != 1 {
		t.Errorf("send attempted %d times", n)
	}
}

func TestSaveAttachmentNeverEscapesOrOverwrites(t *testing.T) {
	cg := &composeGraph{}
	e := newComposeEngine(t, cg)
	dir := t.TempDir()
	p1, err := e.SaveAttachment(context.Background(), "att-msg", "att1", dir)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := e.SaveAttachment(context.Background(), "att-msg", "att1", dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(p1) != dir || filepath.Base(p1) != "passwd" || filepath.Base(p2) != "passwd (1)" {
		t.Errorf("paths %s, %s", p1, p2)
	}
	if data, _ := os.ReadFile(p1); string(data) != "hello" {
		t.Errorf("saved %q", data)
	}
	eml, err := e.SaveAttachment(context.Background(), "att-msg", "att2", dir)
	if err != nil || filepath.Base(eml) != "Lunch?.eml" {
		t.Errorf("attached email saved as %s, %v", eml, err)
	}
	if _, err := e.SaveAttachment(context.Background(), "att-msg", "att3", dir); !errors.Is(err, errReference) {
		t.Errorf("reference attachment: %v", err)
	}
	if data, _ := os.ReadFile(p1); string(data) != "hello" {
		t.Errorf("content = %q", data)
	}
	if st, _ := os.Stat(p1); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", st.Mode())
	}
}

func TestSafeFileName(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf": "report.pdf", `C:\\Users\\x\\evil.exe`: "evil.exe", "..": "attachment", ".bashrc": "bashrc",
		"a\x1b[31mb.txt": "a[31mb.txt", "": "attachment", "dir/": "attachment",
	} {
		if got := SafeFileName(in); got != want {
			t.Errorf("SafeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func patchedBody(t *testing.T, cg *composeGraph) (contentType, content string) {
	t.Helper()
	b, ok := cg.bodies["PATCH /me/messages/draft1"]["body"].(map[string]any)
	if !ok {
		t.Fatalf("no PATCH body; calls %v", cg.calls)
	}
	return b["contentType"].(string), b["content"].(string)
}

func TestMarkdownReplyGoesAboveFormattedHistory(t *testing.T) {
	cg := &composeGraph{changeKey: "ck1"}
	e := newComposeEngine(t, cg)
	body := "Sounds **good**.\n\n" + markdown.RefMarker + "\nOn Mon, Alice wrote:\n> original formatted\n"
	_, err := e.Compose(context.Background(), Outgoing{Kind: compose.Reply, Format: compose.FormatMarkdown, Ref: "orig", Send: true,
		Draft: graph.Draft{To: to, Body: body}})
	if err != nil {
		t.Fatal(err)
	}
	ct, content := patchedBody(t, cg)
	mine := strings.Index(content, "Sounds <strong>good</strong>.")
	hist := strings.Index(content, `<div id="divRplyFwdMsg">`)
	if ct != "html" || mine < 0 || hist < mine || strings.Contains(content, "&gt; original") || strings.Contains(content, "zorp: the original") {
		t.Errorf("body (%s):\n%s", ct, content)
	}
}

func TestMarkdownWithoutMarkerSendsAsWritten(t *testing.T) {
	cg := &composeGraph{}
	e := newComposeEngine(t, cg)
	_, err := e.Compose(context.Background(), Outgoing{Kind: compose.Reply, Format: compose.FormatMarkdown, Ref: "orig", Send: true,
		Draft: graph.Draft{To: to, Body: "> question\n\nanswer\n"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(cg.calls, ","), "GET /me/messages/draft1,") {
		t.Errorf("history fetched without the marker: %v", cg.calls)
	}
	if _, content := patchedBody(t, cg); !strings.Contains(content, "<blockquote") || strings.Contains(content, "divRplyFwdMsg") {
		t.Errorf("body:\n%s", content)
	}
}

func TestSavedMarkdownDraftEditsAsWritten(t *testing.T) {
	cg := &composeGraph{changeKey: "ck1"}
	e := newComposeEngine(t, cg)
	source := "Draft *text*\n\n" + markdown.RefMarker + "\n> quoted\n"
	id, err := e.Compose(context.Background(), Outgoing{Kind: compose.Reply, Format: compose.FormatMarkdown, Ref: "orig",
		Draft: graph.Draft{To: to, Body: source}})
	if err != nil || id != "draft1" {
		t.Fatalf("save: %q %v", id, err)
	}
	d, err := e.GetDraft(context.Background(), id)
	if err != nil || d.Draft.Body != source || d.Format != compose.FormatMarkdown || d.WasHTML {
		t.Fatalf("GetDraft = %+v, %v", d, err)
	}

	// Editing it again keeps the remembered history under the new text.
	cg.calls, cg.bodies = nil, map[string]map[string]any{}
	_, err = e.Compose(context.Background(), Outgoing{Kind: compose.EditDraft, Format: compose.FormatMarkdown, Ref: id, Send: true,
		Draft: graph.Draft{To: to, Body: "Final *text*\n\n" + markdown.RefMarker + "\n> quoted\n"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, content := patchedBody(t, cg); !strings.Contains(content, "Final <em>text</em>") || !strings.Contains(content, "divRplyFwdMsg") {
		t.Errorf("edited body:\n%s", content)
	}
	if e.loadSidecar(id) != nil {
		t.Error("sidecar kept after sending")
	}

	// A draft changed elsewhere (new changeKey) falls back to its HTML as text.
	e.saveSidecar(context.Background(), id, sidecar{Format: compose.FormatMarkdown, Source: source})
	cg.changeKey = "ck2"
	d, _ = e.GetDraft(context.Background(), id)
	if d.Format != "" || !d.WasHTML || !strings.Contains(d.Draft.Body, "original formatted") {
		t.Errorf("stale sidecar used: %+v", d)
	}
}

func TestOpenAttachment(t *testing.T) {
	cg := &composeGraph{}
	e := newComposeEngine(t, cg)
	dir := t.TempDir()
	f, err := e.OpenAttachment(context.Background(), "att-msg", "att1", dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(f.Path) != dir || !strings.HasSuffix(f.Path, "-passwd") || f.Kind != graph.KindFile {
		t.Errorf("opened %+v", f)
	}
	if st, err := os.Stat(f.Path); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("copy: %v, %v", st, err)
	}
	m, err := e.OpenAttachment(context.Background(), "att-msg", "att2", dir)
	if err != nil || m.Message == nil || m.Message.Subject != "Lunch?" || m.Message.Body.Content != "<p>Noon?</p>" {
		t.Errorf("attached email: %+v, %v", m, err)
	}
	if _, err := e.OpenAttachment(context.Background(), "att-msg", "nope", dir); err == nil {
		t.Error("opened a missing attachment")
	}
}

func TestComposeMarkdownInlineImages(t *testing.T) {
	cg := &composeGraph{}
	e := newComposeEngine(t, cg)
	dir := t.TempDir()
	png := filepath.Join(dir, "chart.png")
	var buf strings.Builder
	if err := pngenc.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 20, 20))); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(png, []byte(buf.String()), 0o600)
	fake := filepath.Join(dir, "secret.png")
	os.WriteFile(fake, []byte("-----BEGIN PRIVATE KEY-----"), 0o600)

	// Anything but a real image is refused before anything is created.
	_, err := e.Compose(context.Background(), Outgoing{Kind: compose.New, Format: compose.FormatMarkdown, Send: true,
		Draft: graph.Draft{Subject: "s", Body: "![x](" + fake + ")\n", To: to}})
	if err == nil || len(cg.calls) != 0 {
		t.Fatalf("fake image: err %v, calls %v", err, cg.calls)
	}

	_, err = e.Compose(context.Background(), Outgoing{Kind: compose.New, Format: compose.FormatMarkdown, Send: true,
		Draft: graph.Draft{Subject: "s", Body: "See:\n\n![chart](" + png + ")\n", To: to}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cg.calls, ","); got != "POST /me/messages,POST /me/messages/draft1/attachments,POST /me/messages/draft1/send" {
		t.Errorf("calls = %s", got)
	}
	att := cg.bodies["POST /me/messages/draft1/attachments"]
	cid, _ := att["contentId"].(string)
	if att["isInline"] != true || cid == "" || att["name"] != "chart.png" || att["contentType"] != "image/png" {
		t.Errorf("attachment = %v", att)
	}
	html := cg.bodies["POST /me/messages"]["body"].(map[string]any)["content"].(string)
	if !strings.Contains(html, `src="cid:`+cid+`"`) || strings.Contains(html, png) {
		t.Errorf("body = %s", html)
	}
}
