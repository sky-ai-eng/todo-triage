// writeRouter centralizes the manual-vs-event routing decision once
// per goroutine entry, so the long stretches of bookkeeping in
// runAgent / runChain / Resume / Cancel / Takeover don't repeat the
// branch at every call site.
//
// Routing model (per SKY-299):
//
//   - Manual run (user clicked Delegate / Resume / Cancel / Takeover):
//     write batches run inside SyntheticClaimsWithTx so the RLS-active
//     tf_app pool sees the user identity. CreatorUserID is the
//     synthetic-claims subject.
//
//   - Event-triggered run (router auto-fired from a matched event
//     handler): there is no user; write batches go through the
//     admin-pool `...System` methods on each store. No tx wrap.
//
// The router exposes one entrypoint, `manualBatch`, that returns
// (ranTx, error). When the run is manual it runs fn inside the
// synthetic-claims tx and returns (true, fn-err). When the run is
// event-triggered it returns (false, nil) — the caller falls through
// to its sequential `...System` block. This keeps both arms readable
// and the routing decision visible at each call site.

package delegate

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// writeRouter is goroutine-scoped. Constructed once per Delegate /
// Resume / Cancel / Takeover entry and threaded through that
// goroutine's call stack. Carries the synthetic-claims subject for
// manual runs and nothing else for event runs.
type writeRouter struct {
	tx       db.TxRunner
	orgID    string
	userID   string // synthetic-claims subject; empty for event-triggered runs
	isManual bool
}

// newWriteRouter builds the per-goroutine router from the run's
// trigger type and creator. creatorUserID must be set for manual
// runs; passing empty is treated as a programming error at call
// time (manualBatch fails when fn dereferences a tx that no claim
// was set for, which in practice will surface in the synthetic-
// claims body that the user-ID is empty). Event-triggered runs
// pass empty creatorUserID by design.
func newWriteRouter(s *Spawner, triggerType, creatorUserID string) *writeRouter {
	return &writeRouter{
		tx:       s.tx,
		orgID:    runmode.LocalDefaultOrg,
		userID:   creatorUserID,
		isManual: triggerType == "manual",
	}
}

// manualBatch runs fn inside a SyntheticClaimsWithTx tx for manual
// runs. For event-triggered runs it returns (false, nil) — the caller
// is expected to fall through to its admin-pool `...System` calls.
//
// Idiomatic shape at call sites:
//
//	if ok, err := router.manualBatch(ctx, func(ts db.TxStores) error {
//	    // manual: tx-bound writes, run under the user's RLS claims
//	    if err := ts.AgentRuns.Complete(ctx, ...); err != nil { return err }
//	    if err := ts.Tasks.SetStatus(ctx, ...); err != nil { return err }
//	    return nil
//	}); ok {
//	    return err
//	}
//	// event: sequential admin-pool System calls
//	if err := s.agentRuns.CompleteSystem(ctx, ...); err != nil { return err }
//	if err := s.tasks.SetStatusSystem(ctx, ...); err != nil { return err }
//
// The asymmetry is intentional. Trying to make both arms call the
// same TxStores-shaped facade would require admin-pool adapter
// structs for every store; the volume is high and the boilerplate
// has no value beyond cosmetic uniformity.
func (r *writeRouter) manualBatch(ctx context.Context, fn func(ts db.TxStores) error) (bool, error) {
	if !r.isManual {
		return false, nil
	}
	return true, r.tx.SyntheticClaimsWithTx(ctx, r.orgID, r.userID, fn)
}
