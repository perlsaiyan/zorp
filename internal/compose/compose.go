// Package compose builds and parses the text file a message is written in.
//
// The file starts with header lines (To, Cc, Bcc, Subject, Attach) and a
// blank line, then the body, like a simple email:
//
//	To: Alice Example <alice@example.com>, bob@example.com
//	Cc:
//	Bcc:
//	Subject: Re: Quarterly numbers
//	Attach: ~/Documents/report.pdf
//
//	Thanks, looks good.
//
//	On Mon, 26 Oct 2026 at 09:00, Alice Example <alice@example.com> wrote:
//	> See attached.
//
// Header lines starting with "#" are comments. "Attached:" lines list
// attachments already on the message (a forward's or a draft's); deleting
// one removes that attachment. "Format: markdown" (or "text") picks how the
// body is sent; see package markdown.
package compose

import (
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"

	"github.com/perlsaiyan/zorp/internal/markdown"
	"github.com/perlsaiyan/zorp/internal/render"
	"github.com/perlsaiyan/zorp/internal/store"
)

// Kind is what is being written.
type Kind string

const (
	New       Kind = "new"
	Reply     Kind = "reply"
	ReplyAll  Kind = "reply-all"
	Forward   Kind = "forward"
	EditDraft Kind = "draft" // an existing draft
)

// Message is the content of a compose file.
type Message struct {
	To, Cc, Bcc []store.Address
	Subject     string
	Format      string // FormatMarkdown or FormatText
	Body        string
	Attach      []string // files to attach, as written (may start with ~)
	Attached    []string // names of existing attachments to keep
}

// Body formats.
const (
	FormatMarkdown = "markdown" // sent as HTML
	FormatText     = "text"
)

// Existing is an attachment already on the message being written.
type Existing struct {
	ID   string
	Name string
	Size int
}

