// Package daemon runs zorp's background process: the only writer to the
// local store and the only process that talks to Graph for sync.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/auth"
	"github.com/perlsaiyan/zorp/internal/graph"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/mailsync"
	"github.com/perlsaiyan/zorp/internal/store"
)

// folderRefreshEvery is how often the folder list is re-read from Graph.
// Message deltas run every sync interval regardless.
const folderRefreshEvery = 15 * time.Minute

var ErrAlreadyRunning = errors.New("daemon is already running")

type daemon struct {
	cfg    *config.Config
	store  *store.Store
	engine *mailsync.Engine
	cal    *graph.Client // calendar calls, with a token carrying calendar access
	dir    *graph.Client // directory lookups (auth.DirectoryScopes)
	server *ipc.Server
	log    *slog.Logger
	cancel context.CancelFunc

	requests  chan syncRequest
	kick      chan struct{} // run a sync soon, without waiting for a reply
	flushKick chan struct{} // send queued changes to Graph soon

	mu                 sync.Mutex
	status             ipc.Status
	lastFolderRefresh  time.Time
	offlineStreak      int       // syncs in a row that failed with a network error
	calendarErr        string    // the last calendar sync error logged, to log each once
	desktopFailedUntil time.Time // desktop notifications failed; use the terminal until then
}

type syncRequest struct {
	full  bool
	reply chan syncReply
}

type syncReply struct {
	result *mailsync.Result
	err    error
}

