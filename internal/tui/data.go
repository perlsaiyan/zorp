package tui

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/daemon"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/store"
)

// maxListed caps how many messages of one folder are loaded into the list.
const maxListed = 10000

// folderRow is a folder as shown in the folder pane, in tree order.
type folderRow struct {
	store.Folder
	depth  int
	synced bool // messages are cached locally
	unread int
}

// wellKnownOrder puts standard folders first, in the order Outlook shows them.
var wellKnownOrder = []string{"inbox", "drafts", "sentitems", "archive", "deleteditems", "junkemail", "outbox"}

// folderTree orders folders as a tree: standard folders first, then the
// rest alphabetically, each followed by its children.
func folderTree(folders []store.Folder, unread map[int64]int) []folderRow {
	byID := map[string]bool{}
	children := map[string][]store.Folder{}
	for _, f := range folders {
		byID[f.GraphID] = true
	}
	var roots []store.Folder
	for _, f := range folders {
		if byID[f.ParentGraphID] {
			children[f.ParentGraphID] = append(children[f.ParentGraphID], f)
		} else {
			roots = append(roots, f)
		}
	}
	rank := func(f store.Folder) int {
		if i := slices.Index(wellKnownOrder, f.WellKnown); i >= 0 {
			return i
		}
		return len(wellKnownOrder)
	}
	sortFolders := func(fs []store.Folder) {
		slices.SortStableFunc(fs, func(a, b store.Folder) int {
			if ra, rb := rank(a), rank(b); ra != rb {
				return ra - rb
			}
			return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		})
	}
	var out []folderRow
	var walk func(fs []store.Folder, depth int)
	walk = func(fs []store.Folder, depth int) {
		sortFolders(fs)
		for _, f := range fs {
			row := folderRow{Folder: f, depth: depth, synced: !f.LastSync.IsZero(), unread: f.UnreadCount}
			if row.synced {
				row.unread = unread[f.ID]
			}
			out = append(out, row)
			walk(children[f.GraphID], depth+1)
		}
	}
	walk(roots, 0)
	return out
}

// loadedMsg carries a fresh read of folders and the current folder's messages.
type loadedMsg struct {
	folders  []folderRow
	folderID int64
	msgs     []store.Message
	keep     string // graph ID to keep the cursor on, if still present
	refresh  bool   // a background reload, not the user opening a folder
	err      error
}

func loadData(ctx context.Context, st *store.Store, accountID, folderID int64, keep string) loadedMsg {
	out := loadedMsg{folderID: folderID, keep: keep}
	folders, err := st.Folders(ctx, accountID)
	if err != nil {
		out.err = err
		return out
	}
	_, unread, err := st.LocalFolderCounts(ctx, accountID)
	if err != nil {
		out.err = err
		return out
	}
	out.folders = folderTree(folders, unread)
	if folderID == 0 {
		for _, f := range out.folders {
			if f.WellKnown == "inbox" {
				out.folderID = f.ID
				break
			}
		}
	}
	if out.folderID != 0 {
		out.msgs, out.err = st.MessagesInFolder(ctx, out.folderID, maxListed)
	}
	return out
}

func loadCmd(st *store.Store, accountID, folderID int64, keep string) tea.Cmd {
	return func() tea.Msg {
		l := loadData(context.Background(), st, accountID, folderID, keep)
		l.refresh = true
		return l
	}
}

type bodyState int

const (
	bodyNone bodyState = iota
	bodyLoading
	bodyReady
	bodyFailed
)

// bodyMsg is the result of reading (and maybe fetching) a message body.
type bodyMsg struct {
	seq     int
	id      int64
	body    store.Body
	fetched bool
	err     error
}

func readBodyCmd(st *store.Store, seq int, id int64) tea.Cmd {
	return func() tea.Msg {
		body, fetched, err := st.MessageBody(context.Background(), id)
		return bodyMsg{seq: seq, id: id, body: body, fetched: fetched, err: err}
	}
}

