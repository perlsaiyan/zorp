package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/compose"
	"github.com/perlsaiyan/zorp/internal/graph"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/mailsync"
)

// compose sends a message or saves a draft. Safe mode is enforced here so
// it covers every client: sending is only allowed to the user's own address.
func (d *daemon) compose(ctx context.Context, params json.RawMessage) (ipc.ComposeResult, error) {
	var p ipc.ComposeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return ipc.ComposeResult{}, err
	}
	kind := compose.Kind(p.Kind)
	switch kind {
	case compose.New:
	case compose.Reply, compose.ReplyAll, compose.Forward, compose.EditDraft:
		if p.Ref == "" {
			return ipc.ComposeResult{}, fmt.Errorf("%s needs the message it refers to", kind)
		}
	default:
		return ipc.ComposeResult{}, fmt.Errorf("unknown message kind %q", p.Kind)
	}
	if p.Send {
		if len(p.To)+len(p.Cc)+len(p.Bcc) == 0 {
			return ipc.ComposeResult{}, errors.New("no recipients")
		}
		if err := d.checkRecipients(ctx, p); err != nil {
			return ipc.ComposeResult{}, err
		}
	}
	switch p.Format {
	case "", compose.FormatText, compose.FormatMarkdown:
	default:
		return ipc.ComposeResult{}, fmt.Errorf("unknown format %q", p.Format)
	}
	id, err := d.engine.Compose(ctx, mailsync.Outgoing{
		Kind:   kind,
		Format: p.Format,
		Ref:    p.Ref,
		Draft: graph.Draft{
			Subject: p.Subject,
			Body:    p.Body,
			To:      toGraph(p.To),
			Cc:      toGraph(p.Cc),
			Bcc:     toGraph(p.Bcc),
		},
		Attach: p.Attach,
		Keep:   p.Keep,
		Send:   p.Send,
	})
	var kept *mailsync.DraftKept
	if err == nil || errors.As(err, &kept) {
		// Sent Items or Drafts changed.
		select {
		case d.kick <- struct{}{}:
		default:
		}
	}
	what := "draft saved"
	if p.Send {
		what = "message sent"
	}
	if err != nil {
		d.log.Warn("compose failed", "kind", kind, "send", p.Send, "err", err)
	} else {
		d.log.Info(what, "kind", kind, "recipients", len(p.To)+len(p.Cc)+len(p.Bcc), "attachments", len(p.Attach))
	}
	if kept != nil {
		return ipc.ComposeResult{DraftID: kept.ID, SendError: kept.Error()}, nil
	}
	return ipc.ComposeResult{DraftID: id}, err
}

// checkRecipients applies the send guardrail: unless sending is allowed,
// every recipient must be the user or on safety.send_allow.
func (d *daemon) checkRecipients(ctx context.Context, p ipc.ComposeParams) error {
	if d.cfg.Safety.SendAllowed() {
		return nil
	}
	acct, err := d.store.DefaultAccount(ctx)
	if err != nil {
		return fmt.Errorf("guardrail: can't check recipients before the first sync: %w", err)
	}
	var blocked []string
	for _, list := range [][]ipc.Address{p.To, p.Cc, p.Bcc} {
		for _, a := range list {
			if !d.cfg.Safety.MaySendTo(a.Addr, acct.UPN) {
				blocked = append(blocked, a.Addr)
			}
		}
	}
	if len(blocked) > 0 {
		return fmt.Errorf("guardrail: sending is limited to yourself and safety.send_allow; not allowed: %s (save it as a draft, or set safety.allow_send = true)", strings.Join(blocked, ", "))
	}
	return nil
}

func toGraph(as []ipc.Address) []graph.EmailAddress {
	out := make([]graph.EmailAddress, 0, len(as))
	for _, a := range as {
		out = append(out, graph.EmailAddress{Name: a.Name, Address: a.Addr})
	}
	return out
}

func fromGraph(as []graph.EmailAddress) []ipc.Address {
	out := make([]ipc.Address, 0, len(as))
	for _, a := range as {
		out = append(out, ipc.Address{Name: a.Name, Addr: a.Address})
	}
	return out
}

func attachmentInfo(as []graph.Attachment) []ipc.AttachmentInfo {
	out := make([]ipc.AttachmentInfo, 0, len(as))
	for _, a := range as {
		out = append(out, ipc.AttachmentInfo{ID: a.ID, Name: a.Name, ContentType: a.ContentType, Size: a.Size, IsInline: a.IsInline})
	}
	return out
}

func (d *daemon) getDraft(ctx context.Context, params json.RawMessage) (ipc.DraftInfo, error) {
	var p ipc.MessageRef
	if err := json.Unmarshal(params, &p); err != nil || p.GraphID == "" {
		return ipc.DraftInfo{}, errors.New("get-draft needs a graph_id")
	}
	dd, err := d.engine.GetDraft(ctx, p.GraphID)
	if err != nil {
		return ipc.DraftInfo{}, err
	}
	return ipc.DraftInfo{
		Subject:     dd.Draft.Subject,
		To:          fromGraph(dd.Draft.To),
		Cc:          fromGraph(dd.Draft.Cc),
		Bcc:         fromGraph(dd.Draft.Bcc),
		Body:        dd.Draft.Body,
		Format:      dd.Format,
		WasHTML:     dd.WasHTML,
		Attachments: attachmentInfo(dd.Attachments),
	}, nil
}

func (d *daemon) listAttachments(ctx context.Context, params json.RawMessage) ([]ipc.AttachmentInfo, error) {
	var p ipc.MessageRef
	if err := json.Unmarshal(params, &p); err != nil || p.GraphID == "" {
		return nil, errors.New("attachments needs a graph_id")
	}
	atts, err := d.engine.Attachments(ctx, p.GraphID)
	return attachmentInfo(atts), err
}

func (d *daemon) images(ctx context.Context, params json.RawMessage) ([]ipc.ImageInfo, error) {
	var p ipc.MessageRef
	if err := json.Unmarshal(params, &p); err != nil || p.GraphID == "" {
		return nil, errors.New("images needs a graph_id")
	}
	files, err := d.engine.Images(ctx, p.GraphID, d.imageDir())
	out := make([]ipc.ImageInfo, 0, len(files))
	for _, f := range files {
		out = append(out, ipc.ImageInfo{ContentID: f.ContentID, Name: f.Name, Inline: f.Inline, Path: f.Path})
	}
	return out, err
}

func (d *daemon) imageDir() string { return filepath.Join(d.cfg.Paths.DataDir, "images") }

func (d *daemon) saveAttachment(ctx context.Context, params json.RawMessage) (ipc.SaveAttachmentResult, error) {
	var p ipc.SaveAttachmentParams
	if err := json.Unmarshal(params, &p); err != nil || p.GraphID == "" || p.AttachmentID == "" {
		return ipc.SaveAttachmentResult{}, errors.New("save-attachment needs a graph_id and attachment_id")
	}
	path, err := d.engine.SaveAttachment(ctx, p.GraphID, p.AttachmentID, config.ExpandHome(d.cfg.UI.SaveDir))
	return ipc.SaveAttachmentResult{Path: path}, err
}
