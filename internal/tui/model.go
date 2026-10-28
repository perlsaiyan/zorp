// Package tui is zorp's terminal UI. It reads only from the local store and
// asks the daemon (over its socket) for anything that needs the network.
package tui

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/compose"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/render"
	"github.com/perlsaiyan/zorp/internal/store"
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

	bodySeq    int // bumps on every selection change, to drop stale results
	bodyID     int64
	doc        *render.Doc // parsed body of bodyID, once loaded
	showQuoted bool        // reveal folded quotes/signature (z)
	bodyState  bodyState
	bodyErr    string
	atts       []store.Attachment // bodyID's cached attachment list
	attsKnown  bool               // atts was cached (or has been asked for)
	preview    viewport.Model

	focus       pane
	layout      layout
	showFolders bool
	// Pane sizes set by dragging dividers; zero means the default.
	folderW    int
	listShare  float64 // list's share of the width, preview on the right
	listHShare float64 // list's share of the height, preview at the bottom
	dragging   divider
	reading    bool // full-screen message view
	help       bool
	picker     *picker // move-to-folder / link chooser, when open
	confirm    *confirm
	gPending   bool             // saw "g", waiting for the folder letter
	prompt     *textinput.Model // search prompt, when open
	search     *search          // active search, whose results are listed
	searchSeq  int              // bumps per search, to drop stale results
	composing  *composing       // message being written (review screen)
	viewer     *viewer          // attachment shown full screen
	gfx        gfxState         // inline images (Kitty graphics)
	// leaving is a message just archived, deleted or moved: when the list
	// refreshes, the cursor goes to the message after it.
	leaving string

	daemonUp bool
	events   <-chan ipc.Event
	status   ipc.Status
	errText  string
	notice   string // short non-error status, e.g. "Sent."
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
	m.loadUIState()
	loaded := loadData(ctx, st, account.ID, 0, "")
	if loaded.err != nil {
		return nil, loaded.err
	}
	m.applyLoaded(loaded)
	return m, nil
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(subscribeCmd(m.ctx, m.cfg), m.selectMessage(), m.startGraphics())
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
	if m.search != nil || (l.refresh && l.folderID != m.folderID) {
		// The list shows search results, or this background refresh was
		// started before the user opened another folder: only the folder
		// pane updates.
		m.folderCur = clamp(m.folderCur, 0, len(m.folders)-1)
		return
	}
	folderChanged := l.folderID != m.folderID
	m.folderID = l.folderID
	m.msgs = l.msgs
	if folderChanged {
		m.msgCur, m.msgTop = 0, 0
	}
	if l.keep != "" {
		m.msgCur = m.cursorAfterRefresh(l.keep, m.msgCur)
	}
	m.msgCur = clamp(m.msgCur, 0, len(m.msgs)-1)
	for i, f := range m.folders {
		if f.ID == m.folderID && (folderChanged || m.folderCur >= len(m.folders)) {
			m.folderCur = i
		}
	}
	m.folderCur = clamp(m.folderCur, 0, len(m.folders)-1)
}

// cursorAfterRefresh is where the cursor goes when the list was re-read:
// on keep (the message it was on), or the one after it if keep was just
// archived, deleted or moved but is still listed (search results). If
// keep is gone, the cursor stays at the same row, now the next message.
func (m *Model) cursorAfterRefresh(keep string, old int) int {
	leaving := keep == m.leaving
	if leaving {
		m.leaving = ""
	}
	for i, msg := range m.msgs {
		if msg.GraphID == keep {
			if leaving && i+1 < len(m.msgs) {
				return i + 1
			}
			return i
		}
	}
	return clamp(old, 0, len(m.msgs)-1)
}

// selectMessage starts loading the body of the message under the cursor.
func (m *Model) selectMessage() tea.Cmd {
	m.bodySeq++
	msg, ok := m.currentMessage()
	if !ok {
		m.bodyID, m.bodyState, m.doc = 0, bodyNone, nil
		m.refreshPreview(true)
		return nil
	}
	if msg.ID == m.bodyID && m.bodyState == bodyReady {
		return nil
	}
	m.bodyID, m.bodyState, m.doc, m.bodyErr = msg.ID, bodyLoading, nil, ""
	m.atts, m.attsKnown = nil, false
	m.showQuoted = isForward(msg.Subject)
	m.refreshPreview(true)
	return readBodyCmd(m.st, m.bodySeq, msg.ID)
}

