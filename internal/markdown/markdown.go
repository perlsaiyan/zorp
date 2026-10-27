// Package markdown turns a message written in Markdown into email-ready
// HTML: GitHub-flavoured Markdown, line breaks kept as typed, raw HTML
// dropped, and styles inlined (Outlook ignores most <style> rules).
package markdown

import (
	"bytes"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	// Raw HTML in the source is omitted (WithUnsafe is not set), and a line
	// break in the source is a line break in the message.
	goldmark.WithRendererOptions(gmhtml.WithHardWraps(), gmhtml.WithXHTML()),
)

// Base font for the whole message, matching Outlook's default.
const bodyStyle = "font-family: Calibri, Aptos, Arial, sans-serif; font-size: 11pt; line-height: 1.35;"

// styles are inlined onto each element by tag.
var styles = map[atom.Atom]string{
	atom.P:          "margin: 0 0 0.8em 0;",
	atom.H1:         "font-size: 16pt; margin: 0.6em 0 0.4em 0;",
	atom.H2:         "font-size: 14pt; margin: 0.6em 0 0.4em 0;",
	atom.H3:         "font-size: 12pt; margin: 0.6em 0 0.4em 0;",
	atom.H4:         "font-size: 11pt; margin: 0.6em 0 0.4em 0;",
	atom.Ul:         "margin: 0 0 0.8em 0; padding-left: 1.6em;",
	atom.Ol:         "margin: 0 0 0.8em 0; padding-left: 1.6em;",
	atom.Li:         "margin: 0.15em 0;",
	atom.Blockquote: "margin: 0 0 0.8em 0; padding: 0 0 0 0.8em; border-left: 3px solid #c8c8c8; color: #505050;",
	atom.Code:       "font-family: Consolas, 'Courier New', monospace; font-size: 10pt; background: #f3f3f3; padding: 0 0.2em;",
	atom.Pre:        "font-family: Consolas, 'Courier New', monospace; font-size: 10pt; background: #f3f3f3; padding: 0.6em; margin: 0 0 0.8em 0; white-space: pre-wrap;",
	atom.Table:      "border-collapse: collapse; margin: 0 0 0.8em 0;",
	atom.Th:         "border: 1px solid #c8c8c8; padding: 0.25em 0.6em; background: #f3f3f3; text-align: left;",
	atom.Td:         "border: 1px solid #c8c8c8; padding: 0.25em 0.6em;",
	atom.Hr:         "border: none; border-top: 1px solid #c8c8c8; margin: 1em 0;",
	atom.A:          "color: #0563c1;",
}

// ToHTML renders src as an HTML fragment (a styled <div>), ready to be the
// body of a message or to be put above a reply's quoted history.
func ToHTML(src string) string {
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		// goldmark only fails on writer errors, which a bytes.Buffer
		// doesn't have; fall back to escaped text anyway.
		return `<div style="` + bodyStyle + `">` + html.EscapeString(src) + `</div>`
	}
	return `<div style="` + bodyStyle + `">` + inlineStyles(buf.String()) + `</div>`
}

// inlineStyles adds style attributes to the rendered elements.
func inlineStyles(fragment string) string {
	ctx := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := html.ParseFragment(strings.NewReader(fragment), ctx)
	if err != nil {
		return fragment
	}
	var walk func(n *html.Node, inPre bool)
	walk = func(n *html.Node, inPre bool) {
		if n.Type == html.ElementNode {
			st := styles[n.DataAtom]
			if n.DataAtom == atom.Code && inPre {
				st = "" // the <pre> carries the code style
			}
			if st != "" {
				setStyle(n, st)
			}
			inPre = inPre || n.DataAtom == atom.Pre
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inPre)
		}
	}
	var out bytes.Buffer
	for _, n := range nodes {
		walk(n, false)
		if err := html.Render(&out, n); err != nil {
			return fragment
		}
	}
	return out.String()
}

func setStyle(n *html.Node, st string) {
	for i, a := range n.Attr {
		if a.Key == "style" {
			n.Attr[i].Val = st + " " + a.Val
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: "style", Val: st})
}

// Above puts mine at the top of an existing HTML document (a reply or
// forward draft whose body holds the quoted original), just inside <body>.
func Above(mine, document string) string {
	lower := strings.ToLower(document)
	if i := strings.Index(lower, "<body"); i >= 0 {
		if j := strings.Index(lower[i:], ">"); j >= 0 {
			at := i + j + 1
			return document[:at] + mine + document[at:]
		}
	}
	return mine + document
}

// RefMarker starts the part of a reply or forward that is only there for
// reference while writing: it isn't sent, because the server's formatted
// copy of the original goes below the message instead.
const RefMarker = "<!-- zorp: the original below is for reference and is not sent; its formatted copy goes under your message. Delete this line to send the text below as written. -->"

// SplitRef returns the text above the reference marker and whether the
// marker was there. Without it, the whole body is the message.
func SplitRef(body string) (mine string, hasRef bool) {
	for i, line := range strings.SplitAfter(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "<!-- zorp: the original below") {
			lines := strings.SplitAfter(body, "\n")[:i]
			return strings.TrimRight(strings.Join(lines, ""), " \t\n") + "\n", true
		}
	}
	return body, false
}
