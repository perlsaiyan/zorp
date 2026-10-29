// Command demo fills a zorp cache with made-up mail and stands in for the
// daemon, so zorp can run without an account. scripts/screenshots.sh uses
// it to take the README's screenshots.
//
//	XDG_DATA_HOME=... XDG_CONFIG_HOME=... XDG_RUNTIME_DIR=... go run ./scripts/demo
//
// It writes the cache and then serves the daemon's socket until killed.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/store"
)

const me = "sam@example.com"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if err := cfg.EnsureDataDir(); err != nil {
		log.Fatal(err)
	}
	os.Remove(cfg.Paths.DB)
	if err := seed(ctx, cfg.Paths.DB, time.Now()); err != nil {
		log.Fatal(err)
	}
	os.Remove(cfg.Paths.Socket)
	ln, err := net.Listen("unix", cfg.Paths.Socket)
	if err != nil {
		log.Fatal(err)
	}
	started := time.Now().Add(-2 * time.Minute)
	srv := ipc.NewServer(func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
		switch method {
		case ipc.MethodStatus:
			return ipc.Status{State: ipc.StateIdle, Account: me, LastSync: started.Unix(), PID: os.Getpid()}, nil
		case ipc.MethodMutate:
			return ipc.MutateResult{}, nil
		case ipc.MethodMeeting:
			return ipc.MeetingInfo{Found: true, EventID: "e1", Response: "tentativelyAccepted", Organizer: false, JoinURL: "https://teams.example.com/l/meetup-join/1on1"}, nil
		}
		return struct{}{}, nil
	}, slog.Default())
	fmt.Println("demo daemon on", cfg.Paths.Socket)
	if err := srv.Serve(ctx, ln); err != nil {
		log.Fatal(err)
	}
}

type msg struct {
	folder  string
	conv    string
	from    store.Address
	to      []store.Address
	subject string
	ago     time.Duration
	read    bool
	flagged bool
	attach  []store.Attachment
	html    string
	text    string
	meeting *store.Meeting
}

var (
	sam    = store.Address{Name: "Sam Rivera", Addr: me}
	priya  = store.Address{Name: "Priya Natarajan", Addr: "priya@example.com"}
	marcus = store.Address{Name: "Marcus Webb", Addr: "marcus@example.com"}
	lena   = store.Address{Name: "Lena Okafor", Addr: "lena@example.com"}
	ops    = store.Address{Name: "Build Bot", Addr: "ci@example.com"}
	hr     = store.Address{Name: "People Team", Addr: "people@example.com"}
	dana   = store.Address{Name: "Dana Kim", Addr: "dana@partner.example.org"}
	news   = store.Address{Name: "The Weekly Terminal", Addr: "editor@newsletter.example.net"}
	cal    = store.Address{Name: "Jonas Berg", Addr: "jonas@example.com"}
)

// meetingAt puts a meeting on the coming Friday at 10.
func meetingAt(mt *store.Meeting, now time.Time) *store.Meeting {
	if mt == nil {
		return nil
	}
	day := now.AddDate(0, 0, (int(time.Friday)-int(now.Weekday())+7)%7)
	out := *mt
	out.Start = time.Date(day.Year(), day.Month(), day.Day(), 10, 0, 0, 0, time.Local)
	out.End = out.Start.Add(30 * time.Minute)
	return &out
}

func pdf(name string, size int64) []store.Attachment {
	return []store.Attachment{{GraphID: name, Name: name, ContentType: "application/pdf", Kind: "file", Size: size}}
}

