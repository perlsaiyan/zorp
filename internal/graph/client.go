// Package graph is a small typed client for the Microsoft Graph mail API.
package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"
)

const BaseURL = "https://graph.microsoft.com/v1.0"

// TokenSource returns a current access token.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

type Client struct {
	http    *http.Client
	tokens  TokenSource
	baseURL string
}

type Option func(*Client)

// WithBaseURL points the client at a different Graph root (for tests).
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

func New(tokens TokenSource, opts ...Option) *Client {
	c := &Client{
		http:    &http.Client{Timeout: 30 * time.Second},
		tokens:  tokens,
		baseURL: BaseURL,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Error is a non-2xx Graph response.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("graph: %d %s: %s", e.Status, e.Code, e.Message)
}

const maxAttempts = 5

// IsPermanent reports whether retrying err can't help: a 4xx response other
// than throttling or a timeout. Network errors, 5xx and 429 are transient.
func IsPermanent(err error) bool {
	var gerr *Error
	if !errors.As(err, &gerr) {
		return false
	}
	return gerr.Status >= 400 && gerr.Status < 500 &&
		gerr.Status != http.StatusTooManyRequests && gerr.Status != http.StatusRequestTimeout
}

// IsNotFound reports whether err is a Graph 404.
func IsNotFound(err error) bool {
	var gerr *Error
	return errors.As(err, &gerr) && gerr.Status == http.StatusNotFound
}

// get fetches path (relative to the base URL, or an absolute URL such as a
// nextLink) and decodes the JSON response into v. prefer adds values to the
// Prefer header.
func (c *Client) get(ctx context.Context, path string, v any, prefer ...string) error {
	return c.do(ctx, http.MethodGet, path, nil, v, prefer...)
}

// post sends body as JSON to path and decodes the JSON response into v.
func (c *Client) post(ctx context.Context, path string, body, v any, prefer ...string) error {
	return c.do(ctx, http.MethodPost, path, body, v, prefer...)
}

// patch sends body as JSON to path with PATCH.
func (c *Client) patch(ctx context.Context, path string, body, v any) error {
	return c.do(ctx, http.MethodPatch, path, body, v)
}

// do performs a request, retrying 429 (honoring Retry-After), 5xx, and
// network errors with backoff.
func (c *Client) do(ctx context.Context, method, path string, body, v any, prefer ...string) error {
	return c.doRetry(ctx, true, method, path, body, v, prefer...)
}

// postOnce is post for requests that must not be repeated, such as sending
// mail: after a 5xx or network error the request may already have taken
// effect, so only 429 (rejected before processing) is retried.
func (c *Client) postOnce(ctx context.Context, path string, body, v any) error {
	return c.doRetry(ctx, false, http.MethodPost, path, body, v)
}

func (c *Client) doRetry(ctx context.Context, idempotent bool, method, path string, body, v any, prefer ...string) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	url := path
	if !strings.HasPrefix(path, "https://") && !strings.HasPrefix(path, "http://") {
		url = c.baseURL + path
	} else if !sameOrigin(path, c.baseURL) {
		// Absolute URLs come from Graph (nextLink/deltaLink); never send the
		// bearer token anywhere else.
		return fmt.Errorf("graph: refusing to follow link to another host: %s", path)
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		retryAfter, err := c.attempt(ctx, method, url, payload, v, prefer)
		if err == nil {
			return nil
		}
		lastErr = err
		if retryAfter < 0 || (!idempotent && !isThrottled(err)) {
			return err
		}
		if retryAfter == 0 {
			retryAfter = time.Duration(1<<attempt) * time.Second
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryAfter):
		}
	}
	return lastErr
}

// attempt performs one try. retryAfter < 0 means don't retry; 0 means use backoff.
func (c *Client) attempt(ctx context.Context, method, url string, payload []byte, v any, prefer []string) (retryAfter time.Duration, err error) {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return -1, err
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return -1, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Immutable IDs keep message IDs stable across folder moves. Every request
	// must send this; see PLAN.md section 5.
	req.Header.Set("Prefer", strings.Join(append([]string{`IdType="ImmutableId"`}, prefer...), ", "))

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return -1, ctx.Err()
		}
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 == 2 {
		if v == nil || resp.StatusCode == http.StatusNoContent {
			return 0, nil
		}
		return 0, json.NewDecoder(resp.Body).Decode(v)
	}

	gerr := decodeError(resp)
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return parseRetryAfter(resp.Header.Get("Retry-After")), gerr
	case resp.StatusCode >= 500:
		return 0, gerr
	default:
		return -1, gerr
	}
}

func isThrottled(err error) bool {
	var gerr *Error
	return errors.As(err, &gerr) && gerr.Status == http.StatusTooManyRequests
}

func sameOrigin(a, b string) bool {
	ua, err1 := neturl.Parse(a)
	ub, err2 := neturl.Parse(b)
	return err1 == nil && err2 == nil && ua.Scheme == ub.Scheme && ua.Host == ub.Host
}

func decodeError(resp *http.Response) *Error {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = json.Unmarshal(data, &body)
	return &Error{Status: resp.StatusCode, Code: body.Error.Code, Message: body.Error.Message}
}

func parseRetryAfter(h string) time.Duration {
	if secs, err := strconv.Atoi(h); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}
