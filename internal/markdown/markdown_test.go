package markdown

import (
	"strings"
	"testing"
)

func TestToHTML(t *testing.T) {
	out := ToHTML("# Plan\n\nHi **Bob**,\nsee `x` and [docs](https://example.com).\n\n- one\n- two\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n```\ncode block\n```\n\n> quoted\n")
	for _, want := range []string{
		`<div style="font-family: Calibri`,
		`<h1 style="font-size: 16pt;`,
		`Hi <strong>Bob</strong>,<br/>`, // line breaks kept as typed
		`<code style="font-family: Consolas`,
		`<a href="https://example.com" style="color: #0563c1;">docs</a>`,
		`<li style="margin: 0.15em 0;">one</li>`,
		`<td style="border: 1px solid`,
		`<pre style="font-family: Consolas`,
		`<blockquote style="margin: 0 0 0.8em 0; padding: 0 0 0 0.8em; border-left`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "<pre style=\"font-family: Consolas, 'Courier New', monospace; font-size: 10pt; background: #f3f3f3; padding: 0.6em; margin: 0 0 0.8em 0; white-space: pre-wrap;\"><code style") {
		t.Error("code inside pre should not get its own style")
	}
}

func TestToHTMLDropsRawHTMLAndDangerousLinks(t *testing.T) {
	out := ToHTML("<script>alert(1)</script>\n\n<img src=x onerror=alert(1)>\n\n[click](javascript:alert(1))\n")
	for _, bad := range []string{"<script", "onerror", "javascript:"} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains %q:\n%s", bad, out)
		}
	}
}

func TestAbove(t *testing.T) {
	doc := `<html><head><meta charset="utf-8"></head><BODY class="x"><div>original</div></body></html>`
	got := Above("<p>mine</p>", doc)
	if !strings.Contains(got, `<BODY class="x"><p>mine</p><div>original</div>`) {
		t.Errorf("Above = %s", got)
	}
	if got := Above("<p>mine</p>", "<div>no body tag</div>"); got != "<p>mine</p><div>no body tag</div>" {
		t.Errorf("no body: %s", got)
	}
}

func TestSplitRef(t *testing.T) {
	body := "Thanks!\n\n-- \nTom\n\n" + RefMarker + "\nOn Mon, Alice wrote:\n> hi\n"
	mine, ok := SplitRef(body)
	if !ok || mine != "Thanks!\n\n-- \nTom\n" {
		t.Errorf("SplitRef = %q, %v", mine, ok)
	}
	if mine, ok := SplitRef("just text\n> quote\n"); ok || mine != "just text\n> quote\n" {
		t.Errorf("no marker: %q %v", mine, ok)
	}
}

func TestLocalImages(t *testing.T) {
	src := "![a](~/pics/a.png) ![b](https://example.com/b.png) ![c](/tmp/c.gif)\n\n![d](cid:x) ![e](<~/my pics/e.jpg>)\n"
	got := LocalImages(src)
	if strings.Join(got, "|") != "~/pics/a.png|/tmp/c.gif|~/my pics/e.jpg" {
		t.Errorf("LocalImages = %q", got)
	}
	html := ToHTMLImages(src, func(p string) string {
		if p == "~/pics/a.png" {
			return "cid:img1@zorp"
		}
		return ""
	})
	if !strings.Contains(html, `src="cid:img1@zorp"`) || !strings.Contains(html, `src="https://example.com/b.png"`) || !strings.Contains(html, "max-width") {
		t.Errorf("html = %s", html)
	}
}
