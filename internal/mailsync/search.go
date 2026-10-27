package mailsync

import (
	"context"
	"time"

	"github.com/perlsaiyan/zorp/internal/store"
)

// SearchHitTTL is how long a message found only by a server search stays
// cached after the last search that found it.
const SearchHitTTL = 7 * 24 * time.Hour

// SearchServer runs a KQL search on Graph, caches the results, and returns
// their graph IDs. Messages in folders zorp doesn't know (hidden system
// folders) are skipped.
func (e *Engine) SearchServer(ctx context.Context, kql string, limit int) ([]string, error) {
	accountID, err := e.ensureAccount(ctx)
	if err != nil {
		return nil, err
	}
	found, err := e.graph.SearchMessages(ctx, kql, limit)
	if err != nil {
		return nil, err
	}
	folders, err := e.store.Folders(ctx, accountID)
	if err != nil {
		return nil, err
	}
	byGraphID := make(map[string]int64, len(folders))
	for _, f := range folders {
		byGraphID[f.GraphID] = f.ID
	}
	hits := make([]store.SearchHit, 0, len(found))
	ids := make([]string, 0, len(found))
	for _, m := range found {
		fid, ok := byGraphID[m.ParentFolderID]
		if !ok {
			continue
		}
		hits = append(hits, store.SearchHit{FolderID: fid, Message: toStore(m)})
		ids = append(ids, m.ID)
	}
	if err := e.store.SaveSearchHits(ctx, accountID, hits, e.now()); err != nil {
		return nil, err
	}
	return ids, nil
}

// PruneSearchHits drops search results not seen for SearchHitTTL.
func (e *Engine) PruneSearchHits(ctx context.Context) error {
	accountID, err := e.ensureAccount(ctx)
	if err != nil {
		return err
	}
	_, err = e.store.PruneSearchHits(ctx, accountID, e.now().Add(-SearchHitTTL))
	return err
}
