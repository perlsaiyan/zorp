package render

import (
	"net/url"
	"regexp"
	"strings"
)

// Check Point's email security rewrites links as
// https://protect.checkpoint.com/v2/r01/___<original>___.<signature>
var checkpointRe = regexp.MustCompile(`^https?://protect\.checkpoint\.com/v\d+/r\d+/___(.+?)___\.`)

// DisplayURL returns the link's real destination for display, unwrapping
// security-gateway rewrites (Check Point, Microsoft Safe Links). Opening a
// link should still use the original URL, so the gateway can check it.
func DisplayURL(raw string) string {
	if m := checkpointRe.FindStringSubmatch(raw); m != nil && looksLikeURL(m[1]) {
		return m[1]
	}
	if u, err := url.Parse(raw); err == nil && strings.HasSuffix(strings.ToLower(u.Host), "safelinks.protection.outlook.com") {
		if inner := u.Query().Get("url"); looksLikeURL(inner) {
			return inner
		}
	}
	return raw
}

func looksLikeURL(s string) bool {
	l := strings.ToLower(s)
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "mailto:")
}

// Host is the host part of a link's display URL ("" for mailto and junk).
func Host(raw string) string {
	u, err := url.Parse(DisplayURL(raw))
	if err != nil {
		return ""
	}
	return u.Host
}
