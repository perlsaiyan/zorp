package tui

import (
	"context"
	"errors"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/daemon"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/store"
)

// maxSearchResults caps local search results; the server is asked for fewer.
const (
	maxSearchResults  = 500
	serverSearchLimit = 100
)

// search is an active search: its results replace the folder's messages in
// the list until it is cleared.
type search struct {
	text    string
	kql     string   // set for a server search
	ids     []string // server results, re-read from the store on refresh
	more    bool     // results were capped
	running bool
	note    string // e.g. why the server was searched
}

// searchResultMsg carries results for the search started as seq.
type searchResultMsg struct {
	seq  int
	msgs []store.Message
	more bool
	ids  []string // server results
	kql  string   // set when a local search found nothing and the server should be asked
	keep string
	err  error
}

func newPrompt(value string, width int) *textinput.Model {
	ti := textinput.New()
	ti.Prompt = "/"
	ti.Placeholder = `words, "a phrase", from: to: subject: in: is:unread has:attachment after:7d · ?query searches the server`
	st := textinput.DefaultStyles(true)
	st.Focused.Prompt = lipgloss.NewStyle().Bold(true)
	st.Focused.Placeholder = styleFaint
	st.Cursor.Blink = false
	ti.SetStyles(st)
	ti.SetWidth(max(width-2, 10))
	ti.SetValue(value)
	ti.CursorEnd()
	ti.Focus()
	return &ti
}

func (m *Model) openPrompt() {
	text := ""
	if m.search != nil {
		text = m.search.text
	}
	m.prompt = newPrompt(text, m.width)
}

// promptMsg handles input while the search prompt is open.
func (m *Model) promptMsg(msg tea.Msg) tea.Cmd {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "enter":
			text := strings.TrimSpace(m.prompt.Value())
			m.prompt = nil
			return m.startSearch(text)
		case "esc", "ctrl+c":
			m.prompt = nil
			return nil
		}
	}
	var cmd tea.Cmd
	*m.prompt, cmd = m.prompt.Update(msg)
	return cmd
}

// startSearch runs text as a local search, or a server search when it
// starts with "?". Empty text clears the search.
func (m *Model) startSearch(text string) tea.Cmd {
	if text == "" || text == "?" {
		return m.clearSearch()
	}
	m.searchSeq++
	m.focus = paneList
	m.reading = false
	if kql, ok := strings.CutPrefix(text, "?"); ok {
		m.search = &search{text: text, kql: strings.TrimSpace(kql), running: true}
		m.setResults(nil)
		return tea.Batch(m.selectMessage(), serverSearchCmd(m.cfg, m.st, m.account.ID, m.searchSeq, m.search.kql))
	}
	q, err := store.ParseQuery(text, time.Now())
	if err == nil && q.Empty() {
		err = errors.New("nothing to search for")
	}
	if err != nil {
		m.errText = "search: " + err.Error()
		return nil
	}
	m.search = &search{text: text, running: true}
	return localSearchCmd(m.st, m.account.ID, m.searchSeq, q, "")
}

// clearSearch goes back to the folder the search started from.
func (m *Model) clearSearch() tea.Cmd {
	if m.search == nil {
		return nil
	}
	m.search = nil
	m.searchSeq++ // drop results still in flight
	l := loadData(m.ctx, m.st, m.account.ID, m.folderID, "")
	if l.err != nil {
		m.errText = l.err.Error()
		return nil
	}
	m.applyLoaded(l)
	m.msgCur, m.msgTop = 0, 0
	m.reading = false
	return m.selectMessage()
}

// setResults shows msgs in the list with the cursor on keep, or the top.
func (m *Model) setResults(msgs []store.Message) {
	m.msgs = msgs
	m.msgCur, m.msgTop = 0, 0
}

func (m *Model) handleSearchResult(r searchResultMsg) tea.Cmd {
	if r.seq != m.searchSeq || m.search == nil {
		return nil
	}
	s := m.search
	if r.err != nil {
		s.running = false
		m.errText = "search: " + r.err.Error()
		return nil
	}
	if r.kql != "" {
		// Nothing cached matched; ask the server.
		s.kql = r.kql
		s.note = "no local matches, searched the server"
		m.setResults(nil)
		return tea.Batch(m.selectMessage(), serverSearchCmd(m.cfg, m.st, m.account.ID, m.searchSeq, r.kql))
	}
	s.running = false
	s.more = r.more
	if r.ids != nil {
		s.ids = r.ids
	}
	prev, _ := m.currentMessage()
	m.setResults(r.msgs)
	if r.keep != "" {
		for i, msg := range m.msgs {
			if msg.GraphID == r.keep {
				m.msgCur = i
			}
		}
	}
	m.ensureVisible()
	if cur, ok := m.currentMessage(); !ok || cur.ID != prev.ID {
		return m.selectMessage()
	}
	m.refreshPreview(false)
	return nil
}

// refreshSearch re-reads the results after the cache changed.
func (m *Model) refreshSearch() tea.Cmd {
	s := m.search
	if s.running {
		return nil
	}
	keep := ""
	if cur, ok := m.currentMessage(); ok {
		keep = cur.GraphID
	}
	if s.kql != "" {
		seq, st, acct, ids, more := m.searchSeq, m.st, m.account.ID, s.ids, s.more
		return func() tea.Msg {
			msgs, err := st.MessagesByGraphIDs(context.Background(), acct, ids)
			return searchResultMsg{seq: seq, msgs: msgs, more: more, keep: keep, err: err}
		}
	}
	q, err := store.ParseQuery(s.text, time.Now())
	if err != nil {
		return nil
	}
	return localSearchCmd(m.st, m.account.ID, m.searchSeq, q, keep)
}

func localSearchCmd(st *store.Store, accountID int64, seq int, q store.Query, keep string) tea.Cmd {
	return func() tea.Msg {
		msgs, more, err := st.Search(context.Background(), accountID, q, maxSearchResults)
		r := searchResultMsg{seq: seq, msgs: msgs, more: more, keep: keep, err: err}
		if err == nil && len(msgs) == 0 && keep == "" {
			r.kql, _ = q.KQL()
		}
		return r
	}
}

func serverSearchCmd(cfg *config.Config, st *store.Store, accountID int64, seq int, kql string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c, err := daemon.Connect(ctx, cfg, true)
		if err != nil {
			return searchResultMsg{seq: seq, err: err}
		}
		defer c.Close()
		var res ipc.SearchResult
		if err := c.Call(ctx, ipc.MethodSearch, ipc.SearchParams{KQL: kql, Limit: serverSearchLimit}, &res); err != nil {
			return searchResultMsg{seq: seq, err: err}
		}
		ids := res.GraphIDs
		if ids == nil {
			ids = []string{}
		}
		msgs, err := st.MessagesByGraphIDs(ctx, accountID, ids)
		return searchResultMsg{seq: seq, msgs: msgs, ids: ids, more: len(ids) >= serverSearchLimit, err: err}
	}
}

// folderName is a short name for a folder ID, for the search results list.
func (m *Model) folderName(id int64) string {
	for _, f := range m.folders {
		if f.ID == id {
			return f.Name
		}
	}
	return "?"
}
