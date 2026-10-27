package render

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func plain(lines []string) string {
	for i, l := range lines {
		lines[i] = ansi.Strip(l)
	}
	return strings.Join(lines, "\n")
}

func TestHTMLDropsHiddenAndInvisibleContent(t *testing.T) {
	d := FromHTML(`<html><head><title>T</title><style>p{color:red}</style></head><body>
		<div style="display: none; max-height:0">Preheader you should not see</div>
		<span style="mso-hide:all">outlook only</span>
		<script>alert(1)</script>
		<p>Hello <b>there</b></p>
		<img src="https://track.example.com/p.gif" width="1" height="1">
		<img src="https://cdn.example.com/spacer.gif">
		<img src="https://cdn.example.com/logo.png" alt="Acme logo">
		<img src="cid:image001.png@01DA">
	</body></html>`)
	out := plain(d.Render(80, Options{}))
	for _, bad := range []string{"Preheader", "outlook only", "alert", "color:red", "track.example", "spacer"} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains %q:\n%s", bad, out)
		}
	}
	for _, want := range []string{"Hello there", "[image: Acme logo]", "[image]"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestLinksBecomeFootnotes(t *testing.T) {
	d := FromHTML(`<p>Please <a href="https://protect.checkpoint.com/v2/r01/___https://example.com/register___.YzJ1OnJh">register now</a>
		or <a href="https://nam02.safelinks.protection.outlook.com/?url=https%3A%2F%2Fexample.org%2Fdocs&data=x">read the docs</a>.
		Again: <a href="https://protect.checkpoint.com/v2/r01/___https://example.com/register___.YzJ1OnJh">register</a>.
		Mail <a href="mailto:bob@example.com">bob@example.com</a>. <a href="javascript:alert(1)">evil</a></p>`)
	out := plain(d.Render(200, Options{}))
	if !strings.Contains(out, "Please register now[1] or read the docs[2]. Again: register[1]. Mail bob@example.com. evil") {
		t.Errorf("inline text wrong:\n%s", out)
	}
	if !strings.Contains(out, "[1] https://example.com/register") || !strings.Contains(out, "[2] https://example.org/docs") {
		t.Errorf("footnotes should show unwrapped URLs:\n%s", out)
	}
	if len(d.Links) != 3 {
		t.Errorf("links = %+v; want register, docs, mailto (deduped, no javascript:)", d.Links)
	}
	if !strings.HasPrefix(d.Links[0].URL, "https://protect.checkpoint.com/") {
		t.Error("the link to open must stay the gateway URL so the gateway can check it")
	}
	if d.Links[0].Text != "register now" {
		t.Errorf("link text = %q", d.Links[0].Text)
	}
}

func TestInlineSpacingAndLists(t *testing.T) {
	d := FromHTML(`<p>click&nbsp;<a href="https://x.example/u">here</a>&nbsp;to <i>unsubscribe</i>.</p>
		<ul><li>one</li><li></li><li>two</li></ul><p>after the list</p>
		<ol><li>first</li><li>second</li></ol>`)
	out := plain(d.Render(80, Options{}))
	if !strings.Contains(out, "click here[1] to unsubscribe.") {
		t.Errorf("spacing across elements:\n%s", out)
	}
	if !strings.Contains(out, "• one\n• two") || strings.Contains(out, "• after") {
		t.Errorf("bullets:\n%s", out)
	}
	if !strings.Contains(out, "1. first\n2. second") {
		t.Errorf("ordered list:\n%s", out)
	}
}

const outlookReply = `<div>Sounds good, see you Wednesday.</div><div><br></div><div>Tom</div>
<hr><div id="divRplyFwdMsg"><b>From:</b> Alice Example &lt;alice@example.com&gt;<br><b>Sent:</b> Friday, October 23, 2026 1:22 PM<br>
<b>To:</b> Tom<br><b>Subject:</b> Re: plans</div><div>Can we meet Wednesday?</div>
<div>On Fri, Oct 23, 2026 at 9:22` + "\u202f" + `AM Bob &lt;bob@example.com&gt; wrote:</div><blockquote><div>Earlier message</div></blockquote>`

func TestFoldsOutlookHistory(t *testing.T) {
	d := FromHTML(outlookReply)
	folded := plain(d.Render(80, Options{}))
	if !strings.Contains(folded, "Sounds good") || strings.Contains(folded, "Can we meet") {
		t.Errorf("folded view:\n%s", folded)
	}
	if !strings.Contains(folded, "quoted text hidden") {
		t.Errorf("missing fold marker:\n%s", folded)
	}
	full := plain(d.Render(80, Options{ShowQuoted: true}))
	for _, want := range []string{"From: Alice Example", "Sent: Friday", "Can we meet", "│ Earlier message"} {
		if !strings.Contains(full, want) {
			t.Errorf("expanded view missing %q:\n%s", want, full)
		}
	}
}

func TestFoldHeuristicsAreConservative(t *testing.T) {
	cases := map[string]struct {
		body string
		html bool
		fold bool
	}{
		"gmail wrote":  {"Thanks!\n\nOn Mon, Jan 5, 2026 at 9:00 AM Bob <b@x.com>\nwrote:\n> hi\n> there", false, true},
		"text quote":   {"Yes.\n\n> Can you come?\n> Thanks", false, true},
		"inline reply": {"See below.\n> question one\nanswer one\n> question two\nanswer two", false, false},
		"forward":      {"FYI\n\n---------- Forwarded message ---------\nFrom: X <x@x.com>\nDate: Mon\n\nbody", false, false},
		"history only": {"From: X <x@x.com>\nSent: Monday\n\nold stuff", false, false},
		"plain":        {"Just a message.\nNo history.", false, false},
		"separator":    {"<p>Reply</p><p>________________________________</p><p>From: X</p><p>Sent: Y</p><p>old</p>", true, true},
	}
	for name, c := range cases {
		var d *Doc
		if c.html {
			d = FromHTML(c.body)
		} else {
			d = FromText(c.body)
		}
		if got := d.History >= 0; got != c.fold {
			t.Errorf("%s: folded=%v, want %v (history=%d)", name, got, c.fold, d.History)
		}
	}
}

func TestSignatureFold(t *testing.T) {
	d := FromText("Short reply.\n\n-- \nTom Allen\nCTO, Example Corp\n+1 555 0100")
	out := plain(d.Render(80, Options{}))
	if strings.Contains(out, "CTO") || !strings.Contains(out, "signature hidden") {
		t.Errorf("signature not folded:\n%s", out)
	}
}

func TestPlainTextBodies(t *testing.T) {
	d := FromText("Register<https://example.com/r> today.\nWrite to bob@x.com<mailto:bob@x.com>.\nSee https://example.org/page.\n\n\n\nBye\tnow")
	out := plain(d.Render(80, Options{}))
	for _, want := range []string{"Register[1] today.", "Write to bob@x.com.", "See https://example.org/page.", "Bye    now", "[1] https://example.com/r"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\n\n\n") {
		t.Errorf("blank lines not collapsed:\n%s", out)
	}
}

func TestUntrustedContentIsSanitized(t *testing.T) {
	esc := "\x1b]52;c;ZXZpbA==\x07\x1b[2J"
	d := FromHTML(`<p>hi ` + esc + `there</p><img alt="x` + esc + `y" src="cid:a"><pre>code` + esc + `
line2</pre><a href="https://e.example/` + esc + `">link</a>`)
	for _, l := range d.Render(80, Options{}) {
		s := ansi.Strip(l)
		if strings.ContainsAny(s, "\x1b\x07") {
			t.Errorf("raw control characters survived: %q", s)
		}
	}
	if FromText("a\x1b[31mb\u202ec").PlainText() != "a[31mbc" {
		t.Errorf("text sanitize: %q", FromText("a\x1b[31mb\u202ec").PlainText())
	}
}

func TestRenderNeverExceedsWidth(t *testing.T) {
	long := strings.Repeat("https://example.com/very/long/path/", 10)
	d := FromHTML(`<blockquote><blockquote><ul><li>` + strings.Repeat("日本語テキスト ", 30) + long +
		`</li></ul></blockquote></blockquote><pre>` + strings.Repeat("x", 300) + `</pre><a href="` + long + `">l</a><hr>`)
	for _, w := range []int{20, 37, 80} {
		for _, l := range d.Render(w, Options{ShowQuoted: true}) {
			if lw := ansi.StringWidth(l); lw > w {
				t.Errorf("width %d: line is %d wide: %q", w, lw, ansi.Strip(l))
			}
		}
	}
}

func TestPlainTextForSearch(t *testing.T) {
	d := FromHTML(`<style>.x{}</style><p>Quarterly <b>numbers</b></p><div style="display:none">hidden</div><p>See <a href="https://x.example">report</a></p>`)
	got := d.PlainText()
	if got != "Quarterly numbers\nSee report[1]" {
		t.Errorf("PlainText = %q", got)
	}
}

func FuzzFromHTML(f *testing.F) {
	for _, seed := range []string{outlookReply, "<p><b>x</p></b>", "<table><tr><td>a<td>b</table>", "<a href=", "<li><li><ul>", "\x00\xff<pre>\n"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d := FromHTML(s)
		for _, l := range d.Render(40, Options{ShowQuoted: true}) {
			if ansi.StringWidth(l) > 40 {
				t.Fatalf("line too wide: %q", l)
			}
		}
		for _, l := range FromText(s).Render(40, Options{}) {
			if ansi.StringWidth(l) > 40 {
				t.Fatalf("text line too wide: %q", l)
			}
		}
	})
}
