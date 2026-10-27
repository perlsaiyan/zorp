package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/cache"
)

// fileCache persists MSAL's token cache to a 0600 file. Reads take a shared
// flock and writes an exclusive one on a sibling .lock file, so a CLI login
// and the daemon's silent refresh never see a half-written cache.
//
// MSAL calls Replace before and Export after each token operation, so the lock
// is not held across the whole read-modify-write. That is acceptable here:
// Entra refresh tokens are not single-use, so the worst case of a race is one
// process's newer tokens being overwritten by another's still-valid ones.
type fileCache struct {
	path string
}

var _ cache.ExportReplace = (*fileCache)(nil)

func (f *fileCache) Replace(ctx context.Context, c cache.Unmarshaler, _ cache.ReplaceHints) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	unlock, err := f.lock(syscall.LOCK_SH)
	if err != nil {
		return err
	}
	defer unlock()

	data, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading token cache: %w", err)
	}
	return c.Unmarshal(data)
}

func (f *fileCache) Export(ctx context.Context, c cache.Marshaler, _ cache.ExportHints) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := c.Marshal()
	if err != nil {
		return err
	}
	unlock, err := f.lock(syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer unlock()
	return writeFileAtomic(f.path, data)
}

// clear deletes the cache file.
func (f *fileCache) clear() error {
	unlock, err := f.lock(syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.Remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (f *fileCache) lock(how int) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return nil, err
	}
	lf, err := os.OpenFile(f.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening token cache lock: %w", err)
	}
	if err := syscall.Flock(int(lf.Fd()), how); err != nil {
		lf.Close()
		return nil, fmt.Errorf("locking token cache: %w", err)
	}
	return func() {
		syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		lf.Close()
	}, nil
}

// writeFileAtomic writes data to a 0600 temp file and renames it into place.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".token_cache-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
