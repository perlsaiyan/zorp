package mailsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/perlsaiyan/zorp/internal/compose"
)

// sidecar remembers how a draft saved from zorp was written: its Markdown
// source and the formatted history under it. It is only trusted while the
// draft's changeKey is unchanged, i.e. nobody edited it elsewhere.
type sidecar struct {
	ChangeKey string `json:"change_key"`
	Format    string `json:"format"`
	Source    string `json:"source"`
	History   string `json:"history,omitempty"`
}

func (e *Engine) sidecarPath(id string) string {
	if e.opts.DraftDir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(e.opts.DraftDir, hex.EncodeToString(sum[:12])+".json")
}

func (e *Engine) loadSidecar(id string) *sidecar {
	path := e.sidecarPath(id)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var sc sidecar
	if json.Unmarshal(data, &sc) != nil {
		return nil
	}
	return &sc
}

// saveSidecar records a saved draft's source. Failures only cost the
// ability to re-edit it as written, so they are logged, not returned.
// Plain-text drafts round-trip as they are, so they need none.
func (e *Engine) saveSidecar(ctx context.Context, id, format, source, history string) {
	path := e.sidecarPath(id)
	if path == "" {
		return
	}
	if format != compose.FormatMarkdown {
		os.Remove(path)
		return
	}
	d, err := e.graph.GetDraft(ctx, id)
	if err != nil {
		e.log.Warn("reading draft change key", "err", err)
		return
	}
	data, err := json.Marshal(sidecar{ChangeKey: d.ChangeKey, Format: format, Source: source, History: history})
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
			err = os.WriteFile(path, data, 0o600)
		}
	}
	if err != nil {
		e.log.Warn("saving draft source", "err", err)
	}
}

func (e *Engine) removeSidecar(id string) {
	if path := e.sidecarPath(id); path != "" {
		os.Remove(path)
	}
}
