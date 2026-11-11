package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/cache"
	msalerrors "github.com/AzureAD/microsoft-authentication-library-for-go/apps/errors"
)

type bytesMarshaler []byte

func (b bytesMarshaler) Marshal() ([]byte, error) { return b, nil }

type captureUnmarshaler struct{ got []byte }

func (c *captureUnmarshaler) Unmarshal(b []byte) error { c.got = b; return nil }

func TestFileCacheRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "token_cache.json")
	fc := &fileCache{path: path}
	ctx := context.Background()

	var empty captureUnmarshaler
	if err := fc.Replace(ctx, &empty, cache.ReplaceHints{}); err != nil {
		t.Fatalf("Replace on missing file: %v", err)
	}
	if empty.got != nil {
		t.Fatalf("Replace on missing file loaded %q", empty.got)
	}

	if err := fc.Export(ctx, bytesMarshaler(`{"a":1}`), cache.ExportHints{}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("cache file perm = %o, want 600", perm)
	}

	var got captureUnmarshaler
	if err := fc.Replace(ctx, &got, cache.ReplaceHints{}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if string(got.got) != `{"a":1}` {
		t.Errorf("round trip = %q", got.got)
	}

	if err := fc.clear(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cache file still exists after clear: %v", err)
	}
}

func TestExplain(t *testing.T) {
	err := Explain(errors.New("invalid_grant: AADSTS90094: The grant requires admin permission."))
	if !strings.Contains(err.Error(), "admin to approve") {
		t.Errorf("missing hint: %v", err)
	}
	plain := errors.New("something else")
	if got := Explain(plain); got != plain {
		t.Errorf("unknown error was rewrapped: %v", got)
	}
}

func TestSilentErr(t *testing.T) {
	ctx := context.Background()
	timeout := fmt.Errorf("server response error:\n %w", &url.Error{Op: "Post", URL: "https://login.microsoftonline.com", Err: context.DeadlineExceeded})
	refused := msalerrors.CallErr{Resp: &http.Response{StatusCode: 400}, Err: errors.New("AADSTS70043: the refresh token has expired")}
	busy := msalerrors.CallErr{Resp: &http.Response{StatusCode: 503}, Err: errors.New("service unavailable")}
	for _, tc := range []struct {
		name  string
		err   error
		login bool
	}{
		{"network timeout", timeout, false},
		{"Entra unavailable", busy, false},
		{"refresh token refused", refused, true},
		{"nothing cached", errors.New("no token found"), true},
	} {
		err := silentErr(ctx, tc.err)
		if got := errors.Is(err, ErrNeedsLogin); got != tc.login {
			t.Errorf("%s: needs login = %v, want %v (%v)", tc.name, got, tc.login, err)
		}
		var ne net.Error
		if !tc.login && !errors.As(err, &ne) && !errors.As(err, &msalerrors.CallErr{}) {
			t.Errorf("%s: lost the cause: %v", tc.name, err)
		}
	}
}
