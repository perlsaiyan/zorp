// Package tui is zorp's terminal UI. It reads only from the local store and
// asks the daemon (over its socket) for anything that needs the network.
package tui

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/tom/zorp/config"
	"github.com/tom/zorp/internal/ipc"
	"github.com/tom/zorp/internal/store"
)

type pane int

const (
	paneFolders pane = iota
	paneList
	panePreview
)

// layout is where the preview pane goes.
type layout int

const (
	previewRight layout = iota
	previewBottom
	previewHidden
)

type Model struct {
	ctx     context.Context
	cfg     *config.Config
	st      *store.Store
	account store.Account

	width, height int

	folders   []folderRow
	folderCur int   // cursor in the folder pane
	folderID  int64 // folder whose messages are listed

	msgs    []store.Message
	msgCur  int
	msgTop  int // first visible row of the list
	listLen int // rows the list showed in the last layout

	bodySeq   int // bumps on every selection change, to drop stale results
	bodyID    int64
	bodyText  string
	bodyState bodyState
	bodyErr   string
	preview   viewport.Model

	focus       pane
	layout      layout
	showFolders bool
	reading     bool // full-screen message view
	help        bool
	gPending    bool // saw "g", waiting for the folder letter

	daemonUp bool
	events   <-chan ipc.Event
	status   ipc.Status
	errText  string
}

// New builds the model with an initial synchronous load, so the first frame
// already shows the inbox.
func New(ctx context.Context, cfg *config.Config, st *store.Store, account store.Account) (*Model, error) {
	m := &Model{
		ctx:         ctx,
		cfg:         cfg,
		st:          st,
		account:     account,
		focus:       paneList,
		showFolders: true,
		preview:     viewport.New(),
	}
	loaded := loadData(ctx, st, account.ID, 0, "")
	if loaded.err != nil {
		return nil, loaded.err
	}
	m.applyLoaded(loaded)
	return m, nil
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(subscribeCmd(m.ctx, m.cfg), m.selectMessage())
}

func (m *Model) currentFolder() (folderRow, bool) {
	for _, f := range m.folders {
		if f.ID == m.folderID {
			return f, true
		}
	}
	return folderRow{}, false
}

func (m *Model) currentMessage() (store.Message, bool) {
	if m.msgCur < 0 || m.msgCur >= len(m.msgs) {
		return store.Message{}, false
	}
	return m.msgs[m.msgCur], true
}

func (m *Model) applyLoaded(l loadedMsg) {
	m.folders = l.folders
	folderChanged := l.folderID != m.folderID
	m.folderID = l.folderID
	m.msgs = l.msgs
	if folderChanged {
		m.msgCur, m.msgTop = 0, 0
	}
	if l.keep != "" {
		for i, msg := range m.msgs {
			if msg.GraphID == l.keep {
				m.msgCur = i
				break
			}
		}
	}
	m.msgCur = clamp(m.msgCur, 0, len(m.msgs)-1)
	for i, f := range m.folders {
		if f.ID == m.folderID && (folderChanged || m.folderCur >= len(m.folders)) {
			m.folderCur = i
		}
	}
	m.folderCur = clamp(m.folderCur, 0, len(m.folders)-1)
}

// selectMessage starts loading the body of the message under the cursor.
func (m *Model) selectMessage() tea.Cmd {
	m.bodySeq++
	msg, ok := m.currentMessage()
	if !ok {
		m.bodyID, m.bodyState, m.bodyText = 0, bodyNone, ""
		m.refreshPreview(true)
		return nil
	}
	if msg.ID == m.bodyID && m.bodyState == bodyReady {
		return nil
	}
	m.bodyID, m.bodyState, m.bodyText, m.bodyErr = msg.ID, bodyLoading, "", ""
	m.refreshPreview(true)
	return readBodyCmd(m.st, m.bodySeq, msg.ID)
}

func (m *Model) openFolder(id int64) tea.Cmd {
	if id == m.folderID {
		return nil
	}
	l := loadData(m.ctx, m.st, m.account.ID, id, "")
	if l.err != nil {
		m.errText = l.err.Error()
		return nil
	}
	m.applyLoaded(l)
	m.reading = false
	return m.selectMessage()
}