// fetchDelay debounces on-demand body fetches while the cursor is moving.
const fetchDelay = 150 * time.Millisecond

type fetchBodyTick struct{ seq int }

func fetchBodyCmd(cfg *config.Config, st *store.Store, seq int, id int64, graphID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := daemon.Connect(ctx, cfg, true)
		if err != nil {
			return bodyMsg{seq: seq, id: id, err: err}
		}
		defer c.Close()
		if err := c.Call(ctx, ipc.MethodFetchBody, ipc.FetchBodyParams{GraphID: graphID}, nil); err != nil {
			return bodyMsg{seq: seq, id: id, err: err}
		}
		body, fetched, err := st.MessageBody(ctx, id)
		return bodyMsg{seq: seq, id: id, body: body, fetched: fetched, err: err}
	}
}

// Daemon connection messages.
type (
	subscribedMsg struct {
		events <-chan ipc.Event
		status ipc.Status
	}
	daemonEventMsg ipc.Event
	daemonGoneMsg  struct{ err error }
	reconnectMsg   struct{}
	syncDoneMsg    struct{ err error }
)

func subscribeCmd(ctx context.Context, cfg *config.Config) tea.Cmd {
	return func() tea.Msg {
		c, err := daemon.Connect(ctx, cfg, true)
		if err != nil {
			return daemonGoneMsg{err}
		}
		var st ipc.Status
		if err := c.Call(ctx, ipc.MethodStatus, nil, &st); err != nil {
			c.Close()
			return daemonGoneMsg{err}
		}
		events, err := c.Subscribe(ctx)
		if err != nil {
			c.Close()
			return daemonGoneMsg{err}
		}
		return subscribedMsg{events: events, status: st}
	}
}

func waitEventCmd(events <-chan ipc.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-events
		if !ok {
			return daemonGoneMsg{}
		}
		return daemonEventMsg(ev)
	}
}

func syncNowCmd(cfg *config.Config) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		c, err := daemon.Connect(ctx, cfg, true)
		if err != nil {
			return syncDoneMsg{err}
		}
		defer c.Close()
		return syncDoneMsg{c.Call(ctx, ipc.MethodSync, ipc.SyncParams{}, nil)}
	}
}

type mutateDoneMsg struct{ err error }

func mutateCmd(cfg *config.Config, p ipc.MutateParams) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := daemon.Connect(ctx, cfg, true)
		if err != nil {
			return mutateDoneMsg{err}
		}
		defer c.Close()
		return mutateDoneMsg{c.Call(ctx, ipc.MethodMutate, p, nil)}
	}
}

type openDoneMsg struct{ err error }

// openInBrowserCmd has the daemon write the message as a standalone HTML
// file (inline images embedded, remote content blocked) and opens it.
func openInBrowserCmd(cfg *config.Config, graphID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c, err := daemon.Connect(ctx, cfg, true)
		if err != nil {
			return openDoneMsg{err}
		}
		defer c.Close()
		var res ipc.ExportHTMLResult
		if err := c.Call(ctx, ipc.MethodExportHTML, ipc.ExportHTMLParams{GraphID: graphID}, &res); err != nil {
			return openDoneMsg{err}
		}
		return openDoneMsg{xdgOpen(res.Path)}
	}
}

func openURLCmd(url string) tea.Cmd {
	return func() tea.Msg { return openDoneMsg{xdgOpen(url)} }
}

// xdgOpen hands a path or URL to the desktop's default handler. Its output
// is discarded so it can't scribble over the UI.
func xdgOpen(target string) error {
	cmd := exec.Command("xdg-open", target)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap it
	return nil
}

func setSyncCmd(cfg *config.Config, folderID int64, enabled bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := daemon.Connect(ctx, cfg, true)
		if err != nil {
			return mutateDoneMsg{err}
		}
		defer c.Close()
		return mutateDoneMsg{c.Call(ctx, ipc.MethodSetSync, ipc.SetSyncParams{FolderID: folderID, Enabled: enabled}, nil)}
	}
}
