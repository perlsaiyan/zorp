// Package mailsync pulls the mailbox from Graph into the local store using
// delta queries.
package mailsync

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/perlsaiyan/zorp/internal/graph"
	"github.com/perlsaiyan/zorp/internal/store"
)

type Options struct {
	// Folders to sync messages for, by well-known name or display name.
	Folders []string
	// InitialDays limits a from-scratch pass to recent mail; 0 means everything.
	InitialDays int
	// DraftDir keeps the Markdown source of drafts saved from zorp, so they
	// can be edited again as written.
	DraftDir string
}

type Engine struct {
	graph *graph.Client
	store *store.Store
	opts  Options
	log   *slog.Logger
	now   func() time.Time

	// mu guards accountID: body fetches run concurrently with Sync.
	mu            sync.Mutex
	accountID     int64
	foldersLoaded bool // folder list fetched at least once by this Engine (Sync only)
}

func New(g *graph.Client, s *store.Store, opts Options, log *slog.Logger) *Engine {
	return &Engine{graph: g, store: s, opts: opts, log: log, now: time.Now}
}

// AccountID is the local account ID, known after the first successful Sync.
func (e *Engine) AccountID() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.accountID
}

// FolderResult describes what one sync pass did to one folder.
type FolderResult struct {
	FolderID int64  `json:"folder_id"`
	Folder   string `json:"folder"`
	// WellKnown is the folder's well-known name (inbox, …), if any.
	WellKnown string `json:"well_known,omitempty"`
	Initial   bool   `json:"initial"` // from scratch, not incremental
	Upserted  int    `json:"upserted"`
	Removed   int    `json:"removed"`
	Total     int    `json:"total"` // cached messages after the pass
	Unread    int    `json:"unread"`
	// New are messages seen for the first time in an incremental pass
	// (not moved in from another cached folder): candidates for a
	// new-mail notification.
	New []store.Message `json:"-"`
}

type Result struct {
	Folders       int            `json:"folders"` // folders in the mailbox
	FolderResults []FolderResult `json:"folder_results"`
	Duration      time.Duration  `json:"duration"`
}

// Sync runs one pass. refreshFolders re-reads the folder list from Graph,
// which is needed on the first pass and occasionally after; message deltas
// run every pass.
func (e *Engine) Sync(ctx context.Context, refreshFolders bool) (*Result, error) {
	start := e.now()
	accountID, err := e.ensureAccount(ctx)
	if err != nil {
		return nil, err
	}
	if refreshFolders || !e.foldersLoaded {
		if err := e.syncFolders(ctx, accountID); err != nil {
			return nil, fmt.Errorf("syncing folder list: %w", err)
		}
		e.foldersLoaded = true
	}

	folders, err := e.store.Folders(ctx, accountID)
	if err != nil {
		return nil, err
	}
	res := &Result{Folders: len(folders)}
	for _, f := range e.foldersToSync(folders) {
		fr, err := e.syncFolder(ctx, accountID, f)
		if err != nil {
			return nil, fmt.Errorf("syncing %s: %w", f.Name, err)
		}
		res.FolderResults = append(res.FolderResults, *fr)
	}
	if err := e.repairBlank(ctx, accountID); err != nil {
		e.log.Warn("repairing blank messages", "err", err)
	}
	res.Duration = e.now().Sub(start)
	return res, nil
}

// ResetSync deletes cached messages and delta links so the next Sync rebuilds.
func (e *Engine) ResetSync(ctx context.Context) error {
	accountID, err := e.ensureAccount(ctx)
	if err != nil {
		return err
	}
	return e.store.ResetSync(ctx, accountID)
}

// ensureAccount looks up the signed-in user and records the account locally.
func (e *Engine) ensureAccount(ctx context.Context) (int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.accountID != 0 {
		return e.accountID, nil
	}
	me, err := e.graph.Me(ctx)
	if err != nil {
		return 0, err
	}
	upn := me.UserPrincipalName
	if upn == "" {
		upn = me.Mail
	}
	id, err := e.store.UpsertAccount(ctx, upn, me.DisplayName)
	if err != nil {
		return 0, err
	}
	e.accountID = id
	return id, nil
}

