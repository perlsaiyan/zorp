package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
			if !strings.Contains(p, `IdType="ImmutableId"`) {
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

func TestSearchMessagesQuotesAndPages(t *testing.T) {
	var srv string
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			if got := r.URL.Query().Get("$search"); got != `"from:alice \"q3 report\" a\\b"` {
				t.Errorf("$search = %q", got)
			}
			w.Write([]byte(`{"value":[{"id":"1"},{"id":"2"}],"@odata.nextLink":"` + srv + `/me/messages?page=2"}`))
			return
		}
		w.Write([]byte(`{"value":[{"id":"3"},{"id":"4"}],"@odata.nextLink":"` + srv + `/me/messages?page=3"}`))
	})
	srv = c.baseURL
	msgs, err := c.SearchMessages(context.Background(), `from:alice "q3 report" a\b`, 3)
	if err != nil || len(msgs) != 3 || calls != 2 {
		t.Fatalf("got %d messages in %d calls, %v", len(msgs), calls, err)
	}
}

func TestLargeAttachmentUsesUploadSessionWithoutToken(t *testing.T) {
	upload := httptest.NewServer(nil)
	t.Cleanup(upload.Close)
	var ranges []string
	upload.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("bearer token sent to the upload URL")
		}
		ranges = append(ranges, r.Header.Get("Content-Range"))
		w.WriteHeader(http.StatusOK)
	})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/messages/d1/attachments/createUploadSession" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Write([]byte(`{"uploadUrl":"` + upload.URL + `/up"}`))
	})
	data := make([]byte, smallAttachment+uploadChunk+5)
	if err := c.AddAttachment(context.Background(), "d1", "big.bin", "application/octet-stream", data); err != nil {
		t.Fatal(err)
	}
	n := len(data)
	var want []string
	for off := 0; off < n; off += uploadChunk {
		want = append(want, fmt.Sprintf("bytes %d-%d/%d", off, min(off+uploadChunk, n)-1, n))
	}
	if len(want) < 2 {
		t.Fatal("test data should need several chunks")
	}
	if strings.Join(ranges, ";") != strings.Join(want, ";") {
		t.Errorf("ranges = %v, want %v", ranges, want)
	}
}

func TestSendIsNotRetriedAfterServerError(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests) // rejected before processing: safe to retry
			return
		}
		w.WriteHeader(http.StatusBadGateway) // may have been sent: don't retry
	})
	if err := c.SendDraft(context.Background(), "d1"); err == nil || calls != 2 {
		t.Errorf("err=%v calls=%d, want an error after 2 calls", err, calls)
	}
}