var mail = []msg{
	{folder: "inbox", conv: "launch", from: priya, to: []store.Address{sam, marcus}, subject: "Re: Launch checklist for Thursday", ago: 12 * time.Minute,
		html: `<p>Sam,</p><p>Marcus and I went through the list this morning. Everything is green except two items:</p>
<ol><li><b>Status page</b>: the new incident template still points at staging.</li><li><b>Docs</b>: the migration guide needs a final read.</li></ol>
<p>Can you take the docs? I'll fix the status page before lunch.</p><p>Priya</p>
<blockquote><p>On Tue, Marcus Webb wrote:</p><p>Load tests finished overnight: p99 is 180&nbsp;ms at twice last year's peak. I'm happy with that.</p>
<blockquote><p>On Mon, Sam Rivera wrote:</p><p>Here's the checklist for Thursday. Shout if anything is missing.</p></blockquote></blockquote>`},
	{folder: "inbox", conv: "launch", from: marcus, to: []store.Address{sam, priya}, subject: "Re: Launch checklist for Thursday", ago: 3 * time.Hour, read: true,
		html: `<p>Load tests finished overnight: p99 is 180&nbsp;ms at twice last year's peak. I'm happy with that.</p><p>Graphs are in the dashboard.</p><p>— Marcus</p>
<blockquote><p>On Mon, Sam Rivera wrote:</p><p>Here's the checklist for Thursday. Shout if anything is missing.</p></blockquote>`},
	{folder: "sent", conv: "launch", from: sam, to: []store.Address{priya, marcus}, subject: "Launch checklist for Thursday", ago: 26 * time.Hour, read: true,
		text: "Here's the checklist for Thursday. Shout if anything is missing.\n\n- Load test at 2x peak\n- Status page template\n- Migration guide\n- Rollback plan rehearsed\n\nSam"},
	{folder: "inbox", conv: "contract", from: dana, to: []store.Address{sam}, subject: "[EXTERNAL] Signed renewal attached", ago: 47 * time.Minute, flagged: true,
		attach: pdf("Renewal-2027-signed.pdf", 412_331),
		html:   `<p>Hi Sam,</p><p>Please find the countersigned renewal attached. Our finance team will send the PO separately next week.</p><p>Thanks for turning this around so quickly!</p><p>Dana Kim<br>Partnerships, Example Org</p>`},
	{folder: "inbox", conv: "ci", from: ops, to: []store.Address{sam}, subject: "main: build #4182 passed", ago: 95 * time.Minute, read: true,
		text: "Build #4182 on main passed in 6m 12s.\n\n  test      ok   412 passed\n  lint      ok\n  release   skipped (not a tag)\n"},
	{folder: "inbox", conv: "offsite", from: lena, to: []store.Address{sam}, subject: "Offsite agenda, first draft", ago: 4 * time.Hour,
		attach: []store.Attachment{{GraphID: "a1", Name: "offsite-agenda.docx", ContentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", Kind: "file", Size: 88_211}, {GraphID: "a2", Name: "venue-map.png", ContentType: "image/png", Kind: "file", Size: 1_204_551}},
		html:   `<p>Hey Sam — first pass at the agenda is attached. The big open question is whether we do the roadmap session on day one or day two.</p><p>Lena</p>`},
	{folder: "inbox", conv: "1on1", from: cal, to: []store.Address{sam}, subject: "Updated invitation: Sam / Jonas 1:1", ago: 5 * time.Hour,
		meeting: &store.Meeting{Kind: store.MeetingRequest, Location: "Microsoft Teams Meeting", ResponseRequested: true, Recurring: true},
		text:    "Moved to Fridays at 10, same link. Jonas"},
	{folder: "inbox", conv: "1on1", from: cal, to: []store.Address{sam}, subject: "Moving our 1:1 to Friday?", ago: 6 * time.Hour, read: true,
		text: "Something came up Thursday afternoon. Could we do Friday at 10 instead? Same link.\n\nJonas"},
	{folder: "inbox", conv: "news", from: news, to: []store.Address{sam}, subject: "This week: tmux tricks, Kitty graphics, and a tiny Forth", ago: 20 * time.Hour, read: true,
		html: `<h2>This week</h2><ul><li>Ten tmux bindings you'll actually keep</li><li>How the Kitty graphics protocol puts pictures in your terminal</li><li>A Forth in 300 lines</li></ul><p><a href="https://newsletter.example.net/42">Read it on the web</a></p>`},
	{folder: "inbox", conv: "benefits", from: hr, to: []store.Address{sam}, subject: "Open enrollment closes Friday", ago: 28 * time.Hour, read: true,
		text: "A reminder that open enrollment closes this Friday at 5pm. If you don't make changes, your current elections carry over."},
	{folder: "inbox", conv: "review", from: marcus, to: []store.Address{sam}, subject: "Can you review the sync retry PR?", ago: 30 * time.Hour, read: true, flagged: true,
		text: "It's small: backoff with jitter on 429s, and we respect Retry-After now. Tests included."},
	{folder: "inbox", conv: "lunch", from: priya, to: []store.Address{sam}, subject: "Lunch Wednesday?", ago: 2 * 24 * time.Hour, read: true,
		text: "The new ramen place opened. 12:30?"},
	{folder: "inbox", conv: "q3", from: lena, to: []store.Address{sam}, subject: "Q3 numbers for the board deck", ago: 3 * 24 * time.Hour, read: true,
		attach: pdf("Q3-summary.pdf", 2_310_004),
		text:   "Final numbers attached. Slide 4 is the one they'll ask about."},
	{folder: "inbox", conv: "ci2", from: ops, to: []store.Address{sam}, subject: "main: build #4177 failed", ago: 3*24*time.Hour + time.Hour, read: true,
		text: "Build #4177 on main failed: TestOutboxRetry timed out after 30s."},
	{folder: "inbox", conv: "travel", from: hr, to: []store.Address{sam}, subject: "Your travel is booked", ago: 4 * 24 * time.Hour, read: true,
		text: "Flights and hotel for the offsite are confirmed. Details are in the travel portal."},
	{folder: "inbox", conv: "intro", from: dana, to: []store.Address{sam}, subject: "Intro: our new solutions engineer", ago: 5 * 24 * time.Hour, read: true,
		text: "Sam, meet Alex, who'll be your day-to-day contact from next month."},
	{folder: "inbox", conv: "oncall", from: ops, to: []store.Address{sam}, subject: "You're on call next week", ago: 6 * 24 * time.Hour, read: true,
		text: "Your on-call shift starts Monday 09:00."},
	{folder: "archive", conv: "old", from: priya, to: []store.Address{sam}, subject: "Retro notes", ago: 12 * 24 * time.Hour, read: true,
		text: "Notes from the retro are in the wiki."},
}

func seed(ctx context.Context, path string, now time.Time) error {
	st, err := store.Open(path)
	if err != nil {
		return err
	}
	defer st.Close()
	acct, err := st.UpsertAccount(ctx, me, sam.Name)
	if err != nil {
		return err
	}
	unread := map[string]int{}
	total := map[string]int{}
	for _, m := range mail {
		total[m.folder]++
		if !m.read {
			unread[m.folder]++
		}
	}
	folders := []store.Folder{
		{GraphID: "inbox", Name: "Inbox", WellKnown: "inbox"},
		{GraphID: "drafts", Name: "Drafts", WellKnown: "drafts"},
		{GraphID: "sent", Name: "Sent Items", WellKnown: "sentitems"},
		{GraphID: "archive", Name: "Archive", WellKnown: "archive"},
		{GraphID: "deleted", Name: "Deleted Items", WellKnown: "deleteditems"},
		{GraphID: "junk", Name: "Junk Email", WellKnown: "junkemail"},
		{GraphID: "projects", Name: "Projects"},
		{GraphID: "launch", Name: "Launch", ParentGraphID: "projects"},
		{GraphID: "receipts", Name: "Receipts"},
	}
	for i := range folders {
		folders[i].UnreadCount, folders[i].TotalCount = unread[folders[i].GraphID], total[folders[i].GraphID]
	}
	if err := st.ReplaceFolders(ctx, acct, folders); err != nil {
		return err
	}
	saved, err := st.Folders(ctx, acct)
	if err != nil {
		return err
	}
	ids := map[string]int64{}
	for _, f := range saved {
		ids[f.GraphID] = f.ID
		if err := st.SetDeltaLink(ctx, f.ID, "demo", now.Add(-2*time.Minute)); err != nil {
			return err
		}
	}
	batches := map[string][]store.Message{}
	for i, m := range mail {
		preview := m.text
		if preview == "" {
			preview = m.subject
		}
		batches[m.folder] = append(batches[m.folder], store.Message{
			GraphID:        "m" + strconv.Itoa(i),
			FolderID:       ids[m.folder],
			ConversationID: m.conv,
			Subject:        m.subject,
			From:           m.from,
			To:             m.to,
			ReceivedAt:     now.Add(-m.ago),
			IsRead:         m.read,
			Flagged:        m.flagged,
			HasAttachments: len(m.attach) > 0,
			Importance:     "normal",
			BodyPreview:    preview,
			Meeting:        meetingAt(m.meeting, now),
		})
	}
	for folder, b := range batches {
		if err := st.ApplyDelta(ctx, acct, ids[folder], store.DeltaBatch{Upserts: b}); err != nil {
			return err
		}
	}
	for i, m := range mail {
		id := "m" + strconv.Itoa(i)
		body := store.Body{Type: "text", Text: m.text}
		if m.html != "" {
			body = store.Body{Type: "html", HTML: m.html}
		}
		if err := st.SetBody(ctx, acct, id, body); err != nil {
			return err
		}
		if err := st.SetAttachments(ctx, acct, id, m.attach); err != nil {
			return err
		}
	}
	return nil
}
