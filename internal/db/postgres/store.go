// Package postgres is the Postgres-backed implementation of the
// per-resource store interfaces declared in package db. Multi-tenant
// installs of triagefactory wire this implementation at startup
// (local-mode wires internal/db/sqlite). See the SKY-246 D2 spec at
// docs/specs/sky-246-d2-store-abstraction.html for the full design,
// and the D3 schema at internal/db/migrations-postgres/.
//
// # Two-connection design
//
// New(admin, app) takes two *sql.DB handles. They serve different
// roles:
//
//   - admin: superuser / supabase_admin pool. RLS is bypassed for
//     this role. Used by (a) deploy-time operations like migrations
//     and system-prompt seeding, and (b) server-side system services
//     that need to read/write across users in an org without
//     impersonating each one (the AI scorer is the canonical
//     example — it has no user identity but must operate on every
//     queued task in the org).
//
//   - app: authenticator → tf_app role. RLS-active. Used by request
//     handlers; the TxRunner sets request.jwt.claims so policies
//     see (orgID, userID).
//
// Per-resource stores choose which queryer they wire against based
// on whether they serve request handlers or system services.
package postgres

import (
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// Store holds the two Postgres connection pools + the bundle of
// resource-store implementations wired against them. New returns the
// assembled db.Stores bundle for application wiring; downstream
// consumers such as handlers should depend on the specific store
// interfaces they need rather than the whole bundle.
type Store struct {
	admin *sql.DB
	app   *sql.DB

	stores db.Stores
}

// New wires a db.Stores bundle backed by Postgres. Wave 0 ships only
// ScoreStore + the TxRunner; subsequent waves populate the remaining
// 21 fields on the bundle.
//
// admin is the superuser pool (RLS bypassed); app is the tf_app
// authenticator pool (RLS-active). ScoreStore wires against admin
// because the scorer is a system service operating across all users
// in each org. Request-handler stores (added in later waves) wire
// against app and rely on WithTx to set per-request JWT claims.
func New(admin, app *sql.DB) db.Stores {
	s := &Store{admin: admin, app: app}
	s.stores = db.Stores{
		Scores: newScoreStore(admin),
		// PromptStore needs both pools: SeedOrUpdate writes to
		// system_prompt_versions (REVOKE'd from tf_app — admin only),
		// every other method runs on the app pool. The impl picks
		// per-method internally.
		Prompts:   newPromptStore(app, admin),
		Swipes:    newSwipeStore(app),
		Dashboard: newDashboardStore(app),
		// Secrets wraps the public.vault_* SECURITY DEFINER functions
		// — GRANTed to tf_app only. Caller must have set
		// request.jwt.claims before calling (the wrapper enforces
		// p_org_id == tf.current_org_id()).
		Secrets: newSecretStore(app),
		// EventHandlers needs both pools: Seed writes shipped rows
		// without JWT claims, but event_handlers_insert /
		// event_handlers_update RLS policies gate on either
		// creator_user_id = tf.current_user_id() or
		// tf.user_is_org_admin() on org-visible writes. The impl
		// routes Seed to admin (BYPASSRLS) and every CRUD method to
		// app — same pool-split pattern PromptStore + the predecessor
		// stores used.
		EventHandlers: newEventHandlerStore(app, admin),
		// Chains has no admin/app split — chain rows are user-created,
		// no boot-time seed needs to bypass RLS. All methods run on the
		// app pool; RLS enforces the creator predicate on chain_runs.
		Chains: newChainStore(app),
		// Agents.Create routes through admin (bootstrap has no JWT
		// claims and the agents_insert policy gates on
		// tf.user_is_org_admin); every other method on app. Same
		// pool-split pattern as PromptStore + TaskRuleStore.
		Agents: newAgentStore(app, admin),
		// TeamAgents.AddForTeam routes through admin for the same
		// bootstrap reason; SetEnabled/Overrides/Remove/Get run on
		// app where RLS gates by team membership.
		TeamAgents: newTeamAgentStore(app, admin),
		// Users only mutates existing rows (SKY-264 github_username
		// capture); row creation is an auth-flow concern owned by
		// SKY-251. All methods on app — RLS gates by
		// tf.user_can_read_user() / tf.user_can_update_user() once
		// those policies land.
		Users: newUsersStore(app),
		// Tasks runs on app — every consumer (server tasks handler,
		// router, delegate) is request-equivalent. The AI scorer uses
		// the admin-pooled ScoreStore for its system-service reads, so
		// TaskStore doesn't need an admin variant.
		Tasks: newTaskStore(app),
		// Factory wires admin — the snapshot is a system-level view
		// (no per-user identity, must see every in-flight run
		// regardless of creator) and the LifetimeDistinctCounter
		// Hydrate path runs at server startup before any JWT claims
		// are in scope.
		Factory: newFactoryReadStore(admin),
		// AgentRuns wires app — every consumer is request-
		// equivalent (server agent handler, delegate spawner
		// goroutine spawned from a handler, chains). System-service
		// reads of run state are routed through the admin-pooled
		// FactoryReadStore instead.
		// AgentRuns holds both pools. Manual-trigger Create + every
		// other method run on app (RLS-active). Event-triggered
		// Create routes to admin because the CHECK + RLS policy
		// pair makes that insert unreachable through tf_app — see
		// the impl's Create comment.
		AgentRuns: newAgentRunStore(app, admin),
		// Entities wires app — every consumer is request-equivalent
		// (server panels, classifier, delegate context loaders) or
		// runs in a server-side goroutine that already operates within
		// the org's identity scope (tracker, started at server boot).
		// RLS policy entities_all gates reads + writes on
		// (org_id = tf.current_org_id() AND tf.user_has_org_access).
		Entities: newEntityStore(app),
		Tx:       s,
	}
	return s.stores
}

// Connection openers (OpenAdmin, OpenApp) are NOT defined here in
// wave 0. main.go fatals before reaching them; introducing them now
// would require registering the pgx stdlib driver inside this
// package (a side-effect import) without any caller exercising it.
// SKY-251 (D7) owns the multi-mode startup wiring and will add the
// openers alongside the config + DSN plumbing that actually consumes
// them. Tests construct *sql.DB via the pgtest harness, which
// registers the pgx driver itself.

// NewForTx returns a db.TxStores wired against one *sql.Tx — the
// same shape WithTx produces internally for its closure body,
// exposed so tests can drive store methods against a claims-set
// transaction without going through a WithTx callback. The most
// prominent caller is the SecretStore test, where the vault
// wrapper refuses calls without a matching JWT claim.
//
// Returns db.TxStores (not db.Stores) deliberately: db.Stores
// carries a TxRunner, and a Stores{Tx: nil} would panic on
// stores.Tx.WithTx(...). TxStores has no Tx field, so misuse is
// a compile error rather than a runtime crash. Production code
// reaches the same wiring via Store.WithTx; this helper is the
// test-side door into it.
func NewForTx(tx *sql.Tx) db.TxStores {
	return db.TxStores{
		Scores:        newScoreStore(tx),
		Prompts:       newTxPromptStore(tx),
		Swipes:        newSwipeStore(tx),
		Dashboard:     newDashboardStore(tx),
		Secrets:       newSecretStore(tx),
		EventHandlers: newTxEventHandlerStore(tx),
		Chains:        newChainStore(tx),
		Agents:        newTxAgentStore(tx),
		TeamAgents:    newTxTeamAgentStore(tx),
		Users:         newUsersStore(tx),
		Tasks:         newTaskStore(tx),
		Factory:       newFactoryReadStore(tx),
		// NewForTx is a test door — both pools collapse to the
		// supplied tx. Tests that exercise the admin-only branch
		// (event-triggered AgentRunStore.Create) need the
		// production WithTx wiring instead, which gets the real
		// admin pool via Store.admin.
		AgentRuns: newAgentRunStore(tx, tx),
		Entities:  newEntityStore(tx),
	}
}
