// Package runident holds the shared run-identity helper used at the
// entry point of every `triagefactory exec ...` subcommand to resolve
// the (orgID, userID, runID) triple from the TRIAGE_FACTORY_RUN_ID
// env var the delegate spawner sets.
//
// Lives in its own package (not in cmd/exec) so subcommand packages —
// chain, gh, workspace — can import the helper without forming an
// import cycle through cmd/exec's top-level dispatch.
//
// The pattern matches what internal/delegate/run.go established for
// the spawner-side bookkeeping: branch on the run's trigger_type so
// manual runs route through synthetic-claims (carrying the human's
// identity) and event-triggered runs route through admin-pool
// `...System` methods (no human identity exists). See SKY-302.
//
// SKY-303 will lift this helper behind an AgentHostClient interface
// so the sandboxed-agent path can talk to a host daemon over IPC
// instead of reaching the DB directly. Until then, every subcommand
// resolves identity here and switches its store calls per branch.
package runident

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// RunIdentityEnvVar is the env var name the delegate spawner sets on
// the agent subprocess and `triagefactory exec ...` reads at startup.
// Hardcoded to match internal/delegate/run.go's runAgent — see
// SKY-299 for the spawner-side injection.
const RunIdentityEnvVar = "TRIAGE_FACTORY_RUN_ID"

// ErrRunIdentityMissing is returned by ResolveRunIdentity when the
// TRIAGE_FACTORY_RUN_ID env var is unset. Surfaces as a clear
// "spawner bug" message — an agent invoking these commands without
// the env var present means the spawner failed to inject it.
var ErrRunIdentityMissing = errors.New("TRIAGE_FACTORY_RUN_ID not set; this command must be invoked by the delegated agent spawner")

// ErrRunIdentityNotFound is returned by ResolveRunIdentity when the
// supplied runID doesn't match a row in the agent_runs table. Surfaces
// as a clear "stale env var / spawner bug" message in subcommand
// stderr. Subcommands errors.Is against this sentinel when they want
// to remap to their own package-level "not found" sentinels.
var ErrRunIdentityNotFound = errors.New("TRIAGE_FACTORY_RUN_ID points at a run that does not exist; check spawner injection")

// RunIdentity is the resolved (orgID, userID, runID) triple for a
// cmd/exec subcommand invocation. Returned by ResolveRunIdentity at
// every subcommand's entry point so the body can branch on
// IsEventTriggered to pick its store-routing strategy.
type RunIdentity struct {
	// OrgID is the run's owning org. In local mode this is always
	// runmode.LocalDefaultOrg; the field exists for SKY-269's
	// eventual multi-org switch where it will come from run.OrgID.
	OrgID string

	// UserID is the run's creator_user_id — non-empty for manual
	// runs (the human who pressed delegate / swiped agent), empty
	// for event-triggered runs (no human asked for the work).
	// Manual subcommand callers wrap their writes in
	// SyntheticClaimsWithTx using this value; event-triggered
	// callers route through `...System` admin-pool methods.
	UserID string

	// RunID is TRIAGE_FACTORY_RUN_ID — the run the subprocess is
	// acting on behalf of. Stamped into pending_review.run_id,
	// pending_pr.run_id, run_worktrees.run_id, chain verdicts, etc.
	RunID string

	// IsEventTriggered is true when the run was spawned by an
	// auto-delegation trigger rather than by a human action. The
	// discriminator that picks synthetic-claims vs admin-pool
	// routing in every subcommand. Mirrors the same trigger_type
	// branch internal/delegate/run.go uses for spawner-side
	// bookkeeping.
	IsEventTriggered bool
}

// ResolveRunIdentityFromEnv is the CLI entry-point helper that reads
// TRIAGE_FACTORY_RUN_ID from the process env and delegates to
// ResolveRunIdentity. Subcommands' top-level functions use this; the
// lower-level orchestration body of each subcommand takes the runID
// as a parameter so tests can drive routing without poking at env.
func ResolveRunIdentityFromEnv(ctx context.Context, stores db.Stores) (RunIdentity, error) {
	return ResolveRunIdentity(ctx, stores, os.Getenv(RunIdentityEnvVar))
}

// ResolveRunIdentity looks up the run via the admin pool (we don't
// have user claims yet) and returns the routing-relevant identity
// fields. Empty runID surfaces as ErrRunIdentityMissing — callers
// reading from env should validate up front and not pass "".
//
// The lookup goes through GetSystem because we haven't entered a
// claims-bound tx — we don't know who to claim AS until after the
// lookup tells us run.CreatorUserID. SKY-296's GetSystem on the
// admin pool is the right tool for the cold-start identity probe.
func ResolveRunIdentity(ctx context.Context, stores db.Stores, runID string) (RunIdentity, error) {
	if runID == "" {
		return RunIdentity{}, ErrRunIdentityMissing
	}
	// SKY-269 will replace runmode.LocalDefaultOrg with the run's
	// real org_id once multi-tenant scoping lands. The admin-pool
	// GetSystem read works the same way regardless.
	orgID := runmode.LocalDefaultOrg
	run, err := stores.AgentRuns.GetSystem(ctx, orgID, runID)
	if err != nil {
		return RunIdentity{}, fmt.Errorf("lookup run %s: %w", runID, err)
	}
	if run == nil {
		return RunIdentity{}, fmt.Errorf("%w: %s", ErrRunIdentityNotFound, runID)
	}
	return RunIdentity{
		OrgID:            orgID,
		UserID:           run.CreatorUserID,
		RunID:            runID,
		IsEventTriggered: run.TriggerType == domain.TriggerTypeEvent,
	}, nil
}
