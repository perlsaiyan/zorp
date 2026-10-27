// Package render turns email bodies (HTML or plain text) into styled,
// wrapped terminal lines: links become numbered footnotes, quoted history
// and signatures fold away, images become placeholders (remote images are
// never fetched), and hidden HTML such as preheaders is dropped.
package render

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Run is a span of text with one style.
type Run struct {
	Text   string
	Bold   bool
	Italic bool
	Faint  bool // placeholders, link markers
	Link   int  // text is part of this link (1-based index into Doc.Links), 0 for none
	Ref    int  // this run is the "[n]" marker for link n; such links get a footnote
}

type BlockKind int

const (
	Para BlockKind = iota
	Heading
	Pre  // preformatted: hard-wrapped, whitespace kept
	Rule // horizontal rule
)

// Block is a paragraph-level piece of the message.
type Block struct {
	Kind   BlockKind
	Runs   []Run
	Quote  int    // blockquote / ">" depth
	Indent int    // list nesting
	Bullet string // list marker for the block's first line
	Gap    bool   // blank line before
}

// Plain is the block's text without styling.
func (b Block) Plain() string {
	var s strings.Builder
	for _, r := range b.Runs {
		s.WriteString(r.Text)
	}
	return s.String()
}

// Link is a link found in the message. URL is the real target (which may be
// a security gateway's rewrite); use DisplayURL to show it.
type Link struct {
	Text string
	URL  string
}

// Doc is a parsed message body.
type Doc struct {
	Blocks []Block
	Links  []Link

	// History is the index of the first block of quoted reply history, or
	// -1. Signature is the first block of a "-- " signature, or -1.
	History   int
	Signature int
}

func (d *Doc) addLink(url string) int {
	for i, l := range d.Links {
		if l.URL == url {
			return i + 1
		}
	}
	d.Links = append(d.Links, Link{URL: url})
	return len(d.Links)
}

// PlainText is the whole body as unstyled text, for search indexing.
func (d *Doc) PlainText() string {
	lines := make([]string, 0, len(d.Blocks))
	for _, b := range d.Blocks {
		if b.Kind == Rule {
			continue
		}
		lines = append(lines, b.Plain())
	}
	return strings.Join(lines, "\n")
}

type Options struct {
	// ShowQuoted shows folded reply history and signatures.
	ShowQuoted bool
}

var (
	styleBold      = lipgloss.NewStyle().Bold(true)
	styleItalic    = lipgloss.NewStyle().Italic(true)
	styleFaint     = lipgloss.NewStyle().Faint(true)
	styleUnderline = lipgloss.NewStyle().Underline(true)
	styleQuoteBar  = lipgloss.NewStyle().Faint(true)
)

// Folded reports whether Render hides anything with these options.
func (d *Doc) Folded(opts Options) bool {
	return !opts.ShowQuoted && (d.History >= 0 || d.Signature >= 0)
}

// Render lays the document out at width columns.
func (d *Doc) Render(width int, opts Options) []string {
	width = max(width, 20)
	end := len(d.Blocks)
	hiddenFrom := end
	if !opts.ShowQuoted {
		if d.History >= 0 {
			hiddenFrom = d.History
		}
		if d.Signature >= 0 && d.Signature < hiddenFrom {
			hiddenFrom = d.Signature
		}
	}

	var out []string
	used := map[int]bool{}
	for i := 0; i < hiddenFrom; i++ {
		b := d.Blocks[i]
		if b.Gap && len(out) > 0 && out[len(out)-1] != "" {
			out = append(out, "")
		}
		for _, r := range b.Runs {
			if r.Ref > 0 {
				used[r.Ref] = true
			}
		}
		out = append(out, renderBlock(b, width)...)
	}

	if hiddenFrom < end {
		hidden := 0
		for _, b := range d.Blocks[hiddenFrom:] {
			hidden += strings.Count(b.Plain(), "\n") + 1
		}
		what := "quoted text"
		if hiddenFrom == d.Signature && d.History < 0 {
			what = "signature"
		}
		out = append(out, "", styleFaint.Render(ansi.Truncate(
			"··· "+what+" hidden ("+itoa(hidden)+" lines) · z to show", width, "…")))
	}

	if len(used) > 0 {
		out = append(out, "")
		for i, l := range d.Links {
			if used[i+1] {
				out = append(out, styleFaint.Render(ansi.Truncate("["+itoa(i+1)+"] "+Sanitize(DisplayURL(l.URL), false), width, "…")))
			}
		}
	}
	// Trim trailing blank lines.
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

func renderBlock(b Block, width int) []string {
	quote := strings.Repeat("│ ", b.Quote)
	indent := strings.Repeat("  ", b.Indent)
	bullet := b.Bullet
	prefixW := ansi.StringWidth(quote + indent + bullet)
	cw := max(width-prefixW, 10)

	if b.Kind == Rule {
		return []string{styleQuoteBar.Render(quote) + styleFaint.Render(strings.Repeat("─", cw))}
	}

	var s strings.Builder
	for _, r := range b.Runs {
		st := lipgloss.NewStyle()
		if r.Bold || b.Kind == Heading {
			st = st.Inherit(styleBold)
		}
		if r.Italic {
			st = st.Inherit(styleItalic)
		}
		if r.Faint {
			st = st.Inherit(styleFaint)
		}
		if r.Link > 0 {
			st = st.Inherit(styleUnderline)
		}
		// Style each line separately so wrapping never splits an escape.
		parts := strings.Split(r.Text, "\n")
		for i, p := range parts {
			if i > 0 {
				s.WriteString("\n")
			}
			if p != "" {
				s.WriteString(st.Render(p))
			}
		}
	}

	var wrapped string
	if b.Kind == Pre {
		wrapped = ansi.Hardwrap(s.String(), cw, true)
	} else {
		wrapped = ansi.Wrap(s.String(), cw, "")
	}
	lines := enforceWidth(strings.Split(wrapped, "\n"), cw)
	pad := strings.Repeat(" ", ansi.StringWidth(bullet))
	for i, l := range lines {
		lead := pad
		if i == 0 {
			lead = bullet
		}
		lines[i] = styleQuoteBar.Render(quote) + indent + lead + l
	}
	return lines
}

// enforceWidth hard-wraps any line still wider than w. ansi.Wrap can leave
// over-long lines in edge cases (found by fuzzing: a long word starting
// with "-"), and a too-wide line would break the TUI's layout.
func enforceWidth(lines []string, w int) []string {
	out := lines[:0:0]
	for _, l := range lines {
		if ansi.StringWidth(l) <= w {
			out = append(out, l)
			continue
		}
		out = append(out, strings.Split(ansi.Hardwrap(l, w, true), "\n")...)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Text is the whole document (nothing folded) as unstyled lines wrapped at
// width, with link footnotes: the form used to quote or forward a message.
func (d *Doc) Text(width int) string {
	lines := d.Render(width, Options{ShowQuoted: true})
	for i, l := range lines {
		lines[i] = strings.TrimRight(ansi.Strip(l), " ")
	}
	return strings.Join(lines, "\n")
}
