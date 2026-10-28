package graph

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Draft is the editable part of an outgoing message.
type Draft struct {
	Subject string
	Body    string
	HTML    bool // Body is HTML, not plain text
	To      []EmailAddress
	Cc      []EmailAddress
	Bcc     []EmailAddress
}

func recipients(as []EmailAddress) []Recipient {
	out := make([]Recipient, 0, len(as))
	for _, a := range as {
		out = append(out, Recipient{EmailAddress: a})
	}
	return out
}

func (d Draft) fields() map[string]any {
	ct := "text"
	if d.HTML {
		ct = "html"
	}
	return map[string]any{
		"subject":       d.Subject,
		"body":          map[string]string{"contentType": ct, "content": d.Body},
		"toRecipients":  recipients(d.To),
		"ccRecipients":  recipients(d.Cc),
		"bccRecipients": recipients(d.Bcc),
	}
}

// SendMail sends a message without attachments and saves it to Sent
// Items. Like every send, it is never retried after an error that might
// mean it went out.
func (c *Client) SendMail(ctx context.Context, d Draft) error {
	return c.postOnce(ctx, "/me/sendMail", map[string]any{"message": d.fields(), "saveToSentItems": true}, nil)
}

// CreateDraft creates a new draft in Drafts and returns its ID.
func (c *Client) CreateDraft(ctx context.Context, d Draft) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := c.postOnce(ctx, "/me/messages", d.fields(), &out)
	return out.ID, err
}

// Reply kinds for CreateResponse.
const (
	ReplyOne = "createReply"
	ReplyAll = "createReplyAll"
	Forward  = "createForward"
)

// CreateResponse creates a reply, reply-all or forward draft of a message,
// which carries the threading headers (and, for forwards, the original's
// attachments). It returns the draft's ID.
func (c *Client) CreateResponse(ctx context.Context, messageID, kind string) (string, error) {
	switch kind {
	case ReplyOne, ReplyAll, Forward:
	default:
		return "", fmt.Errorf("graph: unknown response kind %q", kind)
	}
	var out struct {
		ID string `json:"id"`
	}
	err := c.postOnce(ctx, "/me/messages/"+url.PathEscape(messageID)+"/"+kind, map[string]any{}, &out)
	return out.ID, err
}

// UpdateDraft replaces a draft's subject, body and recipients.
func (c *Client) UpdateDraft(ctx context.Context, id string, d Draft) error {
	return c.patch(ctx, "/me/messages/"+url.PathEscape(id), d.fields(), nil)
}

// DraftMessage is a draft as stored on the server.
type DraftMessage struct {
	ID            string      `json:"id"`
	ChangeKey     string      `json:"changeKey"`
	Subject       string      `json:"subject"`
	IsDraft       bool        `json:"isDraft"`
	Body          ItemBody    `json:"body"`
	ToRecipients  []Recipient `json:"toRecipients"`
	CcRecipients  []Recipient `json:"ccRecipients"`
	BccRecipients []Recipient `json:"bccRecipients"`
}

// GetDraft fetches a draft's editable fields.
func (c *Client) GetDraft(ctx context.Context, id string) (*DraftMessage, error) {
	var d DraftMessage
	path := "/me/messages/" + url.PathEscape(id) + "?$select=id,changeKey,subject,isDraft,body,toRecipients,ccRecipients,bccRecipients"
	if err := c.get(ctx, path, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// SendDraft sends a draft. It is never retried after an error that might
// mean it went out.
func (c *Client) SendDraft(ctx context.Context, id string) error {
	return c.postOnce(ctx, "/me/messages/"+url.PathEscape(id)+"/send", map[string]any{}, nil)
}

// DeleteAttachment removes an attachment from a draft.
func (c *Client) DeleteAttachment(ctx context.Context, messageID, attachmentID string) error {
	return c.do(ctx, http.MethodDelete, "/me/messages/"+url.PathEscape(messageID)+"/attachments/"+url.PathEscape(attachmentID), nil, nil)
}

// Attachments up to smallAttachment bytes are posted in one request; larger
// ones go through an upload session, in chunks (a multiple of 320 KiB, as
// Graph requires).
const (
	smallAttachment = 3 << 20
	uploadChunk     = 10 * 320 << 10
	MaxAttachment   = 150 << 20
)

// AddAttachment attaches a file to a draft.
func (c *Client) AddAttachment(ctx context.Context, messageID, name, contentType string, data []byte) error {
	return c.addAttachment(ctx, messageID, name, contentType, "", data)
}

// AddInlineAttachment attaches an image that the draft's HTML shows with
// <img src="cid:contentID">.
func (c *Client) AddInlineAttachment(ctx context.Context, messageID, name, contentType, contentID string, data []byte) error {
	return c.addAttachment(ctx, messageID, name, contentType, contentID, data)
}

func (c *Client) addAttachment(ctx context.Context, messageID, name, contentType, contentID string, data []byte) error {
	if len(data) > MaxAttachment {
		return fmt.Errorf("%s is larger than 150 MB", name)
	}
	base := "/me/messages/" + url.PathEscape(messageID) + "/attachments"
	if len(data) <= smallAttachment {
		att := map[string]any{
			"@odata.type":  "#microsoft.graph.fileAttachment",
			"name":         name,
			"contentType":  contentType,
			"contentBytes": base64.StdEncoding.EncodeToString(data),
		}
		if contentID != "" {
			att["isInline"], att["contentId"] = true, contentID
		}
		return c.postOnce(ctx, base, att, nil)
	}
	var session struct {
		UploadURL string `json:"uploadUrl"`
	}
	item := map[string]any{"attachmentType": "file", "name": name, "size": len(data), "contentType": contentType}
	if contentID != "" {
		item["isInline"], item["contentId"] = true, contentID
	}
	err := c.postOnce(ctx, base+"/createUploadSession", map[string]any{"AttachmentItem": item}, &session)
	if err != nil {
		return err
	}
	if session.UploadURL == "" {
		return fmt.Errorf("graph: no upload URL for %s", name)
	}
	for off := 0; off < len(data); off += uploadChunk {
		end := min(off+uploadChunk, len(data))
		if err := c.uploadChunk(ctx, session.UploadURL, data[off:end], off, len(data)); err != nil {
			return fmt.Errorf("uploading %s: %w", name, err)
		}
	}
	return nil
}

// uploadChunk PUTs one range to an upload session. The upload URL is
// pre-authorized (and on another host), so no bearer token is sent.
func (c *Client) uploadChunk(ctx context.Context, uploadURL string, chunk []byte, off, total int) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(chunk))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(chunk))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, off+len(chunk)-1, total))
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("upload session: %s: %s", resp.Status, bytes.TrimSpace(data))
	}
	return nil
}
