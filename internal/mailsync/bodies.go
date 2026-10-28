package mailsync

import (
	"context"
	"regexp"
	"strings"

	"github.com/perlsaiyan/zorp/internal/graph"
	"github.com/perlsaiyan/zorp/internal/render"
	"github.com/perlsaiyan/zorp/internal/store"
)

// FetchBody fetches and stores one message's body right away (the user is
// looking at it).
func (e *Engine) FetchBody(ctx context.Context, graphID string) error {
	accountID, err := e.ensureAccount(ctx)
	if err != nil {
		return err
	}
	c, err := e.graph.MessageBody(ctx, graphID)
	if err != nil {
		return err
	}
	return e.storeContent(ctx, accountID, graphID, *c)
}

// storeContent caches a message's body and attachment list.
func (e *Engine) storeContent(ctx context.Context, accountID int64, graphID string, c graph.MessageContent) error {
	if err := e.store.SetBody(ctx, accountID, graphID, toStoreBody(c.Body)); err != nil {
		return err
	}
	return e.store.SetAttachments(ctx, accountID, graphID, toStoreAttachments(c.Attachments))
}

func toStoreAttachments(as []graph.Attachment) []store.Attachment {
	out := make([]store.Attachment, 0, len(as))
	for _, a := range as {
		out = append(out, store.Attachment{
			GraphID: a.ID, Name: a.Name, ContentType: a.ContentType, Kind: a.Kind(),
			Size: int64(a.Size), IsInline: a.IsInline,
		})
	}
	return out
}

// toStoreBody keeps HTML as sent, plus a plain-text rendering for search.
func toStoreBody(b graph.ItemBody) store.Body {
	if strings.EqualFold(b.ContentType, "html") {
		return store.Body{Type: "html", HTML: b.Content, Text: render.FromHTML(b.Content).PlainText()}
	}
	return store.Body{Type: "text", Text: normalizeText(b.Content)}
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
		for gid, c := range bodies {
			if err := e.storeContent(ctx, accountID, gid, c); err != nil {
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