func (m *Model) openWellKnown(name string) tea.Cmd {
	for _, f := range m.folders {
		if f.WellKnown == name {
			m.focus = paneList
			return m.openFolder(f.ID)
		}
	}
	m.errText = "no " + name + " folder"
	return nil
}

func (m *Model) moveMessage(delta int) tea.Cmd {
	if len(m.msgs) == 0 {
		return nil
	}
	next := clamp(m.msgCur+delta, 0, len(m.msgs)-1)
	if next == m.msgCur {
		return nil
	}
	m.msgCur = next
	return m.selectMessage()
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.refreshPreview(false)
		return m, nil

	case tea.KeyPressMsg:
		return m, m.handleKey(msg.String())

	case loadedMsg:
		if msg.err != nil {
			m.errText = msg.err.Error()
			return m, nil
		}
		prev, _ := m.currentMessage()
		m.applyLoaded(msg)
		if cur, ok := m.currentMessage(); !ok || cur.ID != prev.ID {
			return m, m.selectMessage()
		}
		m.refreshPreview(false) // headers may have changed (e.g. read state)
		return m, nil

	case bodyMsg:
		if msg.seq != m.bodySeq {
			return m, nil
		}
		switch {
		case msg.err != nil:
			m.bodyState, m.bodyErr = bodyFailed, msg.err.Error()
		case msg.fetched:
			m.bodyState, m.bodyText = bodyReady, msg.text
		default:
			// Not cached yet: ask the daemon, once the cursor settles.
			seq := msg.seq
			return m, tea.Tick(fetchDelay, func(time.Time) tea.Msg { return fetchBodyTick{seq} })
		}
		m.refreshPreview(true)
		return m, nil

	case fetchBodyTick:
		if msg.seq != m.bodySeq {
			return m, nil
		}
		cur, ok := m.currentMessage()
		if !ok {
			return m, nil
		}
		return m, fetchBodyCmd(m.cfg, m.st, msg.seq, cur.ID, cur.GraphID)

	case subscribedMsg:
		m.daemonUp, m.status, m.events = true, msg.status, msg.events
		return m, waitEventCmd(msg.events)

	case daemonEventMsg:
		cmd := m.handleEvent(ipc.Event(msg))
		return m, tea.Batch(cmd, m.waitNext())

	case daemonGoneMsg:
		m.daemonUp = false
		m.events = nil
		// Retry in a bit; the daemon may be restarting.
		return m, tea.Tick(3*time.Second, func(time.Time) tea.Msg { return reconnectMsg{} })

	case reconnectMsg:
		return m, subscribeCmd(m.ctx, m.cfg)

	case syncDoneMsg:
		if msg.err != nil {
			m.errText = "sync: " + msg.err.Error()
		}
		return m, nil

	case loginDoneMsg:
		if msg.err != nil {
			m.errText = "login: " + msg.err.Error()
		}
		return m, nil
	}
	return m, nil
}

func (m *Model) handleEvent(ev ipc.Event) tea.Cmd {
	switch ev.Name {
	case ipc.EventStatus:
		var st ipc.Status
		if json.Unmarshal(ev.Data, &st) == nil {
			m.status = st
		}
	case ipc.EventChanged:
		keep := ""
		if cur, ok := m.currentMessage(); ok {
			keep = cur.GraphID
		}
		return loadCmd(m.st, m.account.ID, m.folderID, keep)
	}
	return nil
}

type loginDoneMsg struct{ err error }

func (m *Model) loginCmd() tea.Cmd {
	exe, err := os.Executable()
	if err != nil {
		m.errText = err.Error()
		return nil
	}
	// Runs `zorp login` in the terminal; it notifies the daemon itself.
	return tea.ExecProcess(exec.Command(exe, "login"), func(err error) tea.Msg { return loginDoneMsg{err} })
}