// Run runs the daemon until ctx is cancelled or a client asks it to shut down.
func Run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	// Everything the daemon creates (DB, WAL, socket, logs) is owner-only.
	syscall.Umask(0o077)
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}
	unlock, err := lockInstance(filepath.Join(cfg.Paths.DataDir, "daemon.lock"))
	if err != nil {
		return err
	}
	defer unlock()

	a, err := auth.New(cfg)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.Paths.DB)
	if err != nil {
		return err
	}
	defer st.Close()

	// We hold the instance lock, so any existing socket file is stale.
	if err := os.Remove(cfg.Paths.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", cfg.Paths.Socket)
	if err != nil {
		return err
	}
	defer os.Remove(cfg.Paths.Socket)
	if err := os.Chmod(cfg.Paths.Socket, 0o600); err != nil {
		ln.Close()
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	d := &daemon{
		cfg:   cfg,
		store: st,
		engine: mailsync.New(graph.New(a), st, mailsync.Options{
			Folders:     cfg.Sync.Folders,
			InitialDays: cfg.Sync.InitialDays,
			DraftDir:    filepath.Join(cfg.Paths.DataDir, "drafts"),
		}, log),
		cal:       graph.New(a.Calendar()),
		dir:       graph.New(a.Directory()),
		log:       log,
		cancel:    cancel,
		requests:  make(chan syncRequest),
		kick:      make(chan struct{}, 1),
		flushKick: make(chan struct{}, 1),
		status:    ipc.Status{State: ipc.StateStarting, PID: os.Getpid(), Guards: cfg.Safety.Guards()},
	}
	d.server = ipc.NewServer(d.handle, log)

	log.Info("daemon started", "pid", os.Getpid(), "socket", cfg.Paths.Socket, "db", cfg.Paths.DB)
	go d.loop(ctx)
	go d.remindLoop(ctx)
	err = d.server.Serve(ctx, ln)
	log.Info("daemon stopped")
	return err
}

// lockWait is how long a starting daemon waits for a previous one that is
// still shutting down (closing the database can take a moment).
const lockWait = 3 * time.Second

func lockInstance(path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(lockWait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, ErrAlreadyRunning
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// loop runs syncs and outbox flushes one at a time. Keeping them serial
// matters: if a flush completed while a sync had already fetched (but not yet
// applied) a delta page, the stale page would briefly undo the change with
// no queued op left to rebase on. Flushing before each sync also means local
// changes reach Graph before we pull.
func (d *daemon) loop(ctx context.Context) {
	timer := time.NewTimer(0) // first sync immediately
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.flushKick:
			d.flush(ctx)
			continue // don't reset the sync timer
		case <-timer.C:
			d.flush(ctx)
			d.syncIfSignedIn(ctx)
		case <-d.kick:
			d.flush(ctx)
			d.syncIfSignedIn(ctx)
		case req := <-d.requests:
			d.flush(ctx)
			res, err := d.runSync(ctx, req.full, true)
			req.reply <- syncReply{res, err}
			if err == nil {
				d.prefetchBodies(ctx)
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(d.cfg.Sync.Interval)
	}
}

// syncIfSignedIn is the background path: while the user needs to log in
// there's no point hitting the network every interval.
func (d *daemon) syncIfSignedIn(ctx context.Context) {
	if d.getStatus().State == ipc.StateNeedsLogin {
		return
	}
	if _, err := d.runSync(ctx, false, false); err == nil {
		d.prefetchBodies(ctx)
	}
}

// flush sends queued changes to Graph, reporting rejected (and reverted)
// ones to clients.
func (d *daemon) flush(ctx context.Context) {
	if d.getStatus().State == ipc.StateNeedsLogin {
		return
	}
	res, err := d.engine.FlushOutbox(ctx)
	if len(res.Changed) > 0 {
		d.server.Broadcast(ipc.EventChanged, ipc.Changed{FolderIDs: res.Changed})
	}
	for _, r := range res.Reverted {
		d.server.Broadcast(ipc.EventError, ipc.ErrorEvent{
			Message: fmt.Sprintf("Couldn't %s a message, undone: %v", describeOp(r.Op), r.Err),
		})
	}
	if res.Sent > 0 || len(res.Reverted) > 0 {
		d.log.Info("flushed outbox", "sent", res.Sent, "reverted", len(res.Reverted))
	}
	pending, perr := d.store.PendingCount(ctx)
	if perr != nil {
		d.log.Error("counting outbox", "err", perr)
	}
	d.mu.Lock()
	d.status.Pending = pending
	d.mu.Unlock()
	if err != nil {
		d.log.Warn("flushing outbox", "pending", pending, "err", err)
		d.setState(classifyError(err), err.Error())
		return
	}
	d.server.Broadcast(ipc.EventStatus, d.getStatus())
}

func describeOp(op store.Op) string {
	switch op.Kind {
	case store.OpSetRead:
		if op.Value {
			return "mark as read"
		}
		return "mark as unread"
	case store.OpSetFlag:
		if op.Value {
			return "flag"
		}
		return "unflag"
	case store.OpMove:
		return "move"
	}
	return string(op.Kind)
}

func classifyError(err error) string {
	switch {
	case errors.Is(err, auth.ErrNeedsLogin):
		return ipc.StateNeedsLogin
	case isNetworkError(err):
		return ipc.StateOffline
	}
	return ipc.StateIdle
}

// prefetchBodies downloads bodies for recent messages after a successful
// sync. Failures are logged and retried after the next sync.
func (d *daemon) prefetchBodies(ctx context.Context) {
	start := time.Now()
	n, err := d.engine.PrefetchBodies(ctx, d.cfg.Sync.PrefetchBodies)
	if err != nil {
		d.log.Warn("prefetching bodies", "stored", n, "err", err)
		return
	}
	if n > 0 {
		d.log.Info("prefetched bodies", "stored", n, "took", time.Since(start).Round(time.Millisecond))
	}
}

// runSync runs one sync pass. refresh re-reads the folder list; it also
// happens on full syncs and every folderRefreshEvery.
func (d *daemon) runSync(ctx context.Context, full, refresh bool) (*mailsync.Result, error) {
	d.setState(ipc.StateSyncing, "")

	refresh = refresh || full
	d.mu.Lock()
	if time.Since(d.lastFolderRefresh) > folderRefreshEvery {
		refresh = true
	}
	d.mu.Unlock()

	var res *mailsync.Result
	err := func() error {
		if full {
			if err := d.engine.ResetSync(ctx); err != nil {
				return err
			}
		}
		var err error
		res, err = d.engine.Sync(ctx, refresh)
		return err
	}()

	if err != nil {
		state := classifyError(err)
		d.log.Warn("sync failed", "state", state, "err", err)
		d.mu.Lock()
		if state == ipc.StateOffline {
			d.offlineStreak++
		} else {
			d.offlineStreak = 0
		}
		streak := d.offlineStreak
		d.mu.Unlock()
		d.setState(state, shownError(state, streak, err))
		return nil, err
	}

	if refresh {
		if err := d.engine.PruneSearchHits(ctx); err != nil {
			d.log.Warn("pruning old search results", "err", err)
		}
		mailsync.PruneImageCache(d.imageDir(), time.Now())
		mailsync.PruneImageCache(d.openDir(), time.Now())
	}

	d.mu.Lock()
	if refresh {
		d.lastFolderRefresh = time.Now()
	}
	d.status.State = ipc.StateIdle
	d.status.LastSync = time.Now().Unix()
	d.status.LastError = ""
	d.offlineStreak = 0
	st := d.status
	d.mu.Unlock()
	d.server.Broadcast(ipc.EventStatus, st)

	var changed ipc.Changed
	for _, fr := range res.FolderResults {
		if fr.Upserted > 0 || fr.Removed > 0 {
			changed.FolderIDs = append(changed.FolderIDs, fr.FolderID)
		}
		d.log.Info("synced folder", "folder", fr.Folder, "initial", fr.Initial,
			"upserted", fr.Upserted, "removed", fr.Removed, "total", fr.Total)
	}
	if len(changed.FolderIDs) > 0 || refresh {
		d.server.Broadcast(ipc.EventChanged, changed)
	}
	d.notifyNew(ctx, res)
	d.syncCalendar(ctx)
	return res, nil
}

// syncCalendar runs a calendar pass after a mail sync. Its failures are
// logged (each once) and never affect mail.
func (d *daemon) syncCalendar(ctx context.Context) {
	if !d.cfg.Calendar.Enabled {
		return
	}
	res, err := d.engine.SyncCalendar(ctx, d.cal)
	d.mu.Lock()
	logIt := err != nil && err.Error() != d.calendarErr
	if err != nil {
		d.calendarErr = err.Error()
	} else {
		d.calendarErr = ""
	}
	d.mu.Unlock()
	if logIt {
		d.log.Warn("calendar sync failed", "err", err)
	}
	if err != nil {
		return
	}
	if res.Initial || res.Upserted > 0 || res.Removed > 0 {
		d.log.Info("synced calendar", "initial", res.Initial, "upserted", res.Upserted, "removed", res.Removed)
		d.server.Broadcast(ipc.EventCalendar, struct{}{})
	}
}

// offlineGrace is how many network failures in a row pass with only the
// "offline" state before the error itself is reported: a request to Graph
// that stalls now and then shouldn't put an error on screen.
const offlineGrace = 1

// shownError is the error a failed sync reports in the status.
func shownError(state string, offlineStreak int, err error) string {
	if state == ipc.StateOffline && offlineStreak <= offlineGrace {
		return ""
	}
	return err.Error()
}

func isNetworkError(err error) bool {
	var ne net.Error
	return errors.As(err, &ne)
}

func (d *daemon) getStatus() ipc.Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status
}

func (d *daemon) setState(state, lastErr string) {
	d.mu.Lock()
	d.status.State = state
	if lastErr != "" || state == ipc.StateSyncing {
		d.status.LastError = lastErr
	}
	st := d.status
	d.mu.Unlock()
	d.server.Broadcast(ipc.EventStatus, st)
}

func (d *daemon) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case ipc.MethodStatus:
		return d.getStatus(), nil

	case ipc.MethodSync:
		var p ipc.SyncParams
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, err
			}
		}
		reply := make(chan syncReply, 1)
		select {
		case d.requests <- syncRequest{full: p.Full, reply: reply}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		select {
		case r := <-reply:
			return r.result, r.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}

	case ipc.MethodFetchBody:
		var p ipc.FetchBodyParams
		if err := json.Unmarshal(params, &p); err != nil || p.GraphID == "" {
			return nil, errors.New("fetch-body needs a graph_id")
		}
		// Runs concurrently with the sync loop; the store serializes writes.
		return true, d.engine.FetchBody(ctx, p.GraphID)

	case ipc.MethodMutate:
		var p ipc.MutateParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return d.mutate(ctx, p)

	case ipc.MethodExportHTML:
		var p ipc.ExportHTMLParams
		if err := json.Unmarshal(params, &p); err != nil || p.GraphID == "" {
			return nil, errors.New("export-html needs a graph_id")
		}
		path, err := d.engine.ExportHTML(ctx, p.GraphID, d.cfg.Paths.ViewDir)
		return ipc.ExportHTMLResult{Path: path}, err

	case ipc.MethodSetSync:
		var p ipc.SetSyncParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := d.store.SetFolderSync(ctx, p.FolderID, p.Enabled); err != nil {
			return nil, err
		}
		d.server.Broadcast(ipc.EventChanged, ipc.Changed{FolderIDs: []int64{p.FolderID}})
		select {
		case d.kick <- struct{}{}: // start the folder's first sync now
		default:
		}
		return true, nil

	case ipc.MethodSearch:
		var p ipc.SearchParams
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.KQL) == "" {
			return nil, errors.New("search needs a query")
		}
		limit := p.Limit
		if limit <= 0 {
			limit = 100
		}
		// Runs concurrently with the sync loop, like fetch-body.
		ids, err := d.engine.SearchServer(ctx, p.KQL, min(limit, 250))
		return ipc.SearchResult{GraphIDs: ids}, err

	case ipc.MethodCompose:
		return d.compose(ctx, params)
	case ipc.MethodGetDraft:
		return d.getDraft(ctx, params)
	case ipc.MethodAttachList:
		return d.listAttachments(ctx, params)
	case ipc.MethodNotifyTest:
		return d.notifyTest(), nil
	case ipc.MethodMeeting:
		return d.meetingInfo(ctx, params)
	case ipc.MethodRespond:
		return d.respond(ctx, params)
	case ipc.MethodEventGet:
		return d.eventGet(ctx, params)
	case ipc.MethodEventSave:
		return d.eventSave(ctx, params)
	case ipc.MethodEventCancel:
		return d.eventCancel(ctx, params)
	case ipc.MethodFreeBusy:
		return d.freeBusy(ctx, params)
	case ipc.MethodPeople:
		return d.people(ctx, params)
	case ipc.MethodOpenAttach:
		return d.openAttachment(ctx, params)
	case ipc.MethodSaveAttach:
		return d.saveAttachment(ctx, params)
	case ipc.MethodImages:
		return d.images(ctx, params)

	case ipc.MethodReloadAuth:
		// MSAL re-reads the token cache file before every token request, so
		// there's nothing to reload; just leave needs_login and sync soon.
		if d.getStatus().State == ipc.StateNeedsLogin {
			d.setState(ipc.StateIdle, "")
		}
		select {
		case d.kick <- struct{}{}:
		default:
		}
		return d.getStatus(), nil

	case ipc.MethodShutdown:
		d.log.Info("shutdown requested")
		// Give the server a moment to send this reply before connections close.
		time.AfterFunc(100*time.Millisecond, d.cancel)
		return true, nil
	}
	return nil, fmt.Errorf("unknown method %q", method)
}

