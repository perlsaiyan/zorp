package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

type ItemBody struct {
	ContentType string `json:"contentType"` // "text" or "html"
	Content     string `json:"content"`
}

// MessageContent is a message's body in its original format, and its
// attachments (without their contents).
type MessageContent struct {
	Body        ItemBody     `json:"body"`
	Attachments []Attachment `json:"attachments"`
}

// contentQuery selects a message's body and expands its attachment list.
const contentQuery = "?$select=body&$expand=attachments($select=" + attachmentSelect + ")"

// MessageBody fetches one message's body and attachment list.
func (c *Client) MessageBody(ctx context.Context, id string) (*MessageContent, error) {
	var m MessageContent
	if err := c.get(ctx, "/me/messages/"+url.PathEscape(id)+contentQuery, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// MaxBatch is the most requests Graph accepts in one $batch call.
const MaxBatch = 20

// MessageBodies fetches up to MaxBatch message bodies (original format) and
// attachment lists in one $batch request. The result maps message ID to
// content; IDs whose individual
// request failed (e.g. throttled or deleted) are left out, to be retried later.
func (c *Client) MessageBodies(ctx context.Context, ids []string) (map[string]MessageContent, error) {
	if len(ids) > MaxBatch {
		return nil, fmt.Errorf("graph: %d requests exceeds batch limit of %d", len(ids), MaxBatch)
	}
	type subRequest struct {
		ID      string            `json:"id"`
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	req := struct {
		Requests []subRequest `json:"requests"`
	}{}
	for i, id := range ids {
		req.Requests = append(req.Requests, subRequest{
			ID:     strconv.Itoa(i),
			Method: "GET",
			URL:    "/me/messages/" + url.PathEscape(id) + contentQuery,
			// Headers on the outer request don't apply to the parts.
			Headers: map[string]string{"Prefer": `IdType="ImmutableId"`},
		})
	}
	var resp struct {
		Responses []struct {
			ID     string          `json:"id"`
			Status int             `json:"status"`
			Body   json.RawMessage `json:"body"`
		} `json:"responses"`
	}
	if err := c.post(ctx, "/$batch", req, &resp); err != nil {
		return nil, err
	}
	out := make(map[string]MessageContent, len(ids))
	for _, r := range resp.Responses {
		i, err := strconv.Atoi(r.ID)
		if err != nil || i < 0 || i >= len(ids) || r.Status/100 != 2 {
			continue
		}
		var m MessageContent
		if json.Unmarshal(r.Body, &m) == nil {
			out[ids[i]] = m
		}
	}
	return out, nil
}
