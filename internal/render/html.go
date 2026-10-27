package render

import (
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// FromHTML parses an HTML body. It never fails: malformed HTML is repaired
// by the HTML5 parser, and anything unrecognised is treated as inline text.
func FromHTML(src string) *Doc {
	d := &Doc{History: -1, Signature: -1}
	root, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return FromText(src)
	}
	b := &htmlBuilder{doc: d}
	b.walk(root)
	b.flush()
	analyze(d)
	return d
}

type htmlBuilder struct {
	doc *Doc

	cur          Block
	hasText      bool // cur has non-space text
	pendingSpace bool
	pendingGap   bool

	bold, italic, pre, quote int
	links                    []int // open <a> link indexes
	lists                    []listState
	bulletNext               string // bullet for the next block (from <li>)
}

type listState struct {
	ordered bool
	n       int
}

// Elements whose content is never shown.
var skipTags = map[atom.Atom]bool{
	atom.Head: true, atom.Style: true, atom.Script: true, atom.Title: true,
	atom.Noscript: true, atom.Template: true, atom.Object: true, atom.Iframe: true,
	atom.Svg: true, atom.Button: true, atom.Select: true,
}

// Elements that start a new paragraph with a blank line before.
var gapBlocks = map[atom.Atom]bool{
	atom.P: true, atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Blockquote: true, atom.Ul: true, atom.Ol: true, atom.Table: true, atom.Pre: true, atom.Dl: true,
}

// Elements that start a new line without a blank line.
var lineBlocks = map[atom.Atom]bool{
	atom.Div: true, atom.Tr: true, atom.Li: true, atom.Section: true, atom.Article: true,
	atom.Header: true, atom.Footer: true, atom.Center: true, atom.Dt: true, atom.Dd: true,
	atom.Address: true, atom.Figure: true, atom.Form: true, atom.Nav: true, atom.Main: true,
	atom.Aside: true, atom.Tbody: true, atom.Thead: true, atom.Tfoot: true, atom.Caption: true,
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// hidden reports elements styled to be invisible, such as marketing
// preheaders (display:none) and Outlook-only blocks (mso-hide:all).
func hidden(n *html.Node) bool {
	for _, a := range n.Attr {
		if a.Key == "hidden" {
			return true
		}
	}
	style := strings.ToLower(strings.ReplaceAll(attr(n, "style"), " ", ""))
	return strings.Contains(style, "display:none") || strings.Contains(style, "mso-hide:all") ||
		strings.Contains(style, "visibility:hidden")
}

func (b *htmlBuilder) walk(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		b.text(n.Data)
		return
	case html.ElementNode:
		b.element(n)
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.walk(c)
	}
}

func (b *htmlBuilder) children(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.walk(c)
	}
}

func (b *htmlBuilder) element(n *html.Node) {
	a := n.DataAtom
	if skipTags[a] || hidden(n) {
		return
	}
	switch a {
	case atom.Br:
		if b.hasText {
			b.flush()
		} else if len(b.doc.Blocks) > 0 {
			b.pendingGap = true // <br><br> makes a blank line
		}
		return
	case atom.Hr:
		b.flush()
		b.doc.Blocks = append(b.doc.Blocks, Block{Kind: Rule, Quote: b.quote, Gap: true})
		b.pendingGap = true
		return
	case atom.Img:
		b.image(n)
		return
	case atom.A:
		b.anchor(n)
		return
	case atom.Td, atom.Th:
		if b.hasText {
			b.pendingSpace = true
		}
		b.children(n)
		if b.hasText {
			b.pendingSpace = true
		}
		return
	}

	gap, line := gapBlocks[a], lineBlocks[a]
	if gap || line {
		b.flush()
		if gap && len(b.doc.Blocks) > 0 {
			b.pendingGap = true
		}
	}

	switch a {
	case atom.B, atom.Strong:
		b.bold++
		defer func() { b.bold-- }()
	case atom.I, atom.Em:
		b.italic++
		defer func() { b.italic-- }()
	case atom.Pre:
		b.pre++
		defer func() { b.pre-- }()
	case atom.Blockquote:
		b.quote++
		defer func() { b.quote-- }()
	case atom.Ul, atom.Ol:
		b.lists = append(b.lists, listState{ordered: a == atom.Ol})
		defer func() { b.lists = b.lists[:len(b.lists)-1] }()
	case atom.Li:
		if len(b.lists) > 0 {
			l := &b.lists[len(b.lists)-1]
			l.n++
			if l.ordered {
				b.bulletNext = strconv.Itoa(l.n) + ". "
			} else {
				b.bulletNext = "• "
			}
		}
		// An empty <li> mustn't hand its bullet to the next paragraph.
		defer func() { b.bulletNext = "" }()
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		b.bold++
		defer func() { b.bold-- }()
	}

	b.children(n)

	if gap || line {
		b.flush()
		if gap {
			b.pendingGap = true
		}
	}
}

func (b *htmlBuilder) anchor(n *html.Node) {
	href := strings.TrimSpace(attr(n, "href"))
	lower := strings.ToLower(href)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "mailto:") {
		b.children(n)
		return
	}
	idx := b.doc.addLink(href)
	b.links = append(b.links, idx)
	before := b.textLen()
	b.children(n)
	b.links = b.links[:len(b.links)-1]
	text := strings.TrimSpace(b.textSince(before))
	if b.doc.Links[idx-1].Text == "" {
		b.doc.Links[idx-1].Text = text
	}
	if text != "" && !selfDescribing(text, href) {
		b.addRun(Run{Text: "[" + itoa(idx) + "]", Faint: true, Ref: idx}, false)
	}
}

