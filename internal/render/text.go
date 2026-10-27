package render

import (
	"regexp"
	"strings"
)

var (
	// Outlook's text rendering puts a link's target in angle brackets after
	// its text: "Register now<https://...>". Those become footnotes.
	bracketURL = regexp.MustCompile(`<((?:https?|mailto):[^>\s]+)>`)
	bareURL    = regexp.MustCompile(`https?://[^\s<>"]+`)
)

// FromText parses a plain-text body. Lines keep their breaks; ">" quoting
// becomes quote depth.
func FromText(src string) *Doc {
	d := &Doc{History: -1, Signature: -1}
	src = strings.ReplaceAll(src, "\r\n", "\n")
	gap := false
	for _, line := range strings.Split(src, "\n") {
		depth, rest := quoteDepth(Sanitize(strings.ReplaceAll(line, "\t", "    "), false))
		rest = strings.TrimRight(rest, " \t")
		if strings.TrimSpace(rest) == "" {
			gap = len(d.Blocks) > 0
			continue
		}
		d.Blocks = append(d.Blocks, Block{Kind: Para, Quote: depth, Runs: textRuns(d, rest), Gap: gap})
		gap = false
	}
	analyze(d)
	return d
}

// quoteDepth counts leading ">" markers, allowing "> > " and ">>".
func quoteDepth(line string) (int, string) {
	depth := 0
	rest := line
	for {
		t := strings.TrimLeft(rest, " ")
		if !strings.HasPrefix(t, ">") {
			break
		}
		depth++
		rest = strings.TrimPrefix(t, ">")
	}
	if depth > 0 {
		rest = strings.TrimPrefix(rest, " ")
	}
	return depth, rest
}

// textRuns splits a line into runs, turning "<url>" targets into footnote
// markers and registering bare URLs as links (shown inline).
func textRuns(d *Doc, line string) []Run {
	var runs []Run
	pos := 0
	for _, m := range bracketURL.FindAllStringSubmatchIndex(line, -1) {
		before := line[pos:m[0]]
		url := line[m[2]:m[3]]
		if strings.HasPrefix(strings.ToLower(url), "mailto:") {
			// "name@x.com<mailto:name@x.com>": the address is already shown.
			runs = append(runs, bareRuns(d, before)...)
			pos = m[1]
			continue
		}
		idx := d.addLink(url)
		if d.Links[idx-1].Text == "" {
			d.Links[idx-1].Text = lastWords(before)
		}
		runs = append(runs, bareRuns(d, before)...)
		runs = append(runs, Run{Text: "[" + itoa(idx) + "]", Faint: true, Ref: idx})
		pos = m[1]
	}
	return append(runs, bareRuns(d, line[pos:])...)
}

func bareRuns(d *Doc, s string) []Run {
	if s == "" {
		return nil
	}
	var runs []Run
	pos := 0
	for _, m := range bareURL.FindAllStringIndex(s, -1) {
		url := strings.TrimRight(s[m[0]:m[1]], ".,;:!?)]")
		idx := d.addLink(url)
		if pos < m[0] {
			runs = append(runs, Run{Text: s[pos:m[0]]})
		}
		runs = append(runs, Run{Text: url, Link: idx})
		pos = m[0] + len(url)
	}
	if pos < len(s) {
		runs = append(runs, Run{Text: s[pos:]})
	}
	return runs
}

// lastWords is a short label for a link from the text just before it.
func lastWords(s string) string {
	f := strings.Fields(s)
	if len(f) > 6 {
		f = f[len(f)-6:]
	}
	return strings.Join(f, " ")
}
