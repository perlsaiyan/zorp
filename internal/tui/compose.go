package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/compose"
	"github.com/perlsaiyan/zorp/internal/daemon"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/store"
)

// quoteWidth is the width replies and forwards wrap the original at.
const quoteWidth = 72

// composing is a message being written. While it is set (and the editor
// isn't running) the review screen is shown.
type composing struct {
	kind     compose.Kind
	ref      string // graph ID of the original, or of the draft being edited
	path     string // the file being edited
	template string // the file as first written, to spot "nothing written"
	existing []ipc.AttachmentInfo
	wasHTML  bool

	msg      compose.Message
	attach   []string // msg.Attach, resolved to absolute paths
	problems []string // reasons it can't be sent as is
	busy     string   // "Sending…" while the daemon works
}

// Messages for the compose flow.
type (
	composeReadyMsg struct {
		c    *composing
		text string
		err  error
	}
	editorDoneMsg  struct{ err error }
	composeDoneMsg struct {
		res  ipc.ComposeResult
		send bool
		err  error
	}
	composeDiscardMsg struct{}
)

// startCompose prepares a template for kind (the original is the current
// message, except for New).
func (m *Model) startCompose(kind compose.Kind) tea.Cmd {
	if m.composing != nil {
		return nil
	}
	var orig store.Message
	if kind != compose.New {
		var ok bool
		if orig, ok = m.currentMessage(); !ok {
			return nil
		}
	}
	if kind == compose.EditDraft && !m.isDraft(orig) {
		m.errText = "e edits drafts; this message isn't in Drafts"
		return nil
	}
	cfg, st, self := m.cfg, m.st, m.account.UPN
	m.notice = "Preparing…"
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c := &composing{kind: kind, ref: orig.GraphID}
		sig := cfg.Compose.Signature
		var msg compose.Message
		switch kind {
		case compose.New:
			msg = compose.NewMessage(sig)
		case compose.Reply, compose.ReplyAll, compose.Forward:
			text, err := originalText(ctx, cfg, st, orig)
			if err != nil {
				return composeReadyMsg{err: err}
			}
			o := compose.Original{Message: orig, Text: text}
			if kind == compose.Forward {
				msg = compose.ForwardOf(o, sig)
				if orig.HasAttachments {
					var atts []ipc.AttachmentInfo
					if err := callDaemon(ctx, cfg, ipc.MethodAttachList, ipc.MessageRef{GraphID: orig.GraphID}, &atts); err != nil {
						return composeReadyMsg{err: fmt.Errorf("listing attachments: %w", err)}
					}
					for _, a := range atts {
						if !a.IsInline {
							c.existing = append(c.existing, a)
						}
					}
				}
			} else {
				msg = compose.ReplyTo(o, kind == compose.ReplyAll, []string{self}, sig)
			}
		case compose.EditDraft:
			var d ipc.DraftInfo
			if err := callDaemon(ctx, cfg, ipc.MethodGetDraft, ipc.MessageRef{GraphID: orig.GraphID}, &d); err != nil {
				return composeReadyMsg{err: err}
			}
			msg = compose.Message{To: fromIPC(d.To), Cc: fromIPC(d.Cc), Bcc: fromIPC(d.Bcc), Subject: d.Subject, Body: d.Body}
			c.existing, c.wasHTML = d.Attachments, d.WasHTML
		}
		existing := make([]compose.Existing, 0, len(c.existing))
		for _, a := range c.existing {
			existing = append(existing, compose.Existing{ID: a.ID, Name: a.Name, Size: a.Size})
		}
		return composeReadyMsg{c: c, text: compose.File(kind, msg, existing)}
	}
}

// originalText is the original message's body as plain text, fetched
// through the daemon if it isn't cached yet.
func originalText(ctx context.Context, cfg *config.Config, st *store.Store, orig store.Message) (string, error) {
	body, fetched, err := st.MessageBody(ctx, orig.ID)
	if err != nil {
		return "", err
	}
	if !fetched {
		if err := callDaemon(ctx, cfg, ipc.MethodFetchBody, ipc.FetchBodyParams{GraphID: orig.GraphID}, nil); err != nil {
			return "", fmt.Errorf("fetching the original: %w", err)
		}
		if body, _, err = st.MessageBody(ctx, orig.ID); err != nil {
			return "", err
		}
	}
	return parseBody(body).Text(quoteWidth), nil
}

func callDaemon(ctx context.Context, cfg *config.Config, method string, params, result any) error {
	c, err := daemon.Connect(ctx, cfg, true)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Call(ctx, method, params, result)
}

