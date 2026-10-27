package graph

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(staticToken("tok"))
	c.baseURL = srv.URL
	return c
}

func TestMeSendsHeadersAndHandlesNullFrom(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Prefer"); got != `IdType="ImmutableId"` {
			t.Errorf("Prefer = %q", got)
		}
		switch r.URL.Path {
		case "/me":
			w.Write([]byte(`{"displayName":"Tom","mail":null,"userPrincipalName":"tom@example.com"}`))
		case "/me/mailFolders/inbox/messages":
			w.Write([]byte(`{"value":[{"id":"1","subject":"hi","from":null,"receivedDateTime":"2026-10-27T10:00:00Z","isRead":false}]}`))
		}
	})
	me, err := c.Me(context.Background())
	if err != nil || me.UserPrincipalName != "tom@example.com" {
		t.Fatalf("Me = %+v, %v", me, err)
	}
	msgs, err := c.RecentInbox(context.Background(), 10)
	if err != nil || len(msgs) != 1 || msgs[0].From != nil {
		t.Fatalf("RecentInbox = %+v, %v", msgs, err)
	}
}

func TestRetriesOn429ThenSucceeds(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"displayName":"Tom"}`))
	})
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestDoesNotRetry403(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"code":"ErrorAccessDenied","message":"Access is denied."}}`))
	})
	_, err := c.Me(context.Background())
	var gerr *Error
	if !errors.As(err, &gerr) || gerr.Status != 403 || gerr.Code != "ErrorAccessDenied" {
		t.Fatalf("err = %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestMessageBodiesBatch(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/$batch" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		var req struct {
			Requests []struct {
				ID      string            `json:"id"`
				URL     string            `json:"url"`
				Headers map[string]string `json:"headers"`
			} `json:"requests"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Requests) != 3 {
			t.Fatalf("batch body: %v, %d requests", err, len(req.Requests))
		}
		for _, sub := range req.Requests {
			p := sub.Headers["Prefer"]
			if !strings.Contains(p, `IdType="ImmutableId"`) || !strings.Contains(p, `outlook.body-content-type="text"`) {
				t.Errorf("sub-request %s Prefer = %q", sub.ID, p)
			}
		}
		// Responses arrive out of order; one is throttled.
		w.Write([]byte(`{"responses":[
			{"id":"2","status":200,"body":{"body":{"contentType":"text","content":"third"}}},
			{"id":"0","status":200,"body":{"body":{"contentType":"text","content":"first"}}},
			{"id":"1","status":429,"body":{"error":{"code":"TooManyRequests"}}}]}`))
	})
	got, err := c.MessageBodies(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["a"].Content != "first" || got["c"].Content != "third" {
		t.Errorf("bodies = %+v", got)
	}
	if _, err := c.MessageBodies(context.Background(), make([]string, MaxBatch+1)); err == nil {
		t.Error("oversized batch accepted")
	}
}