// mutate applies a user's change to the cache right away and queues it for
// Graph. Safe mode is enforced here so it covers every client.
func (d *daemon) mutate(ctx context.Context, p ipc.MutateParams) (ipc.MutateResult, error) {
	accountID, err := d.engine.Account(ctx)
	if err != nil {
		return ipc.MutateResult{}, err
	}
	var m store.Mutation
	switch p.Op {
	case ipc.OpRead, ipc.OpUnread:
		m = store.Mutation{Kind: store.OpSetRead, Value: p.Op == ipc.OpRead}
	case ipc.OpFlag, ipc.OpUnflag:
		m = store.Mutation{Kind: store.OpSetFlag, Value: p.Op == ipc.OpFlag}
	case ipc.OpMove, ipc.OpArchive, ipc.OpDelete:
		var dest store.Folder
		switch p.Op {
		case ipc.OpMove:
			dest, err = d.store.Folder(ctx, p.FolderID)
		case ipc.OpArchive:
			dest, err = d.store.FolderByWellKnown(ctx, accountID, "archive")
		case ipc.OpDelete:
			if !d.cfg.Safety.DeleteAllowed() {
				return ipc.MutateResult{}, errors.New("guardrail: deleting is off (set safety.allow_delete = true to allow it)")
			}
			dest, err = d.store.FolderByWellKnown(ctx, accountID, "deleteditems")
		}
		if errors.Is(err, store.ErrNotFound) {
			return ipc.MutateResult{}, fmt.Errorf("%s: destination folder not found", p.Op)
		}
		if err != nil {
			return ipc.MutateResult{}, err
		}
		if !d.cfg.Safety.MayMoveTo(dest.Name) {
			return ipc.MutateResult{}, fmt.Errorf("guardrail: moves are only allowed to %q, not %q (set safety.allow_move = true to allow any)", d.cfg.Safety.ScratchFolder, dest.Name)
		}
		m = store.Mutation{Kind: store.OpMove, Dest: dest}
	default:
		return ipc.MutateResult{}, fmt.Errorf("unknown op %q", p.Op)
	}

	var res ipc.MutateResult
	var changed []int64
	for _, gid := range p.GraphIDs {
		c, err := d.store.Enqueue(ctx, accountID, gid, m)
		if errors.Is(err, store.ErrNotFound) {
			continue // gone since the client listed it
		}
		if err != nil {
			return res, err
		}
		if len(c) > 0 {
			res.Queued++
			changed = append(changed, c...)
		}
	}
	if res.Queued > 0 {
		// Update the pending count before replying, so a client that checks
		// status right after a change sees it as unsent until it's flushed.
		if n, err := d.store.PendingCount(ctx); err == nil {
			d.mu.Lock()
			d.status.Pending = n
			d.mu.Unlock()
		}
		d.server.Broadcast(ipc.EventStatus, d.getStatus())
		d.server.Broadcast(ipc.EventChanged, ipc.Changed{FolderIDs: changed})
		select {
		case d.flushKick <- struct{}{}:
		default:
		}
	}
	return res, nil
}
