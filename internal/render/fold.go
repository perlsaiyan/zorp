package render

import (
	"regexp"
	"strings"
)

var (
	separatorRe = regexp.MustCompile(`^(_{10,}|-{2,}\s*Original Message\s*-{2,})$`)
	headerRe    = regexp.MustCompile(`^(Sent|Date):\s`)
	wroteRe     = regexp.MustCompile(`(?s)^On\s.{4,300}\bwrote:$`)
	forwardRe   = regexp.MustCompile(`(?i)^-{2,}\s*(Forwarded message|Begin forwarded message)`)
)

// analyze finds where quoted reply history and the signature start, so
// Render can fold them. It is deliberately conservative: when in doubt
// (forwards, inline replies, history at the very top) nothing is folded.
func analyze(d *Doc) {
	d.History, d.Signature = -1, -1
	plains := make([]string, len(d.Blocks))
	for i, b := range d.Blocks {
		plains[i] = strings.TrimSpace(b.Plain())
	}

	for i, p := range plains {
		switch {
		case forwardRe.MatchString(p):
			// A forward's content is the point of the message: stop looking.
			return
		case separatorRe.MatchString(p):
			d.History = i
		case strings.HasPrefix(p, "From:") && (headerRe.MatchString(afterFirstLine(p)) || nextMatches(plains, i, headerRe)):
			d.History = i
		case wroteRe.MatchString(p) || (i+1 < len(plains) && wroteRe.MatchString(p+" "+plains[i+1])):
			d.History = i
		case d.Blocks[i].Quote > 0 && onlyQuotedAfter(d, plains, i):
			d.History = i
		}
		if d.History >= 0 {
			break
		}
	}
	if d.History == 0 || !hasContentBefore(plains, d.History) {
		d.History = -1
	}

	limit := len(plains)
	if d.History >= 0 {
		limit = d.History
	}
	for i := 0; i < limit; i++ {
		if d.Blocks[i].Quote == 0 && (plains[i] == "--" || d.Blocks[i].Plain() == "-- ") {
			if hasContentBefore(plains, i) {
				d.Signature = i
			}
			break
		}
	}
}

func afterFirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return ""
}

// nextMatches looks a few blocks ahead for an Outlook header line.
func nextMatches(plains []string, i int, re *regexp.Regexp) bool {
	for j := i + 1; j < len(plains) && j <= i+3; j++ {
		if re.MatchString(plains[j]) {
			return true
		}
	}
	return false
}

// onlyQuotedAfter reports whether every non-empty block from i on is
// quoted, i.e. this is bottom-quoted history, not an inline reply.
func onlyQuotedAfter(d *Doc, plains []string, i int) bool {
	for j := i; j < len(plains); j++ {
		if d.Blocks[j].Quote == 0 && plains[j] != "" {
			return false
		}
	}
	return true
}

func hasContentBefore(plains []string, i int) bool {
	for j := 0; j < i && j < len(plains); j++ {
		if plains[j] != "" {
			return true
		}
	}
	return false
}
