// Package notify turns newly arrived mail into notifications: on the
// desktop through notify-send (freedesktop notifications, e.g. mako), or
// through the terminal zorp runs in (OSC 9 / 99 / 777), which also works
// over SSH.
//
// Senders and subjects come from strangers. They are stripped of control
// characters, shortened, escaped for notification markup, and passed to
// notify-send as arguments after "--", never through a shell.
package notify

import (
	"context"
	"errors"
	"fmt"
	"html"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/render"
	"github.com/perlsaiyan/zorp/internal/store"
)

// Arrival is the new messages of one synced folder.
type Arrival struct {
	Folder    string // display name
	WellKnown string
	Messages  []store.Message
}

// Select picks the messages to notify about: unread, in a configured
// folder, from an allowed sender, not from the user, outside quiet hours.
func Select(cfg config.Notify, arrivals []Arrival, self string, now time.Time) []store.Message {
	if !cfg.Enabled || cfg.Method == "none" || cfg.Quiet(now) {
		return nil
	}
	var out []store.Message
	for _, a := range arrivals {
		if !folderWanted(cfg.Folders, a) {
			continue
		}
		for _, m := range a.Messages {
			if m.IsRead || strings.EqualFold(m.From.Addr, self) || !senderWanted(cfg.From, m.From.Addr) {
				continue
			}
			out = append(out, m)
		}
	}
	return out
}

func folderWanted(folders []string, a Arrival) bool {
	for _, f := range folders {
		if strings.EqualFold(f, a.WellKnown) || strings.EqualFold(f, a.Folder) {
			return true
		}
	}
	return false
}

func senderWanted(from []string, addr string) bool {
	if len(from) == 0 {
		return true
	}
	addr = strings.ToLower(addr)
	_, domain, _ := strings.Cut(addr, "@")
	for _, f := range from {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == addr || (strings.HasPrefix(f, "@") && f[1:] == domain) {
			return true
		}
	}
	return false
}

// Note is one notification.
type Note struct {
	Title   string
	Body    string
	GraphID string // the message, for a single-message note
}

// Build makes the notifications for msgs: one each, or a single summary
// when there are more than GroupAfter, or counts only without ShowContent.
func Build(cfg config.Notify, msgs []store.Message) []Note {
	switch {
	case len(msgs) == 0:
		return nil
	case !cfg.ShowContent:
		return []Note{{Title: "zorp", Body: count(len(msgs))}}
	case len(msgs) > max(cfg.GroupAfter, 1):
		var lines []string
		for _, m := range msgs[:3] {
			subj, _ := Untag(subject(m))
			lines = append(lines, short(sender(m.From), 30)+": "+short(subj, 60))
		}
		if len(msgs) > 3 {
			lines = append(lines, "and "+strconv.Itoa(len(msgs)-3)+" more")
		}
		return []Note{{Title: count(len(msgs)), Body: strings.Join(lines, "\n")}}
	}
	var out []Note
	for _, m := range msgs {
		subj, external := Untag(subject(m))
		title := short(sender(m.From), 60)
		if external {
			title += " (external)"
		}
		out = append(out, Note{Title: title, Body: short(subj, 160), GraphID: m.GraphID})
	}
	return out
}

// Untag drops the tags mail gateways put before external mail's subject
// ("[EXTERNAL]", "[EXT]", "EXTERNAL:"), which would otherwise fill a
// small notification. external reports whether there was one.
func Untag(subject string) (rest string, external bool) {
	for {
		s := strings.TrimSpace(subject)
		lower := strings.ToLower(s)
		cut := 0
		for _, tag := range []string{"[external]", "[ext]", "external:", "[external email]"} {
			if strings.HasPrefix(lower, tag) {
				cut = len(tag)
				break
			}
		}
		if cut == 0 || cut == len(s) {
			return s, external
		}
		subject, external = s[cut:], true
	}
}

func count(n int) string {
	if n == 1 {
		return "1 new message"
	}
	return strconv.Itoa(n) + " new messages"
}

func sender(a store.Address) string {
	if s := clean(a.Name); s != "" {
		return s
	}
	if s := clean(a.Addr); s != "" {
		return s
	}
	return "Unknown sender"
}

func subject(m store.Message) string {
	if s := clean(m.Subject); s != "" {
		return s
	}
	return "(no subject)"
}

func clean(s string) string {
	return strings.Join(strings.Fields(render.Sanitize(s, false)), " ")
}

