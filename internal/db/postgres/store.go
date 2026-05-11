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
		Tx:      s,
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

// NewForTx wires a db.Stores bundle whose every field shares one
// *sql.Tx — the same shape WithTx produces internally, exposed for
// tests that need to drive store methods against a claims-set
// transaction (most prominently SecretStore, where the vault
// wrapper refuses calls without a matching JWT claim). Production
// code reaches the same wiring via Store.WithTx; this helper is a
// way for the conformance harness to skip the extra WithTx
// callback layer when it already controls the tx.
func NewForTx(tx *sql.Tx) db.Stores {
	return db.Stores{
		Scores:    newScoreStore(tx),
		Prompts:   newTxPromptStore(tx),
		Swipes:    newSwipeStore(tx),
		Dashboard: newDashboardStore(tx),
		Secrets:   newSecretStore(tx),
	}
}
