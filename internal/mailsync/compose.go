package mailsync

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/perlsaiyan/zorp/internal/compose"
	"github.com/perlsaiyan/zorp/internal/graph"
	"github.com/perlsaiyan/zorp/internal/markdown"
	"github.com/perlsaiyan/zorp/internal/render"
)

// Outgoing is a message to send or save as a draft.
type Outgoing struct {
	Kind   compose.Kind
	Format string // compose.FormatMarkdown or FormatText
	Ref    string // message replied to or forwarded, or the draft being edited
	Draft  graph.Draft
	// Attach are absolute paths of files to attach.
	Attach []string
	// Keep names the existing attachments (of a forward or draft) to keep;
	// others are removed. Inline attachments are removed unless the body
	// keeps the original's formatted HTML.
	Keep []string
	Send bool
}

// DraftKept is returned with an error when a draft was created (or
// updated) but sending it failed: the message is safe in Drafts.
type DraftKept struct {
	ID  string
	Err error
}

func (e *DraftKept) Error() string {
	return e.Err.Error() + " (the message is saved in Drafts)"
}

func (e *DraftKept) Unwrap() error { return e.Err }

type fileAttachment struct {
	name, contentType string
	data              []byte
}

// Compose sends a message or saves it as a draft and returns the draft's
// ID (empty when sent directly). Replies and forwards are created from the
// original, so they thread correctly; their recipients, subject and body
// are then replaced with the user's.
//
// In Markdown format the body is sent as HTML. If it still has the
// reference marker (see markdown.SplitRef), only the text above it is the
// message, and the reply's or forward's formatted history (from the server,
// or remembered for a draft) goes below it.
func (e *Engine) Compose(ctx context.Context, o Outgoing) (string, error) {
	// Read attachments before creating anything, so a bad path changes nothing.
	files := make([]fileAttachment, 0, len(o.Attach))
	for _, path := range o.Attach {
		f, err := readAttachment(path)
		if err != nil {
			return "", err
		}
		files = append(files, f)
	}

	source := o.Draft.Body
	mine, hasRef := source, false
	if o.Format == compose.FormatMarkdown {
		mine, hasRef = markdown.SplitRef(source)
		o.Draft.Body, o.Draft.HTML = markdown.ToHTML(mine), true
	}
	mineHTML := o.Draft.Body

	if o.Kind == compose.New && o.Send && len(files) == 0 {
		return "", e.graph.SendMail(ctx, o.Draft)
	}

	var id, history string
	var err error
	switch o.Kind {
	case compose.New:
		id, err = e.graph.CreateDraft(ctx, o.Draft)
	case compose.Reply, compose.ReplyAll, compose.Forward:
		kind := map[compose.Kind]string{compose.Reply: graph.ReplyOne, compose.ReplyAll: graph.ReplyAll, compose.Forward: graph.Forward}[o.Kind]
		id, err = e.graph.CreateResponse(ctx, o.Ref, kind)
		if err == nil && hasRef {
			var d *graph.DraftMessage
			if d, err = e.graph.GetDraft(ctx, id); err == nil {
				history = d.Body.Content
				o.Draft.Body = markdown.Above(mineHTML, history)
			}
		}
		if err == nil {
			err = e.graph.UpdateDraft(ctx, id, o.Draft)
		}
	case compose.EditDraft:
		id = o.Ref
		if sc := e.loadSidecar(id); hasRef && sc != nil && sc.History != "" {
			history = sc.History
			o.Draft.Body = markdown.Above(mineHTML, history)
		}
		err = e.graph.UpdateDraft(ctx, id, o.Draft)
	default:
		return "", fmt.Errorf("unknown message kind %q", o.Kind)
	}
	if err != nil {
		if id != "" {
			return id, &DraftKept{id, err}
		}
		return "", err
	}

	if o.Kind == compose.Forward || o.Kind == compose.EditDraft {
		if err := e.pruneAttachments(ctx, id, o.Keep, o.Format == compose.FormatMarkdown && history != ""); err != nil {
			return id, &DraftKept{id, err}
		}
	}
	for _, f := range files {
		if err := e.graph.AddAttachment(ctx, id, f.name, f.contentType, f.data); err != nil {
			return id, &DraftKept{id, fmt.Errorf("attaching %s: %w", f.name, err)}
		}
	}
	if !o.Send {
		e.saveSidecar(ctx, id, o.Format, source, history)
		return id, nil
	}
	if err := e.graph.SendDraft(ctx, id); err != nil {
		e.saveSidecar(ctx, id, o.Format, source, history)
		return id, &DraftKept{id, err}
	}
	e.removeSidecar(id)
	return id, nil
}

