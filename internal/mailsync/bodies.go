package mailsync

import (
	"context"
	"regexp"
	"strings"

	"github.com/perlsaiyan/zorp/internal/graph"
)

// FetchBody fetches and stores one message's body right away (the user is
// looking at it).
func (e *Engine) FetchBody(ctx context.Context, graphID string) error {
	accountID, err := e.ensureAccount(ctx)
	if err != nil {
		return err
	}
	body, err := e.graph.MessageBody(ctx, graphID)
	if err != nil {
		return err
	}
	return e.store.SetBody(ctx, accountID, graphID, normalizeText(body.Content))
}

// PrefetchBodies fetches bodies for the account's newest `window` messages
// that don't have one yet, in $batch calls. It returns how many it stored.
// Individual failures are skipped and retried on the next call.
func (e *Engine) PrefetchBodies(ctx context.Context, window int) (int, error) {
	if window <= 0 {
		return 0, nil
	}
	accountID, err := e.ensureAccount(ctx)
	if err != nil {
		return 0, err
	}
	ids, err := e.store.MessagesNeedingBodies(ctx, accountID, window, window)
	if err != nil {
		return 0, err
	}
	stored := 0
	for len(ids) > 0 {
		n := min(len(ids), graph.MaxBatch)
		bodies, err := e.graph.MessageBodies(ctx, ids[:n])
		if err != nil {
			return stored, err
		}
		for gid, b := range bodies {
			if err := e.store.SetBody(ctx, accountID, gid, normalizeText(b.Content)); err != nil {
				return stored, err
			}
			stored++
		}
		ids = ids[n:]
	}
	return stored, nil
}

var blankRuns = regexp.MustCompile(`\n{3,}`)

// normalizeText tidies Graph's HTML-to-text output: Unix newlines, no
// trailing spaces, and at most one blank line in a row.
func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t ")
	}
	s = strings.Join(lines, "\n")
	return strings.TrimSpace(blankRuns.ReplaceAllString(s, "\n\n"))
}
