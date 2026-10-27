package mailsync

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/perlsaiyan/zorp/internal/store"
)

// Limits on images embedded into an exported message.
const (
	maxInlineImage = 10 << 20
	maxInlineTotal = 25 << 20
)

// exportCSP blocks everything a message could load or run in the browser:
// scripts, remote images (tracking), fonts, frames and form posts. Only
// images embedded as data: URIs (inline cid: images) are shown.
const exportCSP = `default-src 'none'; img-src data:; style-src 'unsafe-inline'; font-src data:; form-action 'none'; base-uri 'none'`

var (
	cidRe         = regexp.MustCompile(`(?i)cid:([^"'\s>)]+)`)
	metaRefreshRe = regexp.MustCompile(`(?i)<meta[^>]+http-equiv\s*=\s*["']?refresh[^>]*>`)
)

// ExportHTML writes a message to a standalone HTML file in dir and returns
// its path. Inline (cid:) images are embedded; remote content is blocked by
// a Content-Security-Policy.
func (e *Engine) ExportHTML(ctx context.Context, graphID, dir string) (string, error) {
	accountID, err := e.ensureAccount(ctx)
	if err != nil {
		return "", err
	}
	msg, err := e.store.MessageByGraphID(ctx, accountID, graphID)
	if err != nil {
		return "", err
	}
	body, fetched, err := e.store.MessageBody(ctx, msg.ID)
	if err != nil {
		return "", err
	}
	if !fetched {
		if err := e.FetchBody(ctx, graphID); err != nil {
			return "", err
		}
		if body, _, err = e.store.MessageBody(ctx, msg.ID); err != nil {
			return "", err
		}
	}

	var content string
	if body.Type == "html" {
		content = metaRefreshRe.ReplaceAllString(body.HTML, "")
		if cidRe.MatchString(content) {
			content, err = e.embedInlineImages(ctx, graphID, content)
			if err != nil {
				return "", err
			}
		}
	} else {
		content = `<pre style="white-space:pre-wrap;font:14px monospace">` + html.EscapeString(body.Text) + `</pre>`
	}

	var page strings.Builder
	page.WriteString(`<!doctype html><html><head><meta charset="utf-8">`)
	page.WriteString(`<meta http-equiv="Content-Security-Policy" content="` + exportCSP + `">`)
	page.WriteString(`<title>` + html.EscapeString(msg.Subject) + `</title></head><body>`)
	page.WriteString(`<div style="font:13px sans-serif;padding:8px 12px;margin-bottom:12px;border-bottom:1px solid #ccc;background:#f6f6f6;color:#222">`)
	header := func(k, v string) {
		if v != "" {
			page.WriteString(`<div><b>` + k + `:</b> ` + html.EscapeString(v) + `</div>`)
		}
	}
	header("From", addrString(msg.From))
	header("To", addrList(msg.To))
	header("Cc", addrList(msg.Cc))
	header("Date", msg.ReceivedAt.Local().Format("Mon, 02 Jan 2006 15:04"))
	header("Subject", msg.Subject)
	page.WriteString(`<div style="color:#888;margin-top:4px">Remote images and scripts are blocked.</div></div>`)
	page.WriteString(content)
	page.WriteString(`</body></html>`)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(graphID))
	path := filepath.Join(dir, hex.EncodeToString(sum[:8])+".html")
	if err := os.WriteFile(path, []byte(page.String()), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// embedInlineImages replaces cid: references with data: URIs of the
// message's image attachments.
func (e *Engine) embedInlineImages(ctx context.Context, graphID, content string) (string, error) {
	atts, err := e.graph.Attachments(ctx, graphID)
	if err != nil {
		return "", err
	}
	dataURIs := map[string]string{}
	total := 0
	for _, a := range atts {
		if !strings.HasPrefix(strings.ToLower(a.ContentType), "image/") || a.Size > maxInlineImage || total+a.Size > maxInlineTotal {
			continue
		}
		full, err := e.graph.Attachment(ctx, graphID, a.ID)
		if err != nil {
			e.log.Warn("fetching inline image", "attachment", a.Name, "err", err)
			continue
		}
		data, err := full.Content()
		if err != nil || full.ContentID == "" {
			continue
		}
		total += len(data)
		dataURIs[strings.ToLower(full.ContentID)] = "data:" + full.ContentType + ";base64," + base64.StdEncoding.EncodeToString(data)
	}
	return cidRe.ReplaceAllStringFunc(content, func(ref string) string {
		if uri, ok := dataURIs[strings.ToLower(ref[len("cid:"):])]; ok {
			return uri
		}
		return ref
	}), nil
}

func addrString(a store.Address) string {
	if a.Name != "" && a.Addr != "" {
		return fmt.Sprintf("%s <%s>", a.Name, a.Addr)
	}
	return a.Name + a.Addr
}

func addrList(as []store.Address) string {
	parts := make([]string, len(as))
	for i, a := range as {
		parts[i] = addrString(a)
	}
	return strings.Join(parts, ", ")
}