// pruneAttachments removes attachments not named in keep (matched by
// name, as many times as each name is listed). Inline attachments are
// removed too, unless the body keeps the original's HTML (keepInline),
// whose images refer to them.
func (e *Engine) pruneAttachments(ctx context.Context, id string, keep []string, keepInline bool) error {
	atts, err := e.graph.Attachments(ctx, id)
	if err != nil {
		return err
	}
	want := map[string]int{}
	for _, k := range keep {
		want[k]++
	}
	for _, a := range atts {
		name := strings.Join(strings.Fields(a.Name), " ") // as listed in the compose file
		if a.IsInline && keepInline {
			continue
		}
		if !a.IsInline && want[name] > 0 {
			want[name]--
			continue
		}
		if err := e.graph.DeleteAttachment(ctx, id, a.ID); err != nil {
			return fmt.Errorf("removing attachment %s: %w", a.Name, err)
		}
	}
	return nil
}

func readAttachment(path string) (fileAttachment, error) {
	if !filepath.IsAbs(path) {
		return fileAttachment{}, fmt.Errorf("attachment path %q is not absolute", path)
	}
	st, err := os.Stat(path)
	if err != nil {
		return fileAttachment{}, err
	}
	if !st.Mode().IsRegular() {
		return fileAttachment{}, fmt.Errorf("%s is not a regular file", path)
	}
	if st.Size() > graph.MaxAttachment {
		return fileAttachment{}, fmt.Errorf("%s is larger than 150 MB", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fileAttachment{}, err
	}
	ct := mime.TypeByExtension(filepath.Ext(path))
	if ct == "" {
		ct = "application/octet-stream"
	}
	return fileAttachment{name: filepath.Base(path), contentType: ct, data: data}, nil
}

// DraftDetails is a draft's content for editing as text.
type DraftDetails struct {
	Draft       graph.Draft
	Format      string             // set when the draft's source was remembered
	WasHTML     bool               // converted from HTML: formatting was lost
	Attachments []graph.Attachment // not inline
}

// GetDraft fetches a draft for editing. HTML bodies are converted to text.
func (e *Engine) GetDraft(ctx context.Context, id string) (*DraftDetails, error) {
	d, err := e.graph.GetDraft(ctx, id)
	if err != nil {
		return nil, err
	}
	if !d.IsDraft {
		return nil, errors.New("that message isn't a draft")
	}
	out := &DraftDetails{Draft: graph.Draft{
		Subject: d.Subject,
		To:      emailAddrs(d.ToRecipients),
		Cc:      emailAddrs(d.CcRecipients),
		Bcc:     emailAddrs(d.BccRecipients),
		Body:    normalizeText(d.Body.Content),
	}}
	switch sc := e.loadSidecar(id); {
	case sc != nil && sc.ChangeKey != "" && sc.ChangeKey == d.ChangeKey:
		// Saved by zorp and not edited since: edit what was written.
		out.Draft.Body, out.Format = sc.Source, sc.Format
	case strings.EqualFold(d.Body.ContentType, "html"):
		out.WasHTML = true
		out.Draft.Body = render.FromHTML(d.Body.Content).Text(76)
	}
	atts, err := e.graph.Attachments(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, a := range atts {
		if !a.IsInline {
			out.Attachments = append(out.Attachments, a)
		}
	}
	return out, nil
}

func emailAddrs(rs []graph.Recipient) []graph.EmailAddress {
	out := make([]graph.EmailAddress, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.EmailAddress)
	}
	return out
}

// SaveAttachment downloads an attachment into dir under its own name
// (made safe, and never overwriting a file) and returns the path.
func (e *Engine) SaveAttachment(ctx context.Context, messageID, attachmentID, dir string) (string, error) {
	a, err := e.graph.Attachment(ctx, messageID, attachmentID)
	if err != nil {
		return "", err
	}
	if a.ContentB64 == "" && a.Size > 0 {
		return "", errors.New("only file attachments can be saved (not attached emails or links)")
	}
	data, err := a.Content()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := SafeFileName(a.Name)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 1000; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		path := filepath.Join(dir, candidate)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			os.Remove(path)
			return "", err
		}
		return path, f.Close()
	}
	return "", fmt.Errorf("too many files named %s in %s", name, dir)
}

// SafeFileName makes an attachment's name safe to create in a directory:
// no path separators, control characters or leading dots.
func SafeFileName(name string) string {
	name = render.Sanitize(name, false)
	// Keep only the last path component of names like "../../x" or "a\\b".
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimLeft(strings.TrimSpace(name), ".")
	if name == "" {
		name = "attachment"
	}
	if len(name) > 200 {
		ext := filepath.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		name = strings.ToValidUTF8(name[:200-len(ext)], "") + ext
	}
	return name
}

// Attachments lists a message's attachments (without contents).
func (e *Engine) Attachments(ctx context.Context, messageID string) ([]graph.Attachment, error) {
	return e.graph.Attachments(ctx, messageID)
}