// File renders m as the text to edit.
func File(kind Kind, m Message, existing []Existing) string {
	var b strings.Builder
	b.WriteString("# zorp: " + describe(kind) + ". Write below the blank line; save and quit when done.\n")
	b.WriteString("# Nothing is sent until you confirm. Lines starting with # up here are ignored.\n")
	b.WriteString("# Attach files with one \"Attach: path\" line each.")
	if len(existing) > 0 {
		b.WriteString(" Delete an \"Attached:\" line to drop it.")
	}
	b.WriteString("\n")
	b.WriteString("To: " + FormatAddresses(m.To) + "\n")
	b.WriteString("Cc: " + FormatAddresses(m.Cc) + "\n")
	b.WriteString("Bcc: " + FormatAddresses(m.Bcc) + "\n")
	b.WriteString("Subject: " + oneLine(m.Subject) + "\n")
	if m.Format != "" {
		b.WriteString("Format: " + m.Format + "\n")
	}
	for _, a := range existing {
		b.WriteString("Attached: " + oneLine(a.Name) + "\n")
	}
	if len(m.Attach) == 0 {
		b.WriteString("Attach: \n")
	}
	for _, a := range m.Attach {
		b.WriteString("Attach: " + a + "\n")
	}
	b.WriteString("\n")
	b.WriteString(m.Body)
	if !strings.HasSuffix(m.Body, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

func describe(k Kind) string {
	switch k {
	case Reply:
		return "reply"
	case ReplyAll:
		return "reply to all"
	case Forward:
		return "forward"
	case EditDraft:
		return "draft"
	}
	return "new message"
}

// oneLine makes header text (often from a received message, so untrusted)
// safe for the editor: one line, no control or format characters.
func oneLine(s string) string {
	return strings.Join(strings.Fields(render.Sanitize(s, false)), " ")
}

// Parse reads an edited compose file.
func Parse(text string) (Message, error) {
	var m Message
	text = strings.ReplaceAll(text, "\r\n", "\n")
	head, body, found := strings.Cut(text, "\n\n")
	if !found {
		// No blank line: all header, or the user removed it. Treat a file
		// with no header lines at all as body.
		head, body = text, ""
	}
	type field struct{ key, value string }
	var fields []field
	var errs []string
	for i, line := range strings.Split(head, "\n") {
		switch {
		case strings.TrimSpace(line) == "", strings.HasPrefix(line, "#"):
			continue
		case (line[0] == ' ' || line[0] == '\t') && len(fields) > 0:
			fields[len(fields)-1].value += " " + strings.TrimSpace(line)
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			errs = append(errs, fmt.Sprintf("line %d isn't a header (add a blank line before the message): %q", i+1, line))
			continue
		}
		fields = append(fields, field{strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(value)})
	}
	for _, f := range fields {
		var err error
		switch f.key {
		case "to":
			m.To, err = appendAddresses(m.To, f.value)
		case "cc":
			m.Cc, err = appendAddresses(m.Cc, f.value)
		case "bcc":
			m.Bcc, err = appendAddresses(m.Bcc, f.value)
		case "subject":
			m.Subject = f.value
		case "attach":
			if f.value != "" {
				m.Attach = append(m.Attach, f.value)
			}
		case "format":
			switch v := strings.ToLower(f.value); v {
			case FormatMarkdown, FormatText:
				m.Format = v
			case "md":
				m.Format = FormatMarkdown
			default:
				errs = append(errs, fmt.Sprintf("Format: %q isn't markdown or text", f.value))
			}
		case "attached":
			if f.value != "" {
				m.Attached = append(m.Attached, f.value)
			}
		case "from":
			// Always the signed-in user; ignored.
		default:
			errs = append(errs, fmt.Sprintf("unknown header %q", f.key))
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", strings.ToUpper(f.key[:1])+f.key[1:], err))
		}
	}
	m.Body = strings.TrimRight(body, " \t\n") + "\n"
	if strings.TrimSpace(body) == "" {
		m.Body = ""
	}
	if len(errs) > 0 {
		return m, errors.New(strings.Join(errs, "; "))
	}
	return m, nil
}

// ParseAddresses reads a comma- (or semicolon-) separated address list,
// as written in a header line.
func ParseAddresses(value string) ([]store.Address, error) { return appendAddresses(nil, value) }

// OneLine makes text safe for a header line in the editor.
func OneLine(s string) string { return oneLine(s) }

func appendAddresses(to []store.Address, value string) ([]store.Address, error) {
	value = strings.Trim(strings.ReplaceAll(value, ";", ","), " ,")
	if value == "" {
		return to, nil
	}
	list, err := mail.ParseAddressList(value)
	if err != nil {
		return to, fmt.Errorf("%v in %q", err, value)
	}
	for _, a := range list {
		to = append(to, store.Address{Name: a.Name, Addr: a.Address})
	}
	return to, nil
}

// FormatAddress writes an address so that Parse reads it back.
func FormatAddress(a store.Address) string {
	name, addr := oneLine(a.Name), oneLine(a.Addr)
	if name == "" || strings.EqualFold(name, addr) {
		return addr
	}
	if strings.ContainsAny(name, `()<>[]:;@\,."`) {
		name = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name) + `"`
	}
	return name + " <" + addr + ">"
}

func FormatAddresses(as []store.Address) string {
	parts := make([]string, 0, len(as))
	for _, a := range as {
		parts = append(parts, FormatAddress(a))
	}
	return strings.Join(parts, ", ")
}

// Original is the message being replied to or forwarded.
type Original struct {
	store.Message
	Text string // body as plain text
}

// NewMessage is the template for a fresh message.
func NewMessage(signature, format string) Message {
	return Message{Format: format, Body: "\n" + sigBlock(signature)}
}

// ReplyTo is the template for a reply. self holds the user's addresses, so
// they are left out of the recipients.
//
// In Markdown, the quote is preceded by markdown.RefMarker: it's there to
// read while writing, and the server's formatted copy is sent instead.
func ReplyTo(orig Original, all bool, self []string, signature, format string) Message {
	isSelf := func(a store.Address) bool {
		for _, s := range self {
			if strings.EqualFold(a.Addr, s) {
				return true
			}
		}
		return false
	}
	var to, cc []store.Address
	switch {
	case isSelf(orig.From):
		// Replying to something we sent: go to its recipients again.
		to = orig.To
		if all {
			cc = orig.Cc
		}
	case len(orig.ReplyTo) > 0:
		to = orig.ReplyTo
	default:
		to = []store.Address{orig.From}
	}
	if all && !isSelf(orig.From) {
		cc = append(append(cc, orig.To...), orig.Cc...)
	}
	seen := map[string]bool{}
	keep := func(list []store.Address, dropSelf bool) []store.Address {
		var out []store.Address
		for _, a := range list {
			k := strings.ToLower(a.Addr)
			if k == "" || seen[k] || (dropSelf && isSelf(a)) {
				continue
			}
			seen[k] = true
			out = append(out, a)
		}
		return out
	}
	m := Message{
		To:      keep(to, false),
		Subject: prefixed("Re: ", oneLine(orig.Subject)),
	}
	m.Cc = keep(cc, true)
	m.Format = format
	m.Body = "\n" + sigBlock(signature) + "\n" + refMarker(format) + attribution(orig) + "\n" + quote(orig.Text)
	return m
}

// ForwardOf is the template for forwarding a message.
func ForwardOf(orig Original, signature, format string) Message {
	var b strings.Builder
	b.WriteString("\n" + sigBlock(signature) + "\n" + refMarker(format))
	b.WriteString("---------- Forwarded message ---------\n")
	b.WriteString("From: " + FormatAddress(orig.From) + "\n")
	b.WriteString("Date: " + orig.ReceivedAt.Local().Format("Mon, 2 Jan 2006 at 15:04") + "\n")
	b.WriteString("Subject: " + oneLine(orig.Subject) + "\n")
	b.WriteString("To: " + FormatAddresses(orig.To) + "\n")
	if len(orig.Cc) > 0 {
		b.WriteString("Cc: " + FormatAddresses(orig.Cc) + "\n")
	}
	b.WriteString("\n" + strings.TrimRight(orig.Text, "\n") + "\n")
	return Message{Subject: prefixed("Fw: ", oneLine(orig.Subject)), Format: format, Body: b.String()}
}

func refMarker(format string) string {
	if format == FormatMarkdown {
		return markdown.RefMarker + "\n"
	}
	return ""
}

func sigBlock(signature string) string {
	signature = strings.TrimRight(signature, "\n")
	if signature == "" {
		return ""
	}
	return "\n-- \n" + signature + "\n"
}

func attribution(orig Original) string {
	return "On " + orig.ReceivedAt.Local().Format("Mon, 2 Jan 2006 at 15:04") + ", " + FormatAddress(orig.From) + " wrote:"
}

func quote(text string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if l == "" || strings.HasPrefix(l, ">") {
			b.WriteString(">" + l + "\n")
		} else {
			b.WriteString("> " + l + "\n")
		}
	}
	return b.String()
}

