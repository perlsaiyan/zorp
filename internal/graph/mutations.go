package graph

import (
	"context"
	"net/url"
)

// SetRead marks a message read or unread.
func (c *Client) SetRead(ctx context.Context, id string, read bool) error {
	return c.patch(ctx, "/me/messages/"+url.PathEscape(id), map[string]any{"isRead": read}, nil)
}

// SetFlagged flags or unflags a message.
func (c *Client) SetFlagged(ctx context.Context, id string, flagged bool) error {
	status := "notFlagged"
	if flagged {
		status = "flagged"
	}
	return c.patch(ctx, "/me/messages/"+url.PathEscape(id), map[string]any{"flag": map[string]string{"flagStatus": status}}, nil)
}

// Move moves a message to another folder and returns its ID afterwards.
// With immutable IDs (which every request asks for) it is unchanged.
func (c *Client) Move(ctx context.Context, id, destinationFolderID string) (string, error) {
	var moved struct {
		ID string `json:"id"`
	}
	err := c.post(ctx, "/me/messages/"+url.PathEscape(id)+"/move", map[string]string{"destinationId": destinationFolderID}, &moved)
	return moved.ID, err
}
