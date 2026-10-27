package graph

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
