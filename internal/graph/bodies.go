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

// MessageBody fetches one message's body in its original format.
func (c *Client) MessageBody(ctx context.Context, id string) (*ItemBody, error) {
	var m struct {
		Body ItemBody `json:"body"`
	}
	if err := c.get(ctx, "/me/messages/"+url.PathEscape(id)+"?$select=body", &m); err != nil {
		return nil, err
	}
	return &m.Body, nil
}

// MaxBatch is the most requests Graph accepts in one $batch call.
const MaxBatch = 20

// MessageBodies fetches up to MaxBatch message bodies (original format) in one
// $batch request. The result maps message ID to body; IDs whose individual
// request failed (e.g. throttled or deleted) are left out, to be retried later.
func (c *Client) MessageBodies(ctx context.Context, ids []string) (map[string]ItemBody, error) {
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
			URL:    "/me/messages/" + url.PathEscape(id) + "?$select=body",
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
	out := make(map[string]ItemBody, len(ids))
	for _, r := range resp.Responses {
		i, err := strconv.Atoi(r.ID)
		if err != nil || i < 0 || i >= len(ids) || r.Status/100 != 2 {
			continue
		}
		var m struct {
			Body ItemBody `json:"body"`
		}
		if json.Unmarshal(r.Body, &m) == nil {
			out[ids[i]] = m.Body
		}
	}
	return out, nil
}
