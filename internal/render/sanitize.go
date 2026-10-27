package render

import (
	"strings"
	"unicode"
)

// Invisible reports characters dropped from untrusted text: control
// characters (including the ESC that starts terminal escape sequences) and
// Unicode format characters (soft hyphens, zero-width joiners, bidi
// overrides), which terminals disagree about the width of and which
// marketing mail uses as invisible padding.
func Invisible(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '͏'
}

// Sanitize removes invisible characters. With keepNewlines, "\n" survives
// and tabs become four spaces; otherwise newlines and tabs become spaces.
func Sanitize(s string, keepNewlines bool) string {
	if keepNewlines {
		s = strings.ReplaceAll(s, "\t", "    ")
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' && keepNewlines:
			return r
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case Invisible(r):
			return -1
		}
		return r
	}, s)
}