// composeReady writes the template and opens the editor on it.
func (m *Model) composeReady(r composeReadyMsg) tea.Cmd {
	m.notice = ""
	if r.err != nil {
		m.errText = r.err.Error()
		return nil
	}
	dir := m.cfg.Paths.ComposeDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		m.errText = err.Error()
		return nil
	}
	f, err := os.CreateTemp(dir, string(r.c.kind)+"-"+time.Now().Format("20060102-150405")+"-*.txt")
	if err == nil {
		_, err = f.WriteString(r.text)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		m.errText = "writing the message file: " + err.Error()
		return nil
	}
	r.c.path, r.c.template = f.Name(), r.text
	m.composing = r.c
	return m.editCmd()
}

// editorCommand is the user's editor with the file to edit appended.
func editorCommand(cfg *config.Config, path string) *exec.Cmd {
	editor := cfg.Compose.Editor
	for _, env := range []string{"VISUAL", "EDITOR"} {
		if editor == "" {
			editor = os.Getenv(env)
		}
	}
	args := strings.Fields(editor)
	if len(args) == 0 {
		args = []string{"vi"}
	}
	return exec.Command(args[0], append(args[1:], path)...)
}

func (m *Model) editCmd() tea.Cmd {
	return tea.ExecProcess(editorCommand(m.cfg, m.composing.path), func(err error) tea.Msg { return editorDoneMsg{err} })
}

// editorDone reads the edited file back and shows the review screen.
func (m *Model) editorDone(err error) tea.Cmd {
	c := m.composing
	if c == nil {
		return nil
	}
	if err != nil {
		m.errText = "editor: " + err.Error() + " (set compose.editor or $EDITOR)"
	}
	data, rerr := os.ReadFile(c.path)
	if rerr != nil {
		m.errText = rerr.Error()
		m.composing = nil
		return nil
	}
	if string(data) == c.template && c.kind != compose.EditDraft && err == nil {
		m.endCompose()
		m.notice = "Nothing written; discarded."
		return nil
	}
	c.msg, c.attach, c.problems = compose.Message{}, nil, nil
	msg, perr := compose.Parse(string(data))
	c.msg = msg
	if perr != nil {
		c.problems = append(c.problems, perr.Error())
	}
	for _, a := range msg.Attach {
		path := config.ExpandHome(a)
		if !filepath.IsAbs(path) {
			c.problems = append(c.problems, "Attach: use a full path (or ~/...), not "+a)
			continue
		}
		st, err := os.Stat(path)
		switch {
		case err != nil:
			c.problems = append(c.problems, "Attach: "+err.Error())
		case !st.Mode().IsRegular():
			c.problems = append(c.problems, "Attach: "+a+" is not a file")
		default:
			c.attach = append(c.attach, path)
		}
	}
	for _, name := range msg.Attached {
		if !c.hasExisting(name) {
			c.problems = append(c.problems, "Attached: "+name+" isn't one of the existing attachments (use Attach: for new files)")
		}
	}
	return nil
}

func (c *composing) hasExisting(name string) bool {
	for _, a := range c.existing {
		if strings.Join(strings.Fields(a.Name), " ") == name {
			return true
		}
	}
	return false
}

func (c *composing) recipients() int { return len(c.msg.To) + len(c.msg.Cc) + len(c.msg.Bcc) }

// endCompose deletes the message file and closes the review screen.
func (m *Model) endCompose() {
	if m.composing != nil {
		os.Remove(m.composing.path)
	}
	m.composing = nil
}

func (m *Model) composeKey(key string) tea.Cmd {
	c := m.composing
	if c.busy != "" {
		return nil
	}
	switch key {
	case "y":
		switch {
		case len(c.problems) > 0:
			m.errText = "fix the problems first (e to edit)"
			return nil
		case c.recipients() == 0:
			m.errText = "no recipients (e to edit)"
			return nil
		}
		c.busy = "Sending…"
		return m.submitCmd(true)
	case "d":
		if len(c.problems) > 0 {
			m.errText = "fix the problems first (e to edit)"
			return nil
		}
		c.busy = "Saving…"
		return m.submitCmd(false)
	case "e":
		return m.editCmd()
	case "q", "esc":
		m.confirm = &confirm{
			prompt: "Discard this message? y/n",
			cmd:    func() tea.Msg { return composeDiscardMsg{} },
		}
	}
	return nil
}

func (m *Model) submitCmd(send bool) tea.Cmd {
	c, cfg := m.composing, m.cfg
	p := ipc.ComposeParams{
		Kind:    string(c.kind),
		Ref:     c.ref,
		To:      toIPC(c.msg.To),
		Cc:      toIPC(c.msg.Cc),
		Bcc:     toIPC(c.msg.Bcc),
		Subject: c.msg.Subject,
		Body:    c.msg.Body,
		Attach:  c.attach,
		Keep:    c.msg.Attached,
		Send:    send,
	}
	return func() tea.Msg {
		// Large attachments can take a while to upload.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		var res ipc.ComposeResult
		err := callDaemon(ctx, cfg, ipc.MethodCompose, p, &res)
		return composeDoneMsg{res: res, send: send, err: err}
	}
}

