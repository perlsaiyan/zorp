// Package graph is a small typed client for the Microsoft Graph mail API.
package graph

import (
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

// IsNotFound reports whether err is a Graph 404.
func IsNotFound(err error) bool {
	var gerr *Error
	return errors.As(err, &gerr) && gerr.Status == http.StatusNotFound
}

// get fetches path (relative to the base URL, or an absolute URL such as a
// nextLink) and decodes the JSON response into v. prefer adds values to the
// Prefer header. It retries 429 (honoring Retry-After), 5xx, and network
// errors with backoff.
func (c *Client) get(ctx context.Context, path string, v any, prefer ...string) error {
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
		retryAfter, err := c.doGet(ctx, url, v, prefer)
		if err == nil {
			return nil
		}
		lastErr = err
		if retryAfter < 0 {
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

// doGet performs one attempt. retryAfter < 0 means don't retry; 0 means use backoff.
func (c *Client) doGet(ctx context.Context, url string, v any, prefer []string) (retryAfter time.Duration, err error) {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return -1, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return -1, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
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
