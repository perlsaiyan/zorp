package tui

import (
	"context"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/render"
	"github.com/perlsaiyan/zorp/internal/store"
)

// Conversation view (T). The list shows one row per conversation: its
// newest message stands for it, and actions on that row apply to every
// message of the conversation in the list. Opening it shows the whole
// conversation, including replies in other folders, oldest first.

// setList shows msgs (newest first) in the list, collapsed into
// conversations in the conversation view.
func (m *Model) setList(msgs []store.Message) {
	m.allMsgs = msgs
	if !m.threaded {
		m.msgs, m.threads = msgs, nil
		return
	}
	m.msgs, m.threads = collapse(msgs)
}

// collapse groups msgs by conversation, keeping the order of each
// conversation's newest message. threads maps that message's graph ID to
// all of the conversation's messages in msgs, newest first.
func collapse(msgs []store.Message) (rows []store.Message, threads map[string][]store.Message) {
	threads = map[string][]store.Message{}
	head := map[string]string{} // conversation -> graph ID of its row
	for _, msg := range msgs {
		conv := msg.ConversationID
		if conv == "" {
			conv = "\x00" + msg.GraphID
		}
		if h, ok := head[conv]; ok {
			threads[h] = append(threads[h], msg)
			continue
		}
		head[conv] = msg.GraphID
		threads[msg.GraphID] = []store.Message{msg}
		rows = append(rows, msg)
	}
	return rows, threads
}

// members are the messages a row stands for: its conversation's, in the
// conversation view, else just itself.
func (m *Model) members(msg store.Message) []store.Message {
	if t, ok := m.threads[msg.GraphID]; ok {
		return t
	}
	return []store.Message{msg}
}

func (m *Model) memberIDs(msg store.Message) []string {
	var ids []string
	for _, mm := range m.members(msg) {
		ids = append(ids, mm.GraphID)
	}
	return ids
}

// rowState sums up a row's messages for the list.
func (m *Model) rowState(msg store.Message) (unread, flagged, attachments bool, n int) {
	for _, mm := range m.members(msg) {
		unread = unread || !mm.IsRead
		flagged = flagged || mm.Flagged
		attachments = attachments || mm.HasAttachments
		n++
	}
	return unread, flagged, attachments, n
}

// readOp is what u does to a row: mark it all read if anything is
// unread, else mark the newest message unread.
func (m *Model) readOp(msg store.Message) ipc.MutateParams {
	var unread []string
	for _, mm := range m.members(msg) {
		if !mm.IsRead {
			unread = append(unread, mm.GraphID)
		}
	}
	if len(unread) > 0 {
		return ipc.MutateParams{Op: ipc.OpRead, GraphIDs: unread}
	}
	return ipc.MutateParams{Op: ipc.OpUnread, GraphIDs: []string{msg.GraphID}}
}

// flagOp is what f does to a row: clear any flags, else flag the newest.
func (m *Model) flagOp(msg store.Message) ipc.MutateParams {
	var flagged []string
	for _, mm := range m.members(msg) {
		if mm.Flagged {
			flagged = append(flagged, mm.GraphID)
		}
	}
	if len(flagged) > 0 {
		return ipc.MutateParams{Op: ipc.OpUnflag, GraphIDs: flagged}
	}
	return ipc.MutateParams{Op: ipc.OpFlag, GraphIDs: []string{msg.GraphID}}
}

// toggleThreaded switches between the conversation view and the flat
// list, keeping the cursor on the same message's row.
func (m *Model) toggleThreaded() tea.Cmd {
	cur, _ := m.currentMessage()
	m.threaded = !m.threaded
	m.selected = nil
	m.setList(m.allMsgs)
	m.msgCur = 0
	for i, msg := range m.msgs {
		for _, mm := range m.members(msg) {
			if mm.GraphID == cur.GraphID {
				m.msgCur = i
			}
		}
	}
	m.msgTop = 0
	m.saveUIState()
	if m.threaded {
		m.notice = "Conversations: one row per conversation (T for every message)"
	} else {
		m.notice = "Every message (T for conversations)"
	}
	cmd := m.selectMessage()
	m.refreshPreview(false) // the same message may now stand for a conversation
	return cmd
}

// conversation is a whole conversation, loaded for the reader.
type conversation struct {
	forID string // graph ID of the row it was opened from
	msgs  []store.Message
	docs  map[int64]*render.Doc
	err   string
}

type convLoadedMsg struct {
	seq int
	c   *conversation
}