// prefixed adds "Re: " or "Fw: " unless the subject already has one,
// looking past tags such as "[EXTERNAL]".
func prefixed(prefix, subject string) string {
	s := strings.TrimSpace(subject)
	rest := s
	for strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]")
		if end < 0 {
			break
		}
		rest = strings.TrimSpace(rest[end+1:])
	}
	lower := strings.ToLower(rest)
	for _, p := range prefixesFor(prefix) {
		if strings.HasPrefix(lower, p) {
			return s
		}
	}
	return prefix + s
}

// BareSubject is a subject without its gateway tags and "Re:" or "Fw:"
// prefixes, e.g. for a meeting about the message.
func BareSubject(subject string) string {
	s := strings.TrimSpace(subject)
	for {
		rest := s
		if strings.HasPrefix(rest, "[") {
			if end := strings.Index(rest, "]"); end >= 0 {
				rest = rest[end+1:]
			}
		}
		lower := strings.ToLower(rest)
		for _, p := range []string{"re:", "fw:", "fwd:"} {
			if strings.HasPrefix(lower, p) {
				rest = rest[len(p):]
				break
			}
		}
		rest = strings.TrimSpace(rest)
		if rest == s {
			return s
		}
		s = rest
	}
}

func prefixesFor(prefix string) []string {
	if prefix == "Re: " {
		return []string{"re:"}
	}
	return []string{"fw:", "fwd:"}
}

// AddAttach adds an "Attach: path" header to a compose file, filling in an
// empty Attach: line if there is one.
func AddAttach(text, path string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	head, body, found := strings.Cut(text, "\n\n")
	line := "Attach: " + oneLine(path)
	lines := strings.Split(head, "\n")
	placed := false
	for i, l := range lines {
		if key, value, ok := strings.Cut(l, ":"); ok && strings.EqualFold(strings.TrimSpace(key), "attach") && strings.TrimSpace(value) == "" {
			lines[i], placed = line, true
			break
		}
	}
	if !placed {
		lines = append(lines, line)
	}
	head = strings.Join(lines, "\n")
	if !found {
		return head + "\n\n"
	}
	return head + "\n\n" + body
}

// OwnText is the part of a message body the user wrote: above the
// reference marker, the signature, and any quoted or forwarded original.
func OwnText(body string) string {
	mine, _ := markdown.SplitRef(body)
	lines := strings.Split(mine, "\n")
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if l == "-- " || strings.HasPrefix(t, ">") || strings.HasPrefix(t, "---------- Forwarded message") ||
			(strings.HasPrefix(t, "On ") && strings.HasSuffix(t, " wrote:")) {
			return strings.Join(lines[:i], "\n")
		}
	}
	return mine
}

var attachWords = regexp.MustCompile(`(?i)\b(attach(ed|ing|ment|ments)?|enclosed)\b`)

// MentionsAttachment reports whether the user's own text talks about an
// attachment ("see attached"), to catch a forgotten one.
func MentionsAttachment(body string) bool {
	return attachWords.MatchString(OwnText(body))
}