func (e *Engine) syncFolders(ctx context.Context, accountID int64) error {
	remote, err := e.graph.MailFolders(ctx)
	if err != nil {
		return err
	}
	wellKnown := map[string]string{} // graph ID -> well-known name
	for _, name := range graph.WellKnownFolders {
		f, err := e.graph.WellKnownFolder(ctx, name)
		if err != nil {
			return err
		}
		if f != nil {
			wellKnown[f.ID] = name
		}
	}
	folders := make([]store.Folder, 0, len(remote))
	for _, f := range remote {
		folders = append(folders, store.Folder{
			GraphID:       f.ID,
			ParentGraphID: f.ParentFolderID,
			Name:          f.DisplayName,
			WellKnown:     wellKnown[f.ID],
			UnreadCount:   f.UnreadItemCount,
			TotalCount:    f.TotalItemCount,
		})
	}
	return e.store.ReplaceFolders(ctx, accountID, folders)
}

// foldersToSync is the configured folders (in config order) plus any the
// user turned on in the TUI, minus any they turned off.
func (e *Engine) foldersToSync(folders []store.Folder) []store.Folder {
	var out []store.Folder
	seen := map[int64]bool{}
	for _, want := range e.opts.Folders {
		f, ok := findFolder(folders, want)
		if !ok {
			e.log.Warn("configured sync folder not found", "folder", want)
			continue
		}
		if !seen[f.ID] && (f.SyncOverride == nil || *f.SyncOverride) {
			out = append(out, f)
		}
		seen[f.ID] = true
	}
	for _, f := range folders {
		if !seen[f.ID] && f.SyncOverride != nil && *f.SyncOverride {
			out = append(out, f)
		}
	}
	return out
}

func findFolder(folders []store.Folder, want string) (store.Folder, bool) {
	for _, f := range folders {
		if f.WellKnown == strings.ToLower(want) {
			return f, true
		}
	}
	for _, f := range folders {
		if strings.EqualFold(f.Name, want) {
			return f, true
		}
	}
	return store.Folder{}, false
}

func (e *Engine) syncFolder(ctx context.Context, accountID int64, f store.Folder) (*FolderResult, error) {
	fr, err := e.deltaPass(ctx, accountID, f)
	if graph.IsSyncStateExpired(err) {
		e.log.Info("delta link expired, resyncing folder", "folder", f.Name)
		if err := e.store.SetDeltaLink(ctx, f.ID, "", time.Time{}); err != nil {
			return nil, err
		}
		f.DeltaLink = ""
		fr, err = e.deltaPass(ctx, accountID, f)
	}
	if err != nil {
		return nil, err
	}
	// The folder list, and with it the server's counts, is only re-read now
	// and then. When mail arrived or changed, refresh this folder's counts
	// now, or anything showing them (the bar widget) lags the new-mail
	// notification by up to a folder refresh. Worth a warning, not the pass.
	if fr.Upserted > 0 || fr.Removed > 0 {
		if unread, total, err := e.graph.FolderCounts(ctx, f.GraphID); err != nil {
			e.log.Warn("refreshing folder counts", "folder", f.Name, "err", err)
		} else if err := e.store.SetFolderCounts(ctx, f.ID, unread, total); err != nil {
			return nil, err
		}
	}
	fr.Total, fr.Unread, err = e.store.FolderCounts(ctx, f.ID)
	return fr, err
}

