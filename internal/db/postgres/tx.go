package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// WithTx runs fn inside a single Postgres transaction against the app
// pool (RLS-active). Before fn runs, it calls
//
//	SELECT set_config('request.jwt.claims', $1, true)
//
// so RLS policies + tf.current_user_id() / tf.current_org_id() helpers
// see the right (orgID, userID) for this tx. set_config(..., true)
// scopes the setting to the tx — it doesn't leak to other connections
// in the pool.
//
// Callers always pass orgID + userID explicitly. D7 will replace the
// explicit pass with extraction from a request-scoped context (e.g.
// authctx.ClaimsFromContext(ctx)), but the WithTx shape stays the same.
//
// Closures that need to bypass RLS (system services) shouldn't use
// WithTx at all — they should call store methods directly on the
// admin-wired stores (db.Stores.Scores in wave 0; more in later
// waves). WithTx is purely for the request-handler atomicity boundary.
func (s *Store) WithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	return s.runClaimsBoundTx(ctx, orgID, userID, fn)
}

// SyntheticClaimsWithTx mirrors WithTx for callers that have an
// authoritative (orgID, userID) identity but no request context —
// delegate spawner goroutines, curator-message processing, post-
// terminal handler cleanup, agent CLI subcommands.
//
// The Postgres body is identical to WithTx — same role elevation,
// same JWT-claims setup, same TxStores wiring. The only difference
// is the *intent* of the call site: WithTx callers extract the pair
// from request context, SyntheticClaimsWithTx callers construct it
// from a known row identity (the run's creator_user_id, the curator
// session's user, etc.). Both run under tf_app, both honor RLS.
//
// userID must be a real users row id. Passing
// runmode.LocalDefaultUserID is rejected — that sentinel has no FK
// target in the multi-mode users table, and runs.creator_user_id
// has an FK to users(id). Callers that lack a real user identity
// (event-triggered runs by schema CHECK, system services) should
// route through the admin pool via per-store `...System` methods
// instead. See SKY-296.
func (s *Store) SyntheticClaimsWithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	if userID == runmode.LocalDefaultUserID {
		return errors.New("postgres: SyntheticClaimsWithTx rejected runmode.LocalDefaultUserID — sentinel has no FK target in multi-mode users; route to admin pool via per-store ...System methods")
	}
	if userID == "" {
		return errors.New("postgres: SyntheticClaimsWithTx requires a non-empty userID; route through admin pool for callers that have no user identity")
	}
	return s.runClaimsBoundTx(ctx, orgID, userID, fn)
}

// runClaimsBoundTx is the shared body between WithTx and
// SyntheticClaimsWithTx. The only structural difference between the
// two public entry points is the source of the (orgID, userID) pair
// (request context vs caller-supplied) plus the SyntheticClaimsWithTx
// guardrails enforced at the public layer — once we're past those,
// the SQL is identical.
func (s *Store) runClaimsBoundTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	tx, err := s.app.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Elevate the role before doing anything else. The app pool
	// connects as `authenticator` (LOGIN, NOINHERIT) which has no
	// privileges by design — RLS policies expect `tf_app` to be
	// the active role, and the pgtest harness's WithUser helper
	// does the same elevation. Without this, every WithTx-bound
	// store call would fail at the role layer (not even RLS — just
	// "permission denied" because authenticator has no grants).
	// SET LOCAL scopes the role change to the tx, so the pool
	// connection returns to authenticator when the tx ends.
	if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE tf_app`); err != nil {
		return err
	}

	claims, err := json.Marshal(map[string]any{
		"sub":    userID,
		"org_id": orgID,
	})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('request.jwt.claims', $1, true)`, string(claims)); err != nil {
		return err
	}

	// pending accumulates SetOnEventRecorded fires for events
	// recorded inside this tx's app-pool path. Fired post-commit
	// (below) so a rolled-back outer fn never inflates the
	// LifetimeDistinctCounter with a row the DB never persisted.
	// RecordSystem inside WithTx routes to s.admin (autonomous
	// pool, commits independent of this tx) — its hook fires
	// immediately on a successful INSERT, so we pass nil for the
	// admin-side pending below.
	pending := db.NewPendingEventHooks()
	if err := fn(s.txStoresFromTx(tx, pending)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	pending.Fire()
	return nil
}

// txStoresFromTx returns the TxStores bundle wired against a single
// *sql.Tx. Shared between the WithTx and SyntheticClaimsWithTx code
// paths so wiring drift is impossible: both entrypoints get the
// exact same set of tx-bound stores, with the same admin-pool
// retention for AgentRuns (event-triggered Create routes around RLS
// even from inside a claims-set tx — see the Create comment).
func (s *Store) txStoresFromTx(tx *sql.Tx, pending *db.PendingEventHooks) db.TxStores {
	return db.TxStores{
		Scores:        newScoreStore(tx),
		Prompts:       newTxPromptStore(tx),
		Swipes:        newSwipeStore(tx),
		Dashboard:     newDashboardStore(tx),
		Secrets:       newSecretStore(tx),
		EventHandlers: newTxEventHandlerStore(tx),
		// Chains: composed half is tx; admin half stays the real
		// admin pool so event-triggered CreateRun + the `...System`
		// reads route around RLS. The admin writes commit
		// autonomously from the outer tx — same pool-routing
		// semantics as AgentRunStore.Create.
		Chains:     newChainStore(tx, s.admin),
		Agents:     newTxAgentStore(tx),
		TeamAgents: newTxTeamAgentStore(tx),
		Users:      newUsersStore(tx, tx),
		Tasks:      newTaskStore(tx, s.admin),
		Factory:    newFactoryReadStore(tx),
		// AgentRuns: composed half is tx; admin half stays the
		// real admin pool so event-triggered Create can route
		// around RLS. The admin write commits autonomously from
		// the outer tx — see Create's pool-routing comment for
		// why that's the intended semantics.
		AgentRuns:      newAgentRunStore(tx, s.admin),
		Entities:       newEntityStore(tx, tx),
		Reviews:        newReviewStore(tx, s.admin),
		PendingPRs:     newPendingPRStore(tx, s.admin),
		Repos:          newRepoStore(tx, tx),
		PendingFirings: newPendingFiringsStore(tx),
		// Projects: ListSystem routes around RLS the same way
		// AgentRuns' event-triggered Create does. Keeping the admin
		// half pinned to s.admin lets the classifier read each org's
		// project set even when composed inside a claims-set tx.
		Projects: newProjectStore(tx, s.admin),
		// Events: app-side write defers hook firing via pending.Add
		// (drained post-commit by runClaimsBoundTx). Admin half
		// stays pinned to the real admin pool so RecordSystem /
		// GetMetadataSystem inside WithTx routes outside the tx —
		// those writes commit autonomously and fire their hook
		// immediately via db.NotifyEventRecorded.
		Events: newTxEventStore(tx, s.admin, pending.Add, db.NotifyEventRecorded),
	}
}
