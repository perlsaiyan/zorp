package mailsync

import (
	"context"
	"errors"
	"fmt"

	"github.com/perlsaiyan/zorp/internal/graph"
	"github.com/perlsaiyan/zorp/internal/store"
)

// maxOpAttempts bounds retries of an op that keeps failing transiently, so
// one bad op can't block the queue forever.
const maxOpAttempts = 20

// Reverted is an op Graph rejected; its local effect has been undone.
type Reverted struct {
	Op  store.Op
	Err error
}

type FlushResult struct {
	Sent     int
	Reverted []Reverted
	Changed  []int64 // folders whose cached contents changed (from reverts)
}

// Account returns the local account ID, looking the user up if needed.
func (e *Engine) Account(ctx context.Context) (int64, error) {
	return e.ensureAccount(ctx)
}

// FlushOutbox sends queued ops to Graph, oldest first. Ops Graph rejects are
// reverted locally and reported. On a transient failure (network, throttling,
// expired login) it stops and returns the error with the op still queued, so
// ops for a message are always sent in order.
func (e *Engine) FlushOutbox(ctx context.Context) (FlushResult, error) {
	var res FlushResult
	for {
		ops, err := e.store.PendingOps(ctx, 50)
		if err != nil || len(ops) == 0 {
			return res, err
		}
		for _, op := range ops {
			err := e.sendOp(ctx, op)
			switch {
			case err == nil:
				res.Sent++
			case graph.IsPermanent(err) || op.Attempts+1 >= maxOpAttempts:
				changed, rerr := e.store.RevertOp(ctx, op)
				if rerr != nil {
					return res, rerr
				}
				res.Changed = append(res.Changed, changed...)
				res.Reverted = append(res.Reverted, Reverted{Op: op, Err: err})
				e.log.Warn("op rejected, reverted", "op", op.Kind, "message", op.GraphID, "err", err)
			default:
				if rerr := e.store.RetryOp(ctx, op, err.Error()); rerr != nil {
					return res, rerr
				}
				return res, err
			}
		}
	}
}

func (e *Engine) sendOp(ctx context.Context, op store.Op) error {
	switch op.Kind {
	case store.OpSetRead:
		if err := e.graph.SetRead(ctx, op.GraphID, op.Value); err != nil {
			return err
		}
		return e.store.CompleteOp(ctx, op, false)

	case store.OpSetFlag:
		if err := e.graph.SetFlagged(ctx, op.GraphID, op.Value); err != nil {
			return err
		}
		return e.store.CompleteOp(ctx, op, false)

	case store.OpMove:
		newID, err := e.graph.Move(ctx, op.GraphID, op.DestGraphID)
		if err != nil {
			return err
		}
		if newID != "" && newID != op.GraphID {
			// Shouldn't happen with immutable IDs; the next sync of the
			// destination will pick the message up under its new ID.
			e.log.Warn("move changed message ID despite ImmutableId", "old", op.GraphID, "new", newID)
		}
		// A folder that isn't synced will never send a delta for this
		// message, so drop the cached copy rather than leave it stale.
		dest, err := e.store.Folder(ctx, op.DestFolderID)
		unsynced := errors.Is(err, store.ErrNotFound) || (err == nil && dest.DeltaLink == "")
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return e.store.CompleteOp(ctx, op, unsynced || newID != op.GraphID)
	}
	return fmt.Errorf("%w: unknown op %q", errUnknownOp, op.Kind)
}

// errUnknownOp is permanent in effect: retrying can't help. It is wrapped as
// a graph.Error so FlushOutbox reverts the op instead of retrying it.
var errUnknownOp = &graph.Error{Status: 400, Code: "zorpUnknownOp", Message: "unknown op"}