// deltaPass follows a folder's delta pages to the end and saves the new delta
// link. Each page is applied in its own transaction; if the pass dies midway
// the old link is still stored, and replaying pages is harmless because
// upserts and removals are idempotent.
func (e *Engine) deltaPass(ctx context.Context, accountID int64, f store.Folder) (*FolderResult, error) {
	fr := &FolderResult{FolderID: f.ID, Folder: f.Name, WellKnown: f.WellKnown, Initial: f.DeltaLink == ""}
	var since time.Time
	link := f.DeltaLink
	if fr.Initial {
		if e.opts.InitialDays > 0 {
			since = e.now().AddDate(0, 0, -e.opts.InitialDays)
		}
		link = e.graph.InitialDeltaURL(f.GraphID, since)
	}
	// On a from-scratch pass, remember what we saw so messages that vanished
	// while we had no delta link can be pruned afterwards.
	var seen map[string]bool
	if fr.Initial {
		seen = map[string]bool{}
	}

	for {
		page, err := e.graph.MessagesDelta(ctx, link)
		if err != nil {
			return nil, err
		}
		full, err := e.fullMessages(ctx, page.Partial)
		if err != nil {
			return nil, err
		}
		batch := store.DeltaBatch{Removed: page.Removed}
		for _, m := range append(page.Messages, full...) {
			batch.Upserts = append(batch.Upserts, toStore(m))
			if seen != nil {
				seen[m.ID] = true
			}
		}
		if !fr.Initial && len(page.Messages) > 0 {
			ids := make([]string, 0, len(page.Messages))
			for _, m := range page.Messages {
				ids = append(ids, m.ID)
			}
			known, err := e.store.KnownGraphIDs(ctx, accountID, ids)
			if err != nil {
				return nil, err
			}
			for _, m := range batch.Upserts[:len(page.Messages)] {
				if !known[m.GraphID] {
					fr.New = append(fr.New, m)
				}
			}
		}
		if err := e.store.ApplyDelta(ctx, accountID, f.ID, batch); err != nil {
			return nil, err
		}
		fr.Upserted += len(batch.Upserts)
		fr.Removed += len(batch.Removed)

		if page.DeltaLink != "" {
			if seen != nil {
				n, err := e.store.PruneFolder(ctx, f.ID, since, seen)
				if err != nil {
					return nil, err
				}
				fr.Removed += n
			}
			if err := e.store.SetDeltaLink(ctx, f.ID, page.DeltaLink, e.now()); err != nil {
				return nil, err
			}
			return fr, nil
		}
		link = page.NextLink
	}
}

// fullMessages fetches messages in full by ID, for delta items that came
// with only their changed properties. Messages that are gone are left out
// (their removal comes in a later delta).
func (e *Engine) fullMessages(ctx context.Context, ids []string) ([]graph.Message, error) {
	var out []graph.Message
	for len(ids) > 0 {
		n := min(len(ids), graph.MaxBatch)
		got, err := e.graph.Messages(ctx, ids[:n])
		if err != nil {
			return nil, err
		}
		for _, id := range ids[:n] {
			if m, ok := got[id]; ok {
				out = append(out, m)
			}
		}
		ids = ids[n:]
	}
	return out, nil
}

// repairBlank re-fetches cached messages whose summary was wiped by a
// partial delta item before zorp recognized them.
func (e *Engine) repairBlank(ctx context.Context, accountID int64) error {
	blank, err := e.store.BlankMessages(ctx, accountID)
	if err != nil || len(blank) == 0 {
		return err
	}
	byFolder := map[int64][]string{}
	for _, b := range blank {
		byFolder[b.FolderID] = append(byFolder[b.FolderID], b.GraphID)
	}
	for folderID, ids := range byFolder {
		full, err := e.fullMessages(ctx, ids)
		if err != nil {
			return err
		}
		batch := store.DeltaBatch{}
		for _, m := range full {
			batch.Upserts = append(batch.Upserts, toStore(m))
		}
		if err := e.store.ApplyDelta(ctx, accountID, folderID, batch); err != nil {
			return err
		}
		e.log.Info("repaired blank messages", "folder_id", folderID, "repaired", len(full), "of", len(ids))
	}
	return nil
}

func toStore(m graph.Message) store.Message {
	out := store.Message{
		GraphID:           m.ID,
		ConversationID:    m.ConversationID,
		InternetMessageID: m.InternetMessageID,
		Subject:           m.Subject,
		To:                addrs(m.ToRecipients),
		Cc:                addrs(m.CcRecipients),
		ReplyTo:           addrs(m.ReplyTo),
		ReceivedAt:        m.ReceivedDateTime,
		IsRead:            m.IsRead,
		HasAttachments:    m.HasAttachments,
		Importance:        m.Importance,
		Flagged:           m.Flag != nil && m.Flag.FlagStatus == "flagged",
		Categories:        m.Categories,
		BodyPreview:       m.BodyPreview,
	}
	if m.From != nil {
		out.From = store.Address{Name: m.From.EmailAddress.Name, Addr: m.From.EmailAddress.Address}
	}
	if out.Importance == "" {
		out.Importance = "normal"
	}
	return out
}

func addrs(rs []graph.Recipient) []store.Address {
	out := make([]store.Address, 0, len(rs))
	for _, r := range rs {
		out = append(out, store.Address{Name: r.EmailAddress.Name, Addr: r.EmailAddress.Address})
	}
	return out
}