func (m *Model) composeDone(r composeDoneMsg) tea.Cmd {
	c := m.composing
	if c == nil {
		return nil
	}
	c.busy = ""
	switch {
	case r.err != nil:
		// Nothing was created or changed; stay on the review screen.
		m.errText = r.err.Error()
	case r.res.SendError != "":
		// The draft exists but didn't go out: it's safe in Drafts.
		m.endCompose()
		m.errText = "Couldn't send: " + r.res.SendError + ". Open it in Drafts and press e to try again."
	case r.send:
		m.endCompose()
		m.notice = "Sent."
	default:
		m.endCompose()
		m.notice = "Saved to Drafts."
	}
	return nil
}

func toIPC(as []store.Address) []ipc.Address {
	out := make([]ipc.Address, 0, len(as))
	for _, a := range as {
		out = append(out, ipc.Address{Name: a.Name, Addr: a.Addr})
	}
	return out
}

func fromIPC(as []ipc.Address) []store.Address {
	out := make([]store.Address, 0, len(as))
	for _, a := range as {
		out = append(out, store.Address{Name: a.Name, Addr: a.Addr})
	}
	return out
}

func (m *Model) isDraft(msg store.Message) bool {
	for _, f := range m.folders {
		if f.ID == msg.FolderID {
			return f.WellKnown == "drafts"
		}
	}
	return false
}

func describeKind(k compose.Kind) string {
	switch k {
	case compose.Reply:
		return "Reply"
	case compose.ReplyAll:
		return "Reply to all"
	case compose.Forward:
		return "Forward"
	case compose.EditDraft:
		return "Draft"
	}
	return "New message"
}

// composeLines is the review screen shown after editing.
func (m *Model) composeLines(w, h int) []string {
	c := m.composing
	field := func(name, value string) string {
		return ansi.Truncate(styleHeader.Render(fmt.Sprintf("  %-9s", name+":"))+" "+value, w, "…")
	}
	status := "ready to send"
	switch {
	case c.busy != "":
		status = c.busy
	case len(c.problems) > 0:
		status = "needs fixing"
	case c.recipients() == 0:
		status = "no recipients yet"
	}
	lines := []string{
		"",
		styleBold.Render("  "+describeKind(c.kind)) + styleFaint.Render("  ·  "+status),
		"",
		field("From", clean(m.account.UPN)),
		field("To", clean(compose.FormatAddresses(c.msg.To))),
	}
	if len(c.msg.Cc) > 0 {
		lines = append(lines, field("Cc", clean(compose.FormatAddresses(c.msg.Cc))))
	}
	if len(c.msg.Bcc) > 0 {
		lines = append(lines, field("Bcc", clean(compose.FormatAddresses(c.msg.Bcc))))
	}
	lines = append(lines, field("Subject", clean(c.msg.Subject)))
	var atts []string
	for _, name := range c.msg.Attached {
		size := ""
		for _, a := range c.existing {
			if strings.Join(strings.Fields(a.Name), " ") == name {
				size = " (" + humanSize(int64(a.Size)) + ")"
			}
		}
		atts = append(atts, clean(name)+size)
	}
	for _, p := range c.attach {
		size := ""
		if st, err := os.Stat(p); err == nil {
			size = " (" + humanSize(st.Size()) + ")"
		}
		atts = append(atts, clean(filepath.Base(p))+size)
	}
	if len(atts) > 0 {
		lines = append(lines, field("Attach", strings.Join(atts, ", ")))
	}
	lines = append(lines, "")
	for _, p := range c.problems {
		lines = append(lines, styleError.Render(ansi.Truncate("  ✗ "+clean(p), w, "…")))
	}
	if m.status.SafeMode {
		lines = append(lines, styleError.Render(ansi.Truncate("  Safe mode: sending only works to yourself ("+clean(m.account.UPN)+"); d saves a draft.", w, "…")))
	}
	if c.wasHTML {
		lines = append(lines, styleFaint.Render(ansi.Truncate("  This draft was formatted (HTML); saving it makes it plain text.", w, "…")))
	}
	lines = append(lines, styleFaint.Render("  "+strings.Repeat("─", max(min(w-4, 72), 0))))
	bodyW := max(w-4, 10)
	for _, l := range strings.Split(ansi.Wrap(cleanBody(c.msg.Body), bodyW, ""), "\n") {
		lines = append(lines, "  "+l)
	}
	return padLines(lines, w, h)
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", (n+1<<9)>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

var errNoAttachments = errors.New("this message has no attachments")
