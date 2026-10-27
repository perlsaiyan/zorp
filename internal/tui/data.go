package tui

import (
	"context"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/tom/zorp/config"
	"github.com/tom/zorp/internal/daemon"
	"github.com/tom/zorp/internal/ipc"
	"github.com/tom/zorp/internal/store"
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
		return loadData(context.Background(), st, accountID, folderID, keep)
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
	text    string
	fetched bool
	err     error
}

func readBodyCmd(st *store.Store, seq int, id int64) tea.Cmd {
	return func() tea.Msg {
		text, fetched, err := st.MessageBody(context.Background(), id)
		return bodyMsg{seq: seq, id: id, text: text, fetched: fetched, err: err}
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
		text, fetched, err := st.MessageBody(ctx, id)
		return bodyMsg{seq: seq, id: id, text: text, fetched: fetched, err: err}
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
