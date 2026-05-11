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
// resource-store implementations wired against them. Returned by
// New(); the bundle (db.Stores) is what main.go hands to handlers.
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
		Tx:     s,
	}
	return s.stores
}

// OpenAdmin opens a connection pool against the superuser DSN. Wraps
// sql.Open with the pgx driver so callers don't have to remember the
// driver name. Caller owns the returned *sql.DB and is responsible
// for Close().
func OpenAdmin(dsn string) (*sql.DB, error) {
	return openPGX(dsn)
}

// OpenApp opens a connection pool against the tf_app authenticator
// DSN. Same wrapper as OpenAdmin — kept as a separate function for
// future-proofing (the app pool may want different sql.DB tuning
// once we have multi-mode in production, e.g. MaxIdleConns).
func OpenApp(dsn string) (*sql.DB, error) {
	return openPGX(dsn)
}

func openPGX(dsn string) (*sql.DB, error) {
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}
