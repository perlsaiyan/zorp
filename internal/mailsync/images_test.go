package mailsync

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/perlsaiyan/zorp/internal/graph"
)

func TestImagesCachesImageAttachments(t *testing.T) {
	fetches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me/messages/m1/attachments":
			io.WriteString(w, `{"value":[
				{"id":"a1","name":"logo.png","contentType":"image/png","size":100,"isInline":true},
				{"id":"a2","name":"report.pdf","contentType":"application/pdf","size":100},
				{"id":"a3","name":"huge.jpg","contentType":"image/jpeg","size":99999999},
				{"id":"a4","name":"photo.jpg","contentType":"image/jpeg","size":100}]}`)
		case "/me/messages/m1/attachments/a1":
			fetches++
			io.WriteString(w, `{"id":"a1","contentId":"<image001.png@01DA>","contentBytes":"cG5n"}`)
		case "/me/messages/m1/attachments/a4":
			fetches++
			io.WriteString(w, `{"id":"a4","contentBytes":"anBn"}`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	e := New(graph.New(staticToken{}, graph.WithBaseURL(srv.URL)), nil, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	dir := t.TempDir()
	for range 2 {
		imgs, err := e.Images(context.Background(), "m1", dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(imgs) != 2 || imgs[0].ContentID != "image001.png@01DA" || !imgs[0].Inline || imgs[1].Name != "photo.jpg" || imgs[1].Inline {
			t.Fatalf("images = %+v", imgs)
		}
		if data, _ := os.ReadFile(imgs[1].Path); string(data) != "jpg" {
			t.Errorf("content = %q", data)
		}
		if st, _ := os.Stat(imgs[0].Path); st.Mode().Perm() != 0o600 {
			t.Errorf("mode %v", st.Mode())
		}
	}
	if fetches != 2 {
		t.Errorf("fetched %d times; the second call should use the cache", fetches)
	}
	PruneImageCache(dir, time.Now().Add(8*24*time.Hour))
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("not pruned: %d files", len(entries))
	}
}
