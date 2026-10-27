package compose

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/perlsaiyan/zorp/internal/markdown"
	"github.com/perlsaiyan/zorp/internal/store"
)

var (
	alice = store.Address{Name: "Alice Example", Addr: "alice@example.com"}
	bob   = store.Address{Name: "Allen, Bob", Addr: "bob@example.com"}
	me    = store.Address{Name: "Tom", Addr: "tom@example.com"}
	list  = store.Address{Name: "Team", Addr: "team@example.com"}
)

func orig() Original {
	return Original{
		Message: store.Message{
			Subject:    "[EXTERNAL]Quarterly numbers",
			From:       alice,
			To:         []store.Address{me, bob},
			Cc:         []store.Address{list, alice},
			ReceivedAt: time.Date(2026, 10, 26, 9, 0, 0, 0, time.Local),
		},
		Text: "See attached.\n\n> earlier\nThanks",
	}
}

func TestRoundTrip(t *testing.T) {
	m := Message{
		To:      []store.Address{alice, bob, {Addr: "plain@example.com"}, {Name: `Q "Quote" Person`, Addr: "q@example.com"}, {Name: "Zoë Ünicode", Addr: "z@example.com"}},
		Bcc:     []store.Address{me},
		Subject: "Hello  there",
		Format:  FormatMarkdown,
		Body:    "Body line\n\n> quoted\n",
		Attach:  []string{"~/a file.pdf"},
	}
	text := File(New, m, []Existing{{ID: "x", Name: "old.pdf"}})
	got, err := Parse(text)
	if err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	m.Subject = "Hello there"
	m.Attached = []string{"old.pdf"}
	if !reflect.DeepEqual(got, m) {
		t.Errorf("round trip:\n got %+v\nwant %+v\nfile:\n%s", got, m, text)
	}
}

func TestParseForgivingAndStrict(t *testing.T) {
	got, err := Parse("# comment\nto: alice@example.com; Bob <bob@example.com>,\n  carol@example.com\nSubject: hi\nAttach:\n\nbody\n\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.To) != 3 || got.To[2].Addr != "carol@example.com" || got.Body != "body\n" || len(got.Attach) != 0 {
		t.Errorf("got %+v", got)
	}
	if _, err := Parse("To: not an address\nSubject: x\n\nbody"); err == nil || !strings.Contains(err.Error(), "To:") {
		t.Errorf("bad address: %v", err)
	}
	if _, err := Parse("To: a@example.com\nhello there\n\nbody"); err == nil {
		t.Error("stray header line accepted")
	}
	if _, err := Parse("To: a@example.com\nX-Evil: 1\n\nbody"); err == nil {
		t.Error("unknown header accepted")
	}
	if got, _ := Parse("To: a@example.com\n\n  \n"); got.Body != "" {
		t.Errorf("blank body = %q", got.Body)
	}
}

func TestReplyRecipients(t *testing.T) {
	self := []string{"TOM@example.com"}
	r := ReplyTo(orig(), false, self, "", FormatText)
	if !reflect.DeepEqual(r.To, []store.Address{alice}) || len(r.Cc) != 0 {
		t.Errorf("reply: to=%v cc=%v", r.To, r.Cc)
	}
	if r.Subject != "Re: [EXTERNAL]Quarterly numbers" {
		t.Errorf("subject = %q", r.Subject)
	}
	ra := ReplyTo(orig(), true, self, "", FormatText)
	if !reflect.DeepEqual(ra.To, []store.Address{alice}) || !reflect.DeepEqual(ra.Cc, []store.Address{bob, list}) {
		t.Errorf("reply all: to=%v cc=%v (self and duplicates dropped)", ra.To, ra.Cc)
	}

	o := orig()
	o.ReplyTo = []store.Address{list}
	if r := ReplyTo(o, false, self, "", FormatText); !reflect.DeepEqual(r.To, []store.Address{list}) {
		t.Errorf("Reply-To ignored: %v", r.To)
	}

	// Replying to our own sent message goes to its recipients.
	o = orig()
	o.From = me
	o.Subject = "RE: Quarterly numbers"
	r = ReplyTo(o, true, self, "", FormatText)
	if !reflect.DeepEqual(r.To, []store.Address{me, bob}) || !reflect.DeepEqual(r.Cc, []store.Address{list, alice}) || r.Subject != "RE: Quarterly numbers" {
		t.Errorf("reply to own message: %+v", r)
	}
}

func TestReplyBodyQuotesAndSigns(t *testing.T) {
	r := ReplyTo(orig(), false, nil, "Tom\nCTO", FormatText)
	want := "\n\n-- \nTom\nCTO\n\nOn Mon, 26 Oct 2026 at 09:00, Alice Example <alice@example.com> wrote:\n> See attached.\n>\n>> earlier\n> Thanks\n"
	if r.Body != want {
		t.Errorf("body:\n%q\nwant\n%q", r.Body, want)
	}
}

func TestForward(t *testing.T) {
	f := ForwardOf(orig(), "", FormatText)
	if f.Subject != "Fw: [EXTERNAL]Quarterly numbers" || len(f.To) != 0 {
		t.Errorf("forward: %+v", f)
	}
	for _, want := range []string{"---------- Forwarded message ---------", "From: Alice Example <alice@example.com>", `To: Tom <tom@example.com>, "Allen, Bob" <bob@example.com>`, "\nSee attached.\n"} {
		if !strings.Contains(f.Body, want) {
			t.Errorf("forward body missing %q:\n%s", want, f.Body)
		}
	}
	o := orig()
	o.Subject = "FWD: x"
	if f := ForwardOf(o, "", FormatText); f.Subject != "FWD: x" {
		t.Errorf("double prefix: %q", f.Subject)
	}
}

func FuzzParse(f *testing.F) {
	f.Add(File(Reply, ReplyTo(orig(), true, nil, "sig", FormatMarkdown), []Existing{{Name: "a"}}))
	f.Add("To: \"x\n\n")
	f.Fuzz(func(t *testing.T, s string) {
		Parse(s) // must not panic
	})
}

func TestTemplatesSanitizeUntrustedHeaders(t *testing.T) {
	o := orig()
	o.Subject = "Hi\x1b]52;c;ZXZpbA==\x07 \u202eevil"
	o.From = store.Address{Name: "\x1b[31mMallory", Addr: "m@example.com\x1b[2J"}
	for _, m := range []Message{ReplyTo(o, true, nil, "", FormatMarkdown), ForwardOf(o, "", FormatMarkdown)} {
		text := File(Reply, m, nil)
		if strings.ContainsAny(text, "\x1b\x07\u202e") {
			t.Errorf("control characters in template:\n%q", text)
		}
	}
}

func TestMarkdownTemplates(t *testing.T) {
	r := ReplyTo(orig(), false, nil, "Tom", FormatMarkdown)
	text := File(Reply, r, nil)
	got, err := Parse(text)
	if err != nil || got.Format != FormatMarkdown {
		t.Fatalf("format lost: %v %+v", err, got)
	}
	mine, ok := markdown.SplitRef(got.Body)
	if !ok || strings.TrimSpace(mine) != "-- \nTom" || !strings.Contains(got.Body, "> See attached.") {
		t.Errorf("reference split: %q %v\n%s", mine, ok, got.Body)
	}
	f := ForwardOf(orig(), "", FormatMarkdown)
	if _, ok := markdown.SplitRef(f.Body); !ok {
		t.Errorf("forward has no reference marker:\n%s", f.Body)
	}
	if _, err := Parse("To: a@example.com\nFormat: rtf\n\nx"); err == nil {
		t.Error("bad format accepted")
	}
}