// selfDescribing reports link text that already shows the target, such as
// an email address linking to its own mailto:, so no footnote marker is needed.
func selfDescribing(text, href string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	h := strings.ToLower(href)
	if strings.HasPrefix(h, "mailto:") {
		addr, _, _ := strings.Cut(strings.TrimPrefix(h, "mailto:"), "?")
		return t == addr
	}
	d := strings.ToLower(DisplayURL(href))
	return t == d || "https://"+t == d || "http://"+t == d || strings.TrimSuffix(d, "/") == strings.TrimSuffix(t, "/")
}

func (b *htmlBuilder) image(n *html.Node) {
	w, _ := strconv.Atoi(strings.TrimSuffix(attr(n, "width"), "px"))
	h, _ := strconv.Atoi(strings.TrimSuffix(attr(n, "height"), "px"))
	if (w > 0 && w <= 2) || (h > 0 && h <= 2) {
		return // tracking pixel or spacer
	}
	alt := strings.TrimSpace(attr(n, "alt"))
	if alt == "" {
		if !strings.HasPrefix(strings.ToLower(attr(n, "src")), "cid:") {
			return // decorative remote image
		}
		alt = "image"
	} else {
		alt = "image: " + alt
	}
	if b.hasText {
		b.pendingSpace = true
	}
	run := Run{Text: "[" + alt + "]", Faint: true}
	if src := attr(n, "src"); strings.HasPrefix(strings.ToLower(src), "cid:") {
		run.Image = ContentID(src[4:])
	}
	b.addRun(run, true)
	b.pendingSpace = true
}

// ContentID normalizes a content ID from a cid: URL or a Content-ID
// header, for matching the two.
func ContentID(s string) string {
	s = strings.Trim(strings.TrimSpace(s), "<>")
	if u, err := url.PathUnescape(s); err == nil {
		s = u
	}
	return strings.ToLower(s)
}

// text adds character data, collapsing whitespace outside <pre>.
func (b *htmlBuilder) text(s string) {
	if b.pre > 0 {
		b.addRun(Run{Text: s}, true)
		return
	}
	var out strings.Builder
	space := b.pendingSpace // whitespace carried over from before this node
	for _, r := range s {
		if unicode.IsSpace(r) || r == '\u00a0' {
			space = true
			continue
		}
		if space && out.Len() > 0 {
			out.WriteByte(' ')
		}
		if out.Len() == 0 {
			b.pendingSpace = space // leading whitespace: addRun decides
		}
		space = false
		out.WriteRune(r)
	}
	if out.Len() > 0 {
		b.addRun(Run{Text: out.String()}, true)
	}
	// Trailing whitespace separates this text from whatever comes next,
	// which may be in another element (e.g. "click <a>here</a>").
	if space && b.hasText {
		b.pendingSpace = true
	}
}

// addRun appends text in the current style. styled applies bold/italic/link
// from the open elements; link markers pass false.
func (b *htmlBuilder) addRun(r Run, styled bool) {
	r.Text = Sanitize(r.Text, b.pre > 0)
	if r.Text == "" {
		return
	}
	if !b.hasText {
		if strings.TrimSpace(r.Text) == "" && b.pre == 0 {
			return
		}
		if b.pre > 0 {
			b.cur.Kind = Pre
		}
	} else if b.pendingSpace && !strings.HasPrefix(r.Text, " ") {
		r.Text = " " + r.Text
	}
	b.pendingSpace = false
	if styled {
		r.Bold = r.Bold || b.bold > 0
		r.Italic = r.Italic || b.italic > 0
		if len(b.links) > 0 {
			r.Link = b.links[len(b.links)-1]
		}
	}
	if !b.hasText {
		b.cur.Quote = b.quote
		b.cur.Indent = max(len(b.lists)-1, 0)
		b.cur.Bullet, b.bulletNext = b.bulletNext, ""
		b.hasText = true
	}
	runs := b.cur.Runs
	if n := len(runs); n > 0 {
		last := &runs[n-1]
		if last.Bold == r.Bold && last.Italic == r.Italic && last.Faint == r.Faint && last.Link == r.Link && last.Ref == 0 && r.Ref == 0 {
			last.Text += r.Text
			return
		}
	}
	b.cur.Runs = append(runs, r)
}

// textLen is the byte length of the current block's text.
func (b *htmlBuilder) textLen() int {
	n := 0
	for _, r := range b.cur.Runs {
		n += len(r.Text)
	}
	return n
}

// textSince returns the current block's text after byte offset off (within
// one block; anchor text spanning blocks just yields the tail).
func (b *htmlBuilder) textSince(off int) string {
	all := b.cur.Plain()
	if off > len(all) {
		return all
	}
	return all[off:]
}

func (b *htmlBuilder) flush() {
	kind := b.cur.Kind
	if b.hasText {
		blk := b.cur
		if kind == Pre {
			blk.Runs = trimPre(blk.Runs)
		}
		blk.Gap = b.pendingGap && len(b.doc.Blocks) > 0
		b.doc.Blocks = append(b.doc.Blocks, blk)
		b.pendingGap = false
	}
	b.cur = Block{}
	b.hasText = false
	b.pendingSpace = false
}

func trimPre(runs []Run) []Run {
	if len(runs) == 0 {
		return runs
	}
	runs[0].Text = strings.TrimLeft(runs[0].Text, "\n")
	last := &runs[len(runs)-1]
	last.Text = strings.TrimRight(last.Text, "\n ")
	return runs
}
