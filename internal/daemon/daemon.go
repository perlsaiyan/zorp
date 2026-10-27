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
	"sync"
	"syscall"
	"time"

	"github.com/tom/zorp/config"
	"github.com/tom/zorp/internal/auth"
	"github.com/tom/zorp/internal/graph"
	"github.com/tom/zorp/internal/ipc"
	"github.com/tom/zorp/internal/mailsync"
	"github.com/tom/zorp/internal/store"
)

// folderRefreshEvery is how often the folder list is re-read from Graph.
// Message deltas run every sync interval regardless.
const folderRefreshEvery = 15 * time.Minute

var ErrAlreadyRunning = errors.New("daemon is already running")

type daemon struct {
	cfg    *config.Config
	engine *mailsync.Engine
	server *ipc.Server
	log    *slog.Logger
	cancel context.CancelFunc

	requests chan syncRequest
	kick     chan struct{} // run a sync soon, without waiting for a reply

	mu                sync.Mutex
	status            ipc.Status
	lastFolderRefresh time.Time
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
		cfg: cfg,
		engine: mailsync.New(graph.New(a), st, mailsync.Options{
			Folders:     cfg.Sync.Folders,
			InitialDays: cfg.Sync.InitialDays,
		}, log),
		log:      log,
		cancel:   cancel,
		requests: make(chan syncRequest),
		kick:     make(chan struct{}, 1),
		status:   ipc.Status{State: ipc.StateStarting, PID: os.Getpid()},
	}
	d.server = ipc.NewServer(d.handle, log)

	log.Info("daemon started", "pid", os.Getpid(), "socket", cfg.Paths.Socket, "db", cfg.Paths.DB)
	go d.loop(ctx)
	err = d.server.Serve(ctx, ln)
	log.Info("daemon stopped")
	return err
}

func lockInstance(path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}

func (d *daemon) loop(ctx context.Context) {
	timer := time.NewTimer(0) // first sync immediately
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			d.syncIfSignedIn(ctx)
		case <-d.kick:
			d.syncIfSignedIn(ctx)
		case req := <-d.requests:
			res, err := d.runSync(ctx, req.full)
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
	if _, err := d.runSync(ctx, false); err == nil {
		d.prefetchBodies(ctx)
	}
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

func (d *daemon) runSync(ctx context.Context, full bool) (*mailsync.Result, error) {
	d.setState(ipc.StateSyncing, "")

	refresh := full
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
		state := ipc.StateIdle
		switch {
		case errors.Is(err, auth.ErrNeedsLogin):
			state = ipc.StateNeedsLogin
		case isNetworkError(err):
			state = ipc.StateOffline
		}
		d.log.Warn("sync failed", "state", state, "err", err)
		d.setState(state, err.Error())
		return nil, err
	}

	d.mu.Lock()
	if refresh {
		d.lastFolderRefresh = time.Now()
	}
	d.status.State = ipc.StateIdle
	d.status.LastSync = time.Now().Unix()
	d.status.LastError = ""
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
	return res, nil
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
