package graph

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Attachment struct {
	ODataType   string `json:"@odata.type"` // e.g. "#microsoft.graph.fileAttachment"
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Size        int    `json:"size"`
	IsInline    bool   `json:"isInline"`
	ContentID   string `json:"contentId"`    // only on file attachments, when fetched individually
	ContentB64  string `json:"contentBytes"` // only on file attachments, when fetched individually
}

// Attachment kinds, from @odata.type.
const (
	KindFile      = "file"
	KindItem      = "item"      // an attached email (or event, contact…)
	KindReference = "reference" // a link to a file in OneDrive/SharePoint
)

// Kind is the attachment's kind: KindFile, KindItem or KindReference.
func (a *Attachment) Kind() string {
	t := strings.ToLower(a.ODataType)
	switch {
	case strings.HasSuffix(t, "itemattachment"):
		return KindItem
	case strings.HasSuffix(t, "referenceattachment"):
		return KindReference
	}
	return KindFile
}

// Content decodes the attachment's bytes.
func (a *Attachment) Content() ([]byte, error) {
	return base64.StdEncoding.DecodeString(a.ContentB64)
}

// attachmentSelect is what attachment lists ask for (not the contents).
const attachmentSelect = "id,name,contentType,size,isInline"

// Attachments lists a message's attachments without their contents.
func (c *Client) Attachments(ctx context.Context, messageID string) ([]Attachment, error) {
	var page struct {
		Value []Attachment `json:"value"`
	}
	path := "/me/messages/" + url.PathEscape(messageID) + "/attachments?$select=" + attachmentSelect
	if err := c.get(ctx, path, &page); err != nil {
		return nil, err
	}
	return page.Value, nil
}

// Attachment fetches one attachment including its contents and content ID.
func (c *Client) Attachment(ctx context.Context, messageID, attachmentID string) (*Attachment, error) {
	var a Attachment
	path := "/me/messages/" + url.PathEscape(messageID) + "/attachments/" + url.PathEscape(attachmentID)
	if err := c.get(ctx, path, &a); err != nil {
		return nil, err
	}
	a.ContentID = strings.Trim(a.ContentID, "<>")
	return &a, nil
}

// AttachedMessage is an email attached to another (an item attachment).
type AttachedMessage struct {
	Subject          string      `json:"subject"`
	From             *Recipient  `json:"from"`
	ToRecipients     []Recipient `json:"toRecipients"`
	CcRecipients     []Recipient `json:"ccRecipients"`
	SentDateTime     string      `json:"sentDateTime"`
	ReceivedDateTime string      `json:"receivedDateTime"`
	Body             ItemBody    `json:"body"`
}

// AttachedItem fetches an item attachment's message. ok is false when the
// item isn't an email (an event or contact, say).
func (c *Client) AttachedItem(ctx context.Context, messageID, attachmentID string) (msg *AttachedMessage, ok bool, err error) {
	var a struct {
		Item json.RawMessage `json:"item"`
	}
	path := "/me/messages/" + url.PathEscape(messageID) + "/attachments/" + url.PathEscape(attachmentID) +
		"?$expand=microsoft.graph.itemattachment/item"
	if err := c.get(ctx, path, &a); err != nil {
		return nil, false, err
	}
	var item struct {
		ODataType string `json:"@odata.type"`
		AttachedMessage
	}
	if err := json.Unmarshal(a.Item, &item); err != nil {
		return nil, false, err
	}
	if !strings.HasSuffix(strings.ToLower(item.ODataType), ".message") {
		return nil, false, nil
	}
	return &item.AttachedMessage, true, nil
}

// MaxRaw is the most AttachmentRaw reads.
const MaxRaw = 150 << 20

// AttachmentRaw fetches an attachment's raw contents ($value): the file's
// bytes, or for an attached email its MIME source (.eml).
func (c *Client) AttachmentRaw(ctx context.Context, messageID, attachmentID string) ([]byte, error) {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	u := c.baseURL + "/me/messages/" + url.PathEscape(messageID) + "/attachments/" + url.PathEscape(attachmentID) + "/$value"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Prefer", `IdType="ImmutableId"`)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, decodeError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxRaw+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxRaw {
		return nil, fmt.Errorf("attachment is larger than %d MB", MaxRaw>>20)
	}
	return data, nil
}
