package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ChainStore owns the four tables that back prompt chaining:
//
//   - prompt_chain_steps — ordered membership list for a chain prompt.
//   - chain_runs         — one row per Delegate(chainPrompt, …) instance,
//     owning the worktree shared across every step.
//   - runs               — read-only here (per-step state lives on runs;
//     RunsForChain returns the slice of step rows linked to a chain_run).
//   - run_artifacts      — write-only here, kind='chain:verdict'. The
//     verdict pipeline is the only writer today; InsertVerdict hard-codes
//     the kind so callers can't accidentally write other artifact kinds
//     through this store.
//
// Audiences:
//
//   - HTTP handlers (server/chains_handler.go, server/prompts_handler.go,
//     server/pending_prs_handler.go, server/reviews_handler.go) — read-mostly.
//   - Delegate spawner (delegate/chain.go, delegate/run.go) — every method.
//   - exec subcommand (cmd/exec/chain/chain.go) — verdict write + step→chain
//     lookup, running as a child of the spawned agent.
//
// # Postgres / RLS shape
//
// All chain tables are org-scoped (composite FK against (id, org_id) on
// prompts / tasks / event_handlers) and chain_runs is creator-scoped
// (RLS predicate gates rows on creator_user_id = tf.current_user_id()).
// The Postgres impl threads org_id through every WHERE for defense in
// depth and lets RLS enforce the creator predicate; SQLite collapses
// orgID to runmode.LocalDefaultOrg via assertLocalOrg.
//
// No admin/app pool split here: every method runs on the app pool in
// Postgres. There's no boot-time seed (chain rows are user-created), so
// the claims-less write path that PromptStore / TaskRuleStore /
// TriggerStore need doesn't apply.
type ChainStore interface {
	// ListSteps returns the ordered step list for a chain prompt.
	// Empty slice (not error) when the prompt has no steps configured
	// — the orchestrator treats that as a misconfigured chain and aborts.
	ListSteps(ctx context.Context, orgID string, chainPromptID string) ([]domain.ChainStep, error)

	// CountStepReferences returns the number of distinct chain prompts
	// that reference the given prompt as a step. Used by the prompt-delete
	// handler to surface "used by N chain(s)" instead of letting the FK
	// RESTRICT raise a generic constraint error.
	CountStepReferences(ctx context.Context, orgID string, stepPromptID string) (int, error)

	// ReplaceSteps replaces the entire step list for a chain prompt in
	// a single transaction. step_index is densely packed 0..N-1 by the
	// writer; briefs are taken positionally and may be empty (len==0
	// is allowed — every step gets brief="" then). Upstream validation
	// (rejecting nested chains, missing prompt IDs, …) is the caller's
	// job; this method will fail-fast on FK violations.
	ReplaceSteps(ctx context.Context, orgID string, chainPromptID string, stepPromptIDs []string, briefs []string) error

	// CreateRun inserts a new chain instance row. The caller supplies
	// the worktree path produced by setupGitHub/setupJira (the chain
	// owns the worktree across all steps). Returns the generated id
	// (or the caller-supplied id if non-empty). TriggerType is required;
	// empty returns an error rather than silently defaulting.
	CreateRun(ctx context.Context, orgID string, cr domain.ChainRun) (string, error)

	// GetRun returns a chain run by id, or (nil, nil) when not found.
	GetRun(ctx context.Context, orgID string, id string) (*domain.ChainRun, error)

	// GetRunForRun returns the chain run that owns a step run, plus the
	// step index. Returns (nil, nil, nil) when the supplied run is not
	// part of a chain (single-run delegation).
	GetRunForRun(ctx context.Context, orgID string, runID string) (*domain.ChainRun, *int, error)

	// MarkRunStatus transitions a chain run to a terminal status and
	// records optional abort metadata. completed_at is set on every
	// transition out of 'running'.
	//
	// The WHERE clause guards against lost-update races: only transitions
	// from non-terminal statuses are accepted. Returns (true, nil) when
	// the row was updated, (false, nil) when the guard rejected the write
	// (race loss or already terminal), and (false, err) on DB error.
	MarkRunStatus(ctx context.Context, orgID string, id string, status domain.ChainRunStatus, abortReason string, abortedAtStep *int) (changed bool, err error)

	// RunsForChain returns every step run linked to a chain instance,
	// ordered by chain_step_index ASC, started_at ASC. Used by the
	// chain-detail HTTP endpoint to render the per-step timeline in a
	// single fetch.
	RunsForChain(ctx context.Context, orgID string, chainRunID string) ([]domain.AgentRun, error)

	// ActiveStepRunIDs returns the IDs of step runs on a chain that
	// have not reached a terminal state. Used by CancelChain to sweep
	// active step contexts and cancel them. awaiting_input and
	// pending_approval are treated as terminal here — those rows are
	// parked, not actively executing, so cancellation goes through the
	// chain-run row's MarkRunStatus instead.
	ActiveStepRunIDs(ctx context.Context, orgID string, chainRunID string) ([]string, error)

	// InsertVerdict writes a chain:verdict artifact for a step run.
	// metadataJSON is the marshalled domain.ChainVerdict payload.
	//
	// IMPORTANT: implementations format a microsecond-precision UTC
	// timestamp rather than relying on CURRENT_TIMESTAMP / now() with
	// SQLite's second-level granularity. GetLatestVerdict and
	// LatestVerdictsForRuns both ORDER BY created_at DESC and rely on
	// strict insertion order being recoverable from the timestamp when
	// two verdicts land within the same wall-clock second.
	InsertVerdict(ctx context.Context, orgID string, runID string, metadataJSON string) error

	// GetLatestVerdict returns the most recent chain:verdict artifact
	// for a run, or (nil, nil) when no verdict exists — the orchestrator
	// treats that as the "no-verdict" abort default.
	GetLatestVerdict(ctx context.Context, orgID string, runID string) (*domain.ChainVerdict, error)

	// LatestVerdictsForRuns fetches the most-recent chain:verdict for
	// each of the supplied run IDs in a single query. Returns a map
	// keyed by runID; runs with no verdict are omitted. Used by
	// chains_handler.go to avoid an N+1 when rendering the chain-detail
	// page.
	LatestVerdictsForRuns(ctx context.Context, orgID string, runIDs []string) (map[string]*domain.ChainVerdict, error)

	// --- Admin-pool variants (`...System`) ---
	//
	// These mirror the per-method shape of the corresponding app-pool
	// methods but route through the admin pool (BYPASSRLS) in
	// Postgres. They exist for the chain orchestrator goroutine —
	// the long-running loop in delegateChain / runChain /
	// terminateChain that drives a chain through its step list. The
	// orchestrator detaches from the kicking-off handler's context
	// the moment it spawns, so it has no JWT-claims in scope and
	// would otherwise fail under RLS in multi-mode.
	//
	// Behavior contract is identical to the non-System variants;
	// org_id stays in every WHERE clause as defense in depth. The
	// only difference is which Postgres pool the statement runs on;
	// SQLite has one connection and the two variants collapse.
	//
	// CreateRun has no System counterpart — it routes internally on
	// the supplied ChainRun.TriggerType, mirroring the AgentRunStore
	// .Create pattern: event-triggered chain runs land on the admin
	// pool with NULL creator_user_id, manual chains on the app pool
	// with COALESCE fallback.
	ListStepsSystem(ctx context.Context, orgID string, chainPromptID string) ([]domain.ChainStep, error)
	GetRunSystem(ctx context.Context, orgID string, id string) (*domain.ChainRun, error)
	GetRunForRunSystem(ctx context.Context, orgID string, runID string) (*domain.ChainRun, *int, error)
	MarkRunStatusSystem(ctx context.Context, orgID string, id string, status domain.ChainRunStatus, abortReason string, abortedAtStep *int) (changed bool, err error)
	RunsForChainSystem(ctx context.Context, orgID string, chainRunID string) ([]domain.AgentRun, error)
	ActiveStepRunIDsSystem(ctx context.Context, orgID string, chainRunID string) ([]string, error)
	InsertVerdictSystem(ctx context.Context, orgID string, runID string, metadataJSON string) error
	GetLatestVerdictSystem(ctx context.Context, orgID string, runID string) (*domain.ChainVerdict, error)
}
