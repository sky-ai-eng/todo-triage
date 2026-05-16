package db

import (
	"context"
	"errors"
)

// Stores bundles every per-resource store interface plus the
// transaction runner. Constructed once at startup by either
// internal/db/sqlite.New (local mode) or internal/db/postgres.New
// (multi mode); fields are populated wave by wave as SKY-246 lands.
//
// NEVER pass Stores to a handler. Handlers depend only on the
// specific interfaces they consume (db.ScoreStore, db.TaskStore, …).
// The bundle exists for main.go wiring and for the WithTx wrapper —
// nothing else. See docs/specs/sky-246-d2-store-abstraction.html §5.
type Stores struct {
	// Scores is the first store to land on the D2 wave 0 pilot.
	// Subsequent waves add the remaining 21 fields here.
	Scores ScoreStore

	// Prompts owns prompts + system_prompt_versions. SeedOrUpdate is
	// routed to the admin pool in Postgres (sidecar writes are
	// REVOKE'd from tf_app); every other method runs on the app pool.
	Prompts PromptStore

	// Swipes owns the swipe_events audit log + the task-status
	// transitions that follow each swipe.
	Swipes SwipeStore

	// Dashboard is a read-only projection over entities + their
	// snapshot_json blobs. Owns no table.
	Dashboard DashboardStore

	// Secrets is the per-org secret bag. Multi-only — SQLite impl
	// returns ErrNotApplicableInLocal for every method (local-mode
	// credentials live in the OS keychain, not the DB).
	Secrets SecretStore

	// EventHandlers owns the unified event_handlers table (post-SKY-259):
	// rules + triggers as one primitive with a kind discriminator.
	// Rules create unclaimed tasks (human triage); triggers also fire
	// an auto-delegation prompt. The router reads via GetEnabledForEvent
	// on every routed event; handlers do full CRUD per kind.
	EventHandlers EventHandlerStore

	// Chains owns prompt_chain_steps + chain_runs, plus the
	// kind='chain:verdict' slice of run_artifacts. Read by the chain
	// HTTP handlers; written by the delegate spawner and the exec
	// verdict subcommand.
	Chains ChainStore

	// Agents owns the agents table — the org's workload identity.
	// One row per org. Bootstrap-only Create (admin pool in Postgres);
	// reads + admin-gated updates run on the app pool. See SKY-260.
	Agents AgentStore

	// TeamAgents owns team_agents — per-team membership for the
	// agent + per-team config overrides. Bootstrap-only AddForTeam
	// (admin pool in Postgres); SetEnabled/SetOverrides/Remove run
	// on the app pool and gate on team membership via RLS.
	TeamAgents TeamAgentStore

	// Users owns the users table — non-secret identity facts like
	// display_name and github_username. The keychain holds the PAT;
	// the row holds everything else. See SKY-264 for the
	// github_username column that backs the predicate-matcher
	// allowlists.
	Users UsersStore

	// Tasks owns the tasks table — lifecycle, claims, dedup,
	// swipe-triggered transitions, plus the run-history queries
	// powering the auto-delegate breaker. App pool in Postgres
	// (RLS-active) since the queue + per-task surface is request-
	// driven; the AI scorer reads tasks via the admin-pooled
	// ScoreStore.
	Tasks TaskStore

	// Factory is the read-only projection that backs the
	// /api/factory/snapshot handler and the LifetimeDistinctCounter
	// reconciliation path. Admin pool in Postgres — the snapshot is
	// a system-level view (no per-user identity) and the hydrate
	// path runs at startup before any JWT claims are in scope.
	Factory FactoryReadStore

	// AgentRuns owns runs + run_messages — agent run lifecycle,
	// transcript, yield requests/responses. App pool in Postgres;
	// every consumer is request-equivalent or runs in a delegate
	// goroutine launched from a request handler.
	AgentRuns AgentRunStore

	// Entities owns the entities table — the long-lived source
	// objects (PR, Jira issue) every event/task/run hangs off. App
	// pool in Postgres; consumers are the tracker, projectclassify,
	// delegate context loaders, the scorer, and the server panels.
	Entities EntityStore

	// Reviews owns pending_reviews + pending_review_comments — the
	// agent-prepared GitHub review that sits in `pending_approval`
	// until the user accepts / edits / discards. App pool in
	// Postgres; consumers are the reviews handler, the spawner's
	// discard cleanup, the swipe-dismiss path, and the
	// cmd/exec/gh agent submit gate.
	Reviews ReviewStore

	// PendingPRs owns the pending_prs table — the agent-drafted PR
	// that sits in `pending_approval` until the user accepts / edits
	// / discards / submits. App pool in Postgres; consumers are the
	// pending_prs handler, the cmd/exec/gh agent pr-create tool, the
	// spawner's terminal-flip + cleanup paths, and tasks.go's drag-
	// back-to-queue cleanup. Leaf table — no child rows hang off it.
	PendingPRs PendingPRStore

	// Repos owns repo_profiles — the user-configured GitHub repos
	// plus their cached AI profile and clone-attempt state. App pool
	// in Postgres; consumers are the repos handler, settings, the
	// curator, the projects handler, the poller manager, the
	// profiler, and the workspace CLI tests. Every method accepts
	// repoID as "owner/repo" — Postgres splits to (owner, repo) and
	// queries by the natural key UNIQUE(org_id, owner, repo).
	Repos RepoStore

	// PendingFirings owns the pending_firings table — the FIFO queue
	// of intent-to-auto-delegate rows the router enqueues when an
	// entity already has an active auto run. Admin pool in Postgres
	// (the router has no per-user identity; system service).
	PendingFirings PendingFiringsStore

	// Projects owns the projects table — user-curated work groupings
	// (Linear/Jira project mirrors with pinned repos and the curator
	// session that maintains the project's knowledge dir). App pool
	// in Postgres; consumers are the projects handler, curator,
	// backfill, project_entities, projectclassify runner, and the
	// projectbundle import/export paths.
	Projects ProjectStore

	// Events owns the events audit log — append-only event rows the
	// router records and the factory/delegate paths read. Holds both
	// pools (SKY-305): app for request-handler equivalents (stock
	// carry-over, factory drag-to-delegate) and admin for background
	// goroutines without JWT-claims context (router RecordSystem +
	// re-derive, delegate post-run metadata enrichment).
	Events EventStore

	// TaskMemory owns the run_memory table — per-run agent narrative
	// + human verdict, read back by the delegate spawner to
	// materialize prior context into fresh worktrees. Holds both
	// pools: app for request-handler equivalents (review/PR submit,
	// swipe-discard cleanup, factory/run-summary reads) and admin for
	// the spawner's runAgent goroutine (post-completion upsert + run-
	// start materializer, both without a JWT-claims context).
	TaskMemory TaskMemoryStore

	// RunWorktrees owns the run_worktrees table — one row per
	// (run_id, repo_id) lazy worktree reservation a Jira-style run
	// accumulates as the agent materializes repos via `workspace
	// add`. Holds both pools: app for the cmd/exec workspace CLI
	// (its synthetic-claims wrap is owned by a separate cmd/exec
	// auth pass) and admin for the spawner's runAgent + chain
	// orchestrator cleanup defers (no JWT-claims context).
	RunWorktrees RunWorktreeStore

	// Tx is the transaction runner — handlers that need atomic
	// multi-store writes call Tx.WithTx and receive a TxStores with
	// every field tx-bound. Postgres impl also sets the JWT claims
	// that RLS policies + tf.current_user_id() / tf.current_org_id()
	// read from.
	Tx TxRunner
}

