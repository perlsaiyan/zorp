package graph

import (
	"context"
	"encoding/base64"
	"net/url"
	"strings"
)

type Attachment struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Size        int    `json:"size"`
	IsInline    bool   `json:"isInline"`
	ContentID   string `json:"contentId"`    // only on file attachments, when fetched individually
	ContentB64  string `json:"contentBytes"` // only on file attachments, when fetched individually
}

// Content decodes the attachment's bytes.
func (a *Attachment) Content() ([]byte, error) {
	return base64.StdEncoding.DecodeString(a.ContentB64)
}

// Attachments lists a message's attachments without their contents.
func (c *Client) Attachments(ctx context.Context, messageID string) ([]Attachment, error) {
	var page struct {
		Value []Attachment `json:"value"`
	}
	path := "/me/messages/" + url.PathEscape(messageID) + "/attachments?$select=id,name,contentType,size,isInline"
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
