package mailsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ImageFile is one of a message's images, cached on disk for display.
type ImageFile struct {
	ContentID string // for inline images referenced as cid: in the HTML
	Name      string
	Inline    bool
	Path      string
}

// Limits on the images fetched for display.
const (
	maxImages     = 12
	maxImageBytes = 10 << 20
	imageCacheTTL = 7 * 24 * time.Hour
)

// Images fetches a message's image attachments (inline and attached) into
// dir, reusing earlier downloads, and returns them. Images over 10 MB are
// skipped. Remote images (http: in the HTML) are never fetched.
func (e *Engine) Images(ctx context.Context, messageID, dir string) ([]ImageFile, error) {
	atts, err := e.graph.Attachments(ctx, messageID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var out []ImageFile
	for _, a := range atts {
		if len(out) == maxImages {
			break
		}
		if !strings.HasPrefix(strings.ToLower(a.ContentType), "image/") || a.Size > maxImageBytes {
			continue
		}
		sum := sha256.Sum256([]byte(messageID + "\x00" + a.ID))
		path := filepath.Join(dir, hex.EncodeToString(sum[:16]))
		meta := path + ".cid"
		if _, err := os.Stat(path); err != nil {
			full, err := e.graph.Attachment(ctx, messageID, a.ID)
			if err != nil {
				return out, err
			}
			data, err := full.Content()
			if err != nil || len(data) == 0 {
				continue
			}
			if err := writePrivate(meta, []byte(full.ContentID)); err != nil {
				return out, err
			}
			if err := writePrivate(path, data); err != nil {
				return out, err
			}
		}
		cid, _ := os.ReadFile(meta)
		out = append(out, ImageFile{ContentID: string(cid), Name: a.Name, Inline: a.IsInline, Path: path})
	}
	return out, nil
}

// writePrivate writes a file readable only by the user, atomically.
func writePrivate(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// PruneImageCache removes cached images not used for imageCacheTTL.
func PruneImageCache(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, en := range entries {
		info, err := en.Info()
		if err == nil && now.Sub(info.ModTime()) > imageCacheTTL {
			os.Remove(filepath.Join(dir, en.Name()))
		}
	}
}