// loadConversation loads the current row's conversation for the reader,
// if it has more than one message.
func (m *Model) loadConversation() tea.Cmd {
	m.conv = nil
	msg, ok := m.currentMessage()
	if !ok || !m.reading || !m.threaded || msg.ConversationID == "" {
		return nil
	}
	cfg, st, acct, seq := m.cfg, m.st, m.account.ID, m.bodySeq
	skip := map[int64]bool{}
	for _, f := range m.folders {
		if f.WellKnown == "deleteditems" || f.WellKnown == "junkemail" {
			skip[f.ID] = true
		}
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		return convLoadedMsg{seq: seq, c: readConversation(ctx, cfg, st, acct, msg, skip)}
	}
}

// maxConversation caps how many messages the reader shows.
const maxConversation = 100

func readConversation(ctx context.Context, cfg *config.Config, st *store.Store, acct int64, msg store.Message, skip map[int64]bool) *conversation {
	c := &conversation{forID: msg.GraphID, docs: map[int64]*render.Doc{}}
	all, err := st.MessagesInConversation(ctx, acct, msg.ConversationID, maxConversation)
	if err != nil {
		c.err = err.Error()
		return c
	}
	for _, mm := range all {
		if !skip[mm.FolderID] || mm.GraphID == msg.GraphID {
			c.msgs = append(c.msgs, mm)
		}
	}
	for _, mm := range c.msgs {
		body, fetched, err := st.MessageBody(ctx, mm.ID)
		if err == nil && !fetched {
			if err = callDaemon(ctx, cfg, ipc.MethodFetchBody, ipc.FetchBodyParams{GraphID: mm.GraphID}, nil); err == nil {
				body, fetched, err = st.MessageBody(ctx, mm.ID)
			}
		}
		if err == nil && fetched {
			c.docs[mm.ID] = parseBody(body)
		}
	}
	return c
}

func (m *Model) conversationLoaded(r convLoadedMsg) {
	if r.seq != m.bodySeq || !m.reading {
		return
	}
	cur, ok := m.currentMessage()
	if !ok || cur.GraphID != r.c.forID {
		return
	}
	if len(r.c.msgs) < 2 && r.c.err == "" {
		return // just the one message: the normal reader
	}
	m.conv = r.c
	m.refreshPreview(true)
	m.preview.SetYOffset(m.convAnchor)
}

// conversationContent renders the conversation, oldest first, and notes
// where the first unread message starts (or the last, if all are read).
func (m *Model) conversationContent(w int) string {
	c := m.conv
	var lines []string
	if c.err != "" {
		return styleError.Render("Couldn't load the conversation: " + clean(c.err))
	}
	subject := clean(c.msgs[len(c.msgs)-1].Subject)
	lines = append(lines, ansi.Wrap(styleBold.Render(subject), w, ""),
		styleFaint.Render(strconv.Itoa(len(c.msgs))+" messages, oldest first · z shows quoted text"), "")
	anchor, last := -1, 0
	for _, msg := range c.msgs {
		if anchor < 0 && !msg.IsRead {
			anchor = len(lines)
		}
		last = len(lines)
		head := styleBold.Render(clean(senderName(msg.From))) + styleFaint.Render("  "+msg.ReceivedAt.Local().Format("Mon, 02 Jan 2006 15:04"))
		if f := m.folderName(msg.FolderID); f != "" && msg.FolderID != m.folderID {
			head += styleFaint.Render("  · " + clean(f))
		}
		if !msg.IsRead {
			head += " " + styleAccent.Render("new")
		}
		lines = append(lines, styleFaint.Render(strings.Repeat("─", w)), ansi.Truncate(head, w, "…"))
		if to := formatAddrs(msg.To); to != "" {
			lines = append(lines, ansi.Truncate(styleHeader.Render("To:")+" "+to, w, "…"))
		}
		lines = append(lines, "")
		if doc := c.docs[msg.ID]; doc != nil {
			body := doc.Render(w, render.Options{ShowQuoted: m.showQuoted, Styles: &renderStyles})
			if len(body) == 0 {
				body = []string{styleFaint.Render("(no text)")}
			}
			lines = append(lines, body...)
		} else {
			lines = append(lines, strings.Split(ansi.Wrap(cleanBody(msg.BodyPreview), w, ""), "\n")...)
			lines = append(lines, styleFaint.Render("(only the preview: the message isn't cached)"))
		}
		lines = append(lines, "")
	}
	if anchor < 0 {
		anchor = last
	}
	m.convAnchor = anchor
	return strings.Join(lines, "\n")
}
