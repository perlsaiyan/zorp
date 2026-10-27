package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/cache"
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