// short cuts s to n characters, with an ellipsis.
func short(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// DesktopAvailable reports whether there's a desktop notification server
// to talk to: notify-send is installed, and on the session bus something
// owns org.freedesktop.Notifications or can be started for it (mako,
// dunst…). Without one, notify-send would hang waiting for it.
func DesktopAvailable() bool {
	if _, err := exec.LookPath("notify-send"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	const name = "org.freedesktop.Notifications"
	if _, err := exec.LookPath("busctl"); err == nil {
		out, _ := exec.CommandContext(ctx, "busctl", "--user", "call", "org.freedesktop.DBus", "/org/freedesktop/DBus",
			"org.freedesktop.DBus", "NameHasOwner", "s", name).Output()
		if strings.TrimSpace(string(out)) == "b true" {
			return true
		}
		out, _ = exec.CommandContext(ctx, "busctl", "--user", "call", "org.freedesktop.DBus", "/org/freedesktop/DBus",
			"org.freedesktop.DBus", "ListActivatableNames").Output()
		return strings.Contains(string(out), `"`+name+`"`)
	}
	call := func(method string, args ...string) string {
		out, _ := exec.CommandContext(ctx, "gdbus", append([]string{"call", "--session", "--dest", "org.freedesktop.DBus",
			"--object-path", "/org/freedesktop/DBus", "--method", "org.freedesktop.DBus." + method}, args...)...).Output()
		return string(out)
	}
	return strings.HasPrefix(call("NameHasOwner", name), "(true") || strings.Contains(call("ListActivatableNames"), "'"+name+"'")
}

// ErrNoServer means notify-send ran but no notification server answered
// (e.g. an SSH session with no desktop).
var ErrNoServer = errors.New("no desktop notification server")

// Desktop shows n with notify-send. It returns once the notification is
// shown (or failed); clicked is called if the user clicks it, until ctx
// ends. Failures to show it are returned.
func Desktop(ctx context.Context, n Note, clicked func()) error {
	cmd := exec.CommandContext(ctx, "notify-send",
		"--app-name=zorp", "--icon=mail-unread", "--category=email.arrived",
		"--action=default=Open", "--wait",
		"--", n.Title, html.EscapeString(n.Body)) // bodies may be rendered as markup
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	// notify-send fails within moments when there's no server; with one,
	// it waits (--wait) until the notification is clicked or closed.
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%w: %s", ErrNoServer, strings.TrimSpace(stderr.String()))
		}
		if strings.TrimSpace(stdout.String()) == "default" && clicked != nil {
			clicked()
		}
		return nil
	case <-time.After(2 * time.Second):
	}
	go func() {
		if <-done == nil && strings.TrimSpace(stdout.String()) == "default" && clicked != nil {
			clicked()
		}
	}()
	return nil
}

// Terminal is the escape sequence that asks the terminal zorp runs in to
// show n, for the terminal identified by term ($TERM) and program
// ($TERM_PROGRAM): OSC 777 (Ghostty, foot, WezTerm…), OSC 99 (kitty) or
// OSC 9 (most others). Inside tmux it's wrapped for passthrough.
//
// The terminal shows it under its own name, so the title says it's zorp.
func Terminal(n Note, term, program string, tmux bool) string {
	title, body := "zorp · "+oneLine(n.Title), oneLine(strings.ReplaceAll(n.Body, "\n", " · "))
	var seq string
	switch t, p := strings.ToLower(term), strings.ToLower(program); {
	case strings.Contains(t, "kitty") || p == "kitty":
		seq = "\x1b]99;i=zorp:d=0:p=title;" + title + "\x1b\\" + "\x1b]99;i=zorp:d=1:p=body;" + body + "\x1b\\"
	case strings.Contains(t, "ghostty") || p == "ghostty" || strings.HasPrefix(t, "foot") || p == "wezterm":
		// Fields are separated by ";".
		seq = "\x1b]777;notify;" + strings.ReplaceAll(title, ";", ",") + ";" + strings.ReplaceAll(body, ";", ",") + "\x1b\\"
	default:
		seq = "\x1b]9;" + title + ": " + body + "\x07"
	}
	if tmux {
		seq = ansi.TmuxPassthrough(seq)
	}
	return seq
}

// oneLine keeps text safe inside an escape sequence: no control
// characters (including ESC and BEL), one line.
func oneLine(s string) string { return clean(s) }
