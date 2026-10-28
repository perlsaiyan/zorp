package mailsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/perlsaiyan/zorp/internal/graph"
)

// OpenedAttachment is an attachment fetched for viewing.
type OpenedAttachment struct {
	Name        string
	ContentType string
	Kind        string // graph.KindFile or graph.KindItem
	Size        int
	// Path is a private copy of a file attachment, under the open cache.
	Path string
	// Message is an attached email.
	Message *graph.AttachedMessage
}

var errReference = errors.New("that's a link to a file in OneDrive or SharePoint, not a copy; open the message in the browser (o) to follow it")

// OpenAttachment fetches an attachment for viewing: a file attachment is
// copied into dir (readable only by the user, named so other programs know
// its type), and an attached email is returned as a message.
func (e *Engine) OpenAttachment(ctx context.Context, messageID, attachmentID, dir string) (*OpenedAttachment, error) {
	a, err := e.findAttachment(ctx, messageID, attachmentID)
	if err != nil {
		return nil, err
	}
	out := &OpenedAttachment{Name: a.Name, ContentType: a.ContentType, Kind: a.Kind(), Size: a.Size}
	switch a.Kind() {
	case graph.KindReference:
		return nil, errReference
	case graph.KindItem:
		msg, ok, err := e.graph.AttachedItem(ctx, messageID, attachmentID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("only attached emails can be opened here (this is a calendar item or contact); s saves it")
		}
		out.Message = msg
		return out, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(messageID + "\x00" + attachmentID))
	out.Path = filepath.Join(dir, hex.EncodeToString(sum[:6])+"-"+SafeFileName(a.Name))
	if _, err := os.Stat(out.Path); err == nil {
		touch(out.Path)
		return out, nil
	}
	data, err := e.graph.AttachmentRaw(ctx, messageID, attachmentID)
	if err != nil {
		return nil, err
	}
	if err := writePrivate(out.Path, data); err != nil {
		return nil, err
	}
	return out, nil
}

// findAttachment looks an attachment up in its message's list.
func (e *Engine) findAttachment(ctx context.Context, messageID, attachmentID string) (*graph.Attachment, error) {
	atts, err := e.Attachments(ctx, messageID)
	if err != nil {
		return nil, err
	}
	for i := range atts {
		if atts[i].ID == attachmentID {
			return &atts[i], nil
		}
	}
	return nil, fmt.Errorf("no such attachment (it may have been removed)")
}

// touch keeps a reused cache file from being pruned.
func touch(path string) {
	now := time.Now()
	os.Chtimes(path, now, now)
}