func (m *Model) openFolder(id int64) tea.Cmd {
	if m.search != nil {
		m.search = nil
		m.searchSeq++
		m.folderID = 0 // force a reload, even of the same folder
	}
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
	if cmd, ok := m.graphicsEvent(msg); ok {
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.refreshPreview(false)
		if m.viewer != nil {
			m.layoutViewer()
		}
		return m, m.maybeLoadImages()

	case tea.KeyPressMsg:
		if m.prompt != nil {
			return m, m.promptMsg(msg)
		}
		cmd := m.handleKey(msg.String())
		return m, tea.Batch(cmd, m.maybeLoadImages())

	case imagesMsg:
		return m, m.imagesReady(msg)

	case tea.PasteMsg:
		if m.prompt != nil {
			return m, m.promptMsg(msg)
		}
		return m, nil

	case searchResultMsg:
		return m, m.handleSearchResult(msg)

	case composeReadyMsg:
		return m, m.composeReady(msg)

	case editorDoneMsg:
		return m, m.editorDone(msg.err)

	case composeDoneMsg:
		return m, m.composeDone(msg)

	case composeDiscardMsg:
		m.endCompose()
		m.notice = "Discarded."
		return m, nil

	case attachmentsMsg:
		m.openAttachmentPicker(msg)
		return m, nil

	case cachedAttsMsg:
		if msg.seq == m.bodySeq {
			m.atts = msg.atts
			m.refreshPreview(false)
		}
		return m, nil

	case openedMsg:
		return m, m.opened(msg)

	case filesMsg:
		m.openFilePicker(msg)
		return m, nil

	case savedMsg:
		m.saved(msg)
		return m, nil

	case tea.MouseMsg:
		if !m.cfg.UI.Mouse {
			return m, nil
		}
		cmd := m.handleMouse(msg)
		return m, tea.Batch(cmd, m.maybeLoadImages())

	case loadedMsg:
		if msg.err != nil {
			m.errText = msg.err.Error()
			return m, nil
		}
		prev, _ := m.currentMessage()
		m.applyLoaded(msg)
		if m.search != nil {
			return m, nil
		}
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
			m.bodyState, m.doc = bodyReady, parseBody(msg.body)
			m.atts, m.attsKnown = msg.atts, msg.attsKnown
			m.refreshPreview(true)
			return m, tea.Batch(m.maybeLoadImages(), m.fetchAttsCmd())
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

	case mutateDoneMsg:
		if msg.err != nil {
			m.errText = msg.err.Error()
			m.leaving = ""
		}
		return m, nil

	case exportedMsg:
		return m, m.exported(msg)

	case openDoneMsg:
		return m, m.openDone(msg)
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
	case ipc.EventError:
		var e ipc.ErrorEvent
		if json.Unmarshal(ev.Data, &e) == nil {
			m.errText = e.Message
		}
	case ipc.EventChanged:
		if m.search != nil {
			return tea.Batch(loadCmd(m.st, m.account.ID, m.folderID, ""), m.refreshSearch())
		}
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
	if m.composing == nil || m.composing.busy == "" {
		m.notice = ""
	}
	if m.help {
		m.help = false
		return nil
	}
	if m.confirm != nil {
		c := m.confirm
		m.confirm = nil
		if key == "y" || key == "Y" {
			m.leaving = c.leaving
			return c.cmd
		}
		return nil
	}
	if m.picker != nil {
		return m.pickerKey(key)
	}
	if m.viewer != nil {
		return m.viewerKey(key)
	}
	if m.composing != nil {
		return m.composeKey(key)
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

	if cmd, ok := m.actionKey(key); ok {
		return cmd
	}
	if m.reading {
		return m.readerKey(key)
	}

	switch key {
	case "q", "esc":
		if m.search != nil {
			return m.clearSearch()
		}
		if key == "q" {
			return tea.Quit
		}
		return nil
	case "/":
		m.openPrompt()
		return nil
	case "c":
		return m.startCompose(compose.New)
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
		m.saveUIState()
		return nil
	case "b":
		m.showFolders = !m.showFolders
		if !m.showFolders && m.focus == paneFolders {
			m.focus = paneList
		}
		m.refreshPreview(false)
		m.saveUIState()
		return nil
	case "+", "-":
		return m.toggleFolderSync(key == "+")
	case "ctrl+r":
		return syncNowCmd(m.cfg)
	case "L":
		return m.loginCmd()
	}

	switch m.focus {
	case paneFolders:
		return m.folderKey(key)
	case panePreview:
		scrollViewport(&m.preview, key)
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
			return m.markRead()
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
		return tea.Batch(m.moveMessage(1), m.markRead())
	case "p", "K":
		return tea.Batch(m.moveMessage(-1), m.markRead())
	case "?":
		m.help = true
	default:
		scrollViewport(&m.preview, key)
	}
	return nil
}

func scrollViewport(vp *viewport.Model, key string) {
	switch key {
	case "j", "down":
		vp.ScrollDown(1)
	case "k", "up":
		vp.ScrollUp(1)
	case "space", "pgdown", "ctrl+f":
		vp.PageDown()
	case "-", "pgup", "ctrl+b":
		vp.PageUp()
	case "ctrl+d":
		vp.HalfPageDown()
	case "ctrl+u":
		vp.HalfPageUp()
	case "home":
		vp.GotoTop()
	case "end", "G":
		vp.GotoBottom()
	}
}

// actionKey handles keys that change messages; they work in the list,
// preview and reader alike.
func (m *Model) actionKey(key string) (tea.Cmd, bool) {
	if m.focus == paneFolders && !m.reading {
		return nil, false
	}
	msg, ok := m.currentMessage()
	if !ok {
		return nil, false
	}
	one := func(op string) tea.Cmd {
		return mutateCmd(m.cfg, ipc.MutateParams{Op: op, GraphIDs: []string{msg.GraphID}})
	}
	switch key {
	case "u":
		if msg.IsRead {
			return one(ipc.OpUnread), true
		}
		return one(ipc.OpRead), true
	case "f":
		if msg.Flagged {
			return one(ipc.OpUnflag), true
		}
		return one(ipc.OpFlag), true
	case "d":
		m.leaving = msg.GraphID
		if m.cfg.Keys.DeleteArchives {
			return one(ipc.OpArchive), true
		}
		return one(ipc.OpDelete), true
	case "X":
		m.confirm = &confirm{prompt: "Delete this message (move to Deleted Items)? y/n", cmd: one(ipc.OpDelete), leaving: msg.GraphID}
		return nil, true
	case "a":
		m.leaving = msg.GraphID
		return one(ipc.OpArchive), true
	case "m":
		m.openMovePicker()
		return nil, true
	case "O":
		m.openLinkPicker()
		return nil, true
	case "o":
		return m.openMessage(msg.GraphID), true
	case "z":
		m.showQuoted = !m.showQuoted
		m.refreshPreview(false)
		return nil, true
	case "r":
		return m.startCompose(compose.Reply), true
	case "R":
		return m.startCompose(compose.ReplyAll), true
	case "F":
		return m.startCompose(compose.Forward), true
	case "e":
		return m.startCompose(compose.EditDraft), true
	case "s":
		return m.listAttachmentsCmd(false), true
	case "V":
		return m.listAttachmentsCmd(true), true
	}
	return nil, false
}

// toggleFolderSync turns syncing of the folder under the cursor (in the
// folder pane) or the open folder on or off.
func (m *Model) toggleFolderSync(on bool) tea.Cmd {
	f, ok := m.currentFolder()
	if m.focus == paneFolders && m.folderCur < len(m.folders) {
		f, ok = m.folders[m.folderCur], true
	}
	if !ok {
		return nil
	}
	if on {
		if f.synced && (f.SyncOverride == nil || *f.SyncOverride) {
			m.errText = clean(f.Name) + " is already synced"
			return nil
		}
		return setSyncCmd(m.cfg, f.ID, true)
	}
	if !f.synced && (f.SyncOverride == nil || !*f.SyncOverride) {
		m.errText = clean(f.Name) + " isn't synced"
		return nil
	}
	m.confirm = &confirm{
		prompt: "Stop syncing " + clean(f.Name) + "? Its cached messages are removed from this computer; nothing changes on the server. y/n",
		cmd:    setSyncCmd(m.cfg, f.ID, false),
	}
	return nil
}

// confirm is a pending y/n question; any key but y cancels.
type confirm struct {
	prompt  string
	cmd     tea.Cmd
	leaving string // set as Model.leaving on y
}

func parseBody(b store.Body) *render.Doc {
	if b.Type == "html" && b.HTML != "" {
		return render.FromHTML(b.HTML)
	}
	return render.FromText(b.Text)
}

// isForward reports forwarded messages, whose quoted part is the content,
// so it isn't folded.
func isForward(subject string) bool {
	s := strings.ToLower(strings.TrimSpace(subject))
	for strings.HasPrefix(s, "[external]") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "[external]"))
	}
	return strings.HasPrefix(s, "fw:") || strings.HasPrefix(s, "fwd:")
}

// markRead marks the message under the cursor read, as opening it does in
// most mail clients. The cursor may have just moved, so it reads the
// current message rather than taking one.
func (m *Model) markRead() tea.Cmd {
	msg, ok := m.currentMessage()
	if !ok || msg.IsRead {
		return nil
	}
	return mutateCmd(m.cfg, ipc.MutateParams{Op: ipc.OpRead, GraphIDs: []string{msg.GraphID}})
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
