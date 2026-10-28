package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/store"
)

func msg(id, name, addr, subject string, read bool) store.Message {
	return store.Message{GraphID: id, From: store.Address{Name: name, Addr: addr}, Subject: subject, IsRead: read}
}

func defaults() config.Notify {
	return config.Notify{Enabled: true, Method: "auto", Folders: []string{"inbox"}, ShowContent: true, GroupAfter: 3}
}

func TestSelect(t *testing.T) {
	noon := time.Date(2026, 10, 28, 12, 0, 0, 0, time.Local)
	arrivals := []Arrival{
		{Folder: "Inbox", WellKnown: "inbox", Messages: []store.Message{
			msg("a", "Alice", "alice@example.com", "Hi", false),
			msg("b", "Bob", "bob@other.org", "Read already", true),
			msg("c", "Me", "ME@example.com", "Note to self", false),
		}},
		{Folder: "Archive", WellKnown: "archive", Messages: []store.Message{msg("d", "Dee", "dee@example.com", "x", false)}},
	}
	ids := func(ms []store.Message) string {
		var s []string
		for _, m := range ms {
			s = append(s, m.GraphID)
		}
		return strings.Join(s, ",")
	}
	cfg := defaults()
	if got := ids(Select(cfg, arrivals, "me@example.com", noon)); got != "a" {
		t.Errorf("default = %s", got)
	}
	cfg.Folders = []string{"inbox", "Archive"}
	cfg.From = []string{"@example.com"}
	if got := ids(Select(cfg, arrivals, "me@example.com", noon)); got != "a,d" {
		t.Errorf("folders+from = %s", got)
	}
	cfg.QuietHours = "11:00-13:00"
	if got := Select(cfg, arrivals, "me@example.com", noon); got != nil {
		t.Errorf("quiet hours: %v", got)
	}
	cfg = defaults()
	cfg.Enabled = false
	if Select(cfg, arrivals, "", noon) != nil {
		t.Error("disabled")
	}
}

func TestBuild(t *testing.T) {
	evil := msg("x", "\x1b]52;c;cHduZWQ=\x07Mallory", "m@example.com", "Invoice <b>now</b>\x1b[2J "+strings.Repeat("long ", 60), false)
	notes := Build(defaults(), []store.Message{evil})
	if len(notes) != 1 || strings.ContainsAny(notes[0].Title+notes[0].Body, "\x1b\x07") || notes[0].GraphID != "x" ||
		!strings.HasSuffix(notes[0].Body, "…") || !strings.Contains(notes[0].Title, "Mallory") {
		t.Errorf("single = %+v", notes)
	}
	var four []store.Message
	for _, n := range []string{"A", "B", "C", "D"} {
		four = append(four, msg(n, n, n+"@x.org", "S"+n, false))
	}
	notes = Build(defaults(), four)
	if len(notes) != 1 || notes[0].Title != "4 new messages" || !strings.Contains(notes[0].Body, "and 1 more") || notes[0].GraphID != "" {
		t.Errorf("grouped = %+v", notes)
	}
	cfg := defaults()
	cfg.ShowContent = false
	notes = Build(cfg, four[:1])
	if notes[0].Body != "1 new message" || strings.Contains(notes[0].Title+notes[0].Body, "SA") {
		t.Errorf("private = %+v", notes)
	}
}

func TestTerminal(t *testing.T) {
	n := Note{Title: "Al;ice\x1b\\", Body: "Hi\x07 there;\nsecond"}
	ghost := Terminal(n, "xterm-ghostty", "", false)
	if ghost != "\x1b]777;notify;zorp · Al,ice\\;Hi there, · second\x1b\\" {
		t.Errorf("ghostty = %q", ghost)
	}
	if k := Terminal(n, "xterm-kitty", "", false); !strings.HasPrefix(k, "\x1b]99;i=zorp:d=0:p=title;zorp · ") {
		t.Errorf("kitty = %q", k)
	}
	if o := Terminal(n, "xterm-256color", "", false); !strings.HasPrefix(o, "\x1b]9;") || strings.Count(o, "\x07") != 1 {
		t.Errorf("osc 9 = %q", o)
	}
	if tm := Terminal(n, "xterm-ghostty", "", true); !strings.HasPrefix(tm, "\x1bPtmux;") {
		t.Errorf("tmux = %q", tm)
	}
}

func TestUntag(t *testing.T) {
	for _, c := range []struct {
		in, want string
		external bool
	}{
		{"[EXTERNAL]test", "test", true},
		{"[External] [EXT] Re: lunch", "Re: lunch", true},
		{"EXTERNAL: Invoice", "Invoice", true},
		{"[EXTERNAL]", "[EXTERNAL]", false}, // nothing else to show
		{"Plain subject", "Plain subject", false},
	} {
		if got, ext := untag(c.in); got != c.want || ext != c.external {
			t.Errorf("untag(%q) = %q, %v", c.in, got, ext)
		}
	}
	notes := Build(defaults(), []store.Message{msg("x", "Jo Park", "jo@ext.example", "[EXTERNAL]test", false)})
	if notes[0].Title != "Jo Park (external)" || notes[0].Body != "test" {
		t.Errorf("note = %+v", notes[0])
	}
}