// TxStores mirrors Stores but each field is bound to a single
// *sql.Tx so the closure body inside WithTx runs every operation
// in the same transaction. Fields are added as their parent stores
// land in successive waves.
type TxStores struct {
	Scores         ScoreStore
	Prompts        PromptStore
	Swipes         SwipeStore
	Dashboard      DashboardStore
	Secrets        SecretStore
	EventHandlers  EventHandlerStore
	Chains         ChainStore
	Agents         AgentStore
	TeamAgents     TeamAgentStore
	Users          UsersStore
	Tasks          TaskStore
	Factory        FactoryReadStore
	AgentRuns      AgentRunStore
	Entities       EntityStore
	Reviews        ReviewStore
	PendingPRs     PendingPRStore
	Repos          RepoStore
	PendingFirings PendingFiringsStore
	Projects       ProjectStore
	Events         EventStore
	TaskMemory     TaskMemoryStore
	RunWorktrees   RunWorktreeStore
}

// TxRunner runs fn inside a single database transaction. Postgres
// impl additionally calls
//
//	SELECT set_config('request.jwt.claims', $1, true)
//
// before fn so RLS policies see the right (orgID, userID) claims for
// this transaction. set_config(..., true) scopes to the tx and does
// not leak to other pool connections. SQLite impl ignores orgID /
// userID beyond asserting orgID == runmode.LocalDefaultOrg.
//
// Callers always pass orgID + userID explicitly — D7 will replace
// the explicit pass with extraction from a request-scoped context,
// but the WithTx shape stays the same.
//
// SyntheticClaimsWithTx mirrors WithTx for callers that have an
// authoritative (orgID, userID) identity but no request context —
// delegate-spawner goroutines, curator-message processing, post-
// terminal handler cleanup, agent CLI subcommands. The only
// structural difference from WithTx is the source of the claims
// values: request context vs caller-supplied. The Postgres impl
// shares its body with WithTx via a private helper; the SQLite
// impl asserts orgID == runmode.LocalDefaultOrg and ignores
// userID (no auth concept in local mode).
//
// userID is required and must reference a real users row in
// Postgres — runs.creator_user_id has an FK to users(id). Callers
// that don't have a real user (event-triggered run completion,
// system services) should route to admin pool via the per-store
// `...System` methods instead. Passing runmode.LocalDefaultUserID
// in production multi-mode is rejected with a clear error because
// that sentinel has no FK target in the multi-mode users table.
type TxRunner interface {
	WithTx(ctx context.Context, orgID, userID string, fn func(TxStores) error) error
	SyntheticClaimsWithTx(ctx context.Context, orgID, userID string, fn func(TxStores) error) error
}

// ErrNotApplicableInLocal is returned by SQLite impls of multi-only
// store methods (SessionStore.Insert, MembershipStore.Add, …). The
// auth path is gated behind runmode.ModeMulti, so this should never
// reach a production user; the error is the safety net for code that
// escapes that gate.
var ErrNotApplicableInLocal = errors.New("db: operation not applicable in local mode")