func (m *Model) handleKey(key string) tea.Cmd {
	if key == "ctrl+c" {
		return tea.Quit
	}
	m.errText = ""
	if m.help {
		m.help = false
		return nil
	}

	if m.gPending {
		m.gPending = false
		switch key {
		case "i":
			return m.openWellKnown("inbox")
		case "s":
			return m.openWellKnown("sentitems")
		case "d":
			return m.openWellKnown("drafts")
		case "a":
			return m.openWellKnown("archive")
		case "g":
			return m.moveMessage(-len(m.msgs))
		}
		return nil
	}

	if m.reading {
		return m.readerKey(key)
	}

	switch key {
	case "q":
		return tea.Quit
	case "?":
		m.help = true
		return nil
	case "g":
		m.gPending = true
		return nil
	case "tab":
		m.cycleFocus(1)
		return nil
	case "shift+tab":
		m.cycleFocus(-1)
		return nil
	case "w":
		m.layout = (m.layout + 1) % 3
		if m.layout == previewHidden && m.focus == panePreview {
			m.focus = paneList
		}
		m.refreshPreview(false)
		return nil
	case "b":
		m.showFolders = !m.showFolders
		if !m.showFolders && m.focus == paneFolders {
			m.focus = paneList
		}
		m.refreshPreview(false)
		return nil
	case "ctrl+r":
		return syncNowCmd(m.cfg)
	case "L":
		return m.loginCmd()
	}

	switch m.focus {
	case paneFolders:
		return m.folderKey(key)
	case panePreview:
		m.scrollKey(key)
		return nil
	}
	return m.listKey(key)
}

func (m *Model) cycleFocus(dir int) {
	panes := []pane{paneList}
	if m.showFolders {
		panes = []pane{paneFolders, paneList}
	}
	if m.layout != previewHidden {
		panes = append(panes, panePreview)
	}
	i := 0
	for j, p := range panes {
		if p == m.focus {
			i = j
		}
	}
	m.focus = panes[(i+dir+len(panes))%len(panes)]
}

func (m *Model) folderKey(key string) tea.Cmd {
	switch key {
	case "j", "down":
		m.folderCur = clamp(m.folderCur+1, 0, len(m.folders)-1)
	case "k", "up":
		m.folderCur = clamp(m.folderCur-1, 0, len(m.folders)-1)
	case "enter", "l", "right":
		if m.folderCur < len(m.folders) {
			m.focus = paneList
			return m.openFolder(m.folders[m.folderCur].ID)
		}
	}
	return nil
}

func (m *Model) listKey(key string) tea.Cmd {
	page := max(m.listLen-1, 1)
	switch key {
	case "j", "down":
		return m.moveMessage(1)
	case "k", "up":
		return m.moveMessage(-1)
	case "ctrl+d":
		return m.moveMessage(page / 2)
	case "ctrl+u":
		return m.moveMessage(-page / 2)
	case "pgdown", "ctrl+f":
		return m.moveMessage(page)
	case "pgup", "ctrl+b":
		return m.moveMessage(-page)
	case "home":
		return m.moveMessage(-len(m.msgs))
	case "end", "G":
		return m.moveMessage(len(m.msgs))
	case "enter", "v":
		if _, ok := m.currentMessage(); ok {
			m.reading = true
			m.refreshPreview(true)
		}
	case "h", "left":
		if m.showFolders {
			m.focus = paneFolders
		}
	}
	return nil
}

func (m *Model) readerKey(key string) tea.Cmd {
	switch key {
	case "q", "esc", "h", "left":
		m.reading = false
		m.refreshPreview(false)
	case "n", "J":
		return m.moveMessage(1)
	case "p", "K":
		return m.moveMessage(-1)
	case "?":
		m.help = true
	default:
		m.scrollKey(key)
	}
	return nil
}

func (m *Model) scrollKey(key string) {
	switch key {
	case "j", "down":
		m.preview.ScrollDown(1)
	case "k", "up":
		m.preview.ScrollUp(1)
	case "space", "pgdown", "ctrl+f":
		m.preview.PageDown()
	case "-", "pgup", "ctrl+b":
		m.preview.PageUp()
	case "ctrl+d":
		m.preview.HalfPageDown()
	case "ctrl+u":
		m.preview.HalfPageUp()
	case "home":
		m.preview.GotoTop()
	case "end", "G":
		m.preview.GotoBottom()
	}
}

// waitNext keeps listening for daemon events after handling one.
func (m *Model) waitNext() tea.Cmd {
	if m.events == nil {
		return nil
	}
	return waitEventCmd(m.events)
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	return min(max(v, lo), hi)
}
