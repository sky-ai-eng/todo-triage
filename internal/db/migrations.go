package db

import (
	"database/sql"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log"

	"github.com/pressly/goose/v3"
)

//go:embed migrations-sqlite/*.sql
var migrationsSQLiteFS embed.FS

//go:embed migrations-postgres
var migrationsPostgresFS embed.FS

// baselineVersionID is the goose version_id of the consolidated
// baseline that absorbed the 18 hand-rolled migrations shipped before
// SKY-245 (D1). Encoded as YYYYMMDDNNNN so future migrations get a
// natural lexicographic / numeric ordering with the goose convention
// of int64 version IDs. This value is also written into
// `goose_db_version` by importLegacyVersionsIfNeeded for any existing
// install whose pre-goose `schema_migrations` table contains rows —
// avoids re-running the baseline against a DB that already has the
// schema in place.
const baselineVersionID int64 = 202605090001

// gooseVersionTableDDL is the schema the goose-sqlite3 dialect creates
// on first use. We pre-create it ourselves in the legacy-import shim
// so we can INSERT the baseline stamp before goose.Up's bookkeeping
// runs; goose's EnsureDBVersion sees the table already present and
// proceeds without re-creating. Mirror this verbatim if a future
// goose upgrade changes the canonical shape — divergence here would
// make the goose-managed inserts target a different schema than the
// runner expects.
const gooseVersionTableDDL = `
CREATE TABLE IF NOT EXISTS goose_db_version (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    version_id INTEGER NOT NULL,
    is_applied INTEGER NOT NULL,
    tstamp TIMESTAMP DEFAULT (datetime('now'))
)`

// runMigrations brings the on-disk schema up to head via goose. The
// hand-rolled runner that walked migrations/*.sql lexicographically
// is gone (see SKY-245's spec for the rationale); goose owns version
// tracking via `goose_db_version` from here on out.
//
// Sequence:
//  1. Detect dialect (sqlite3 today; the postgres tree is scaffolded
//     for SKY-247 / D6 but not yet exercised).
//  2. Run importLegacyVersionsIfNeeded — for any install whose
//     pre-goose `schema_migrations` table contains rows, stamp the
//     baseline as already applied so goose does not re-execute its
//     CREATE TABLE statements against the live schema.
//  3. Hand the routed embed.FS to goose and call goose.Up.
//
// Failures roll back at the per-migration boundary goose owns; the
// next launch retries any unapplied migration. The baseline is
// idempotent (every CREATE uses IF NOT EXISTS) so even if the legacy
// import shim no-ops on a borderline install — schema_migrations
// missing or empty — the baseline runs cleanly against the existing
// schema.
func runMigrations(db *sql.DB) error {
	dialect := detectDialect(db)
	if err := importLegacyVersionsIfNeeded(db, dialect); err != nil {
		return fmt.Errorf("legacy import: %w", err)
	}
	treeFS, treeDir, err := migrationsFor(dialect)
	if err != nil {
		return err
	}
	goose.SetBaseFS(treeFS)
	if err := goose.SetDialect(dialect); err != nil {
		return fmt.Errorf("set dialect %s: %w", dialect, err)
	}
	if err := goose.Up(db, treeDir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

// detectDialect returns the goose dialect string for the connected
// database. Today this is always sqlite3 — the Postgres path
// (SKY-247 / D6) will plumb a real driver-name probe through here.
// Centralizing the decision now keeps the call sites stable when that
// happens.
func detectDialect(_ *sql.DB) string {
	return "sqlite3"
}

// migrationsFor returns the embedded migration tree for a goose
// dialect. The trees are kept side-by-side so the parser only ever
// sees DDL it can interpret — no runtime if/else inside a single
// migration file deciding whether to emit BYTEA or BLOB. Pure-SQLite
// builds (the only supported runtime today) never read the postgres
// tree; once D6 lands the postgres path becomes a real consumer.
func migrationsFor(dialect string) (fs.FS, string, error) {
	switch dialect {
	case "sqlite3":
		return migrationsSQLiteFS, "migrations-sqlite", nil
	case "postgres":
		return migrationsPostgresFS, "migrations-postgres", nil
	default:
		return nil, "", fmt.Errorf("unsupported dialect %q", dialect)
	}
}

// importLegacyVersionsIfNeeded stamps the goose baseline as applied on
// installs that came in through the pre-goose `schema_migrations`
// runner. Any DB whose `schema_migrations` table exists AND contains
// at least one row is taken to already be at head — the only release
// users were ever on shipped after the last hand-rolled migration, so
// "schema_migrations has rows" means "this user has every legacy
// migration applied" (see SKY-245 spec for the explicit assumption).
//
// The shim is a no-op when:
//   - schema_migrations is missing (fresh install — let goose run baseline).
//   - schema_migrations exists but is empty (extremely unlikely; safest
//     to treat the same as missing).
//   - goose_db_version already has a baseline row (this boot already
//     stamped it; nothing to do).
//
// Idempotent across reboots: a stamped baseline is observed via the
// goose_db_version probe and the function returns without touching
// anything. The legacy `schema_migrations` table is intentionally
// left in place as an audit trail / rollback safety net per the
// design discussion.
func importLegacyVersionsIfNeeded(db *sql.DB, dialect string) error {
	if dialect != "sqlite3" {
		// Postgres path lands with D6; fresh installs there will
		// have no legacy table to import from.
		return nil
	}

	hasLegacy, err := legacyMigrationsHasRows(db)
	if err != nil {
		return err
	}
	if !hasLegacy {
		return nil
	}

	if _, err := db.Exec(gooseVersionTableDDL); err != nil {
		return fmt.Errorf("create goose_db_version: %w", err)
	}

	// goose_db_version has no UNIQUE constraint on version_id (only the
	// AUTOINCREMENT id is unique), so a check-then-insert pair would
	// admit duplicates if two processes raced past the existence gate
	// concurrently. INSERT ... SELECT ... WHERE NOT EXISTS folds the
	// existence check into the same statement; SQLite serializes
	// writes so the second racer sees the first's row and inserts
	// nothing. The bootstrap (version_id=0) and baseline rows use the
	// same shape — both are no-ops on the second call regardless of
	// concurrency.
	const stampSQL = `INSERT INTO goose_db_version (version_id, is_applied)
		SELECT ?, 1
		WHERE NOT EXISTS (SELECT 1 FROM goose_db_version WHERE version_id = ?)`
	if _, err := db.Exec(stampSQL, int64(0), int64(0)); err != nil {
		return fmt.Errorf("insert goose bootstrap row: %w", err)
	}
	res, err := db.Exec(stampSQL, baselineVersionID, baselineVersionID)
	if err != nil {
		return fmt.Errorf("stamp baseline: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected > 0 {
		log.Printf("[db] stamped goose baseline %d on existing install (legacy schema_migrations had rows)", baselineVersionID)
	}
	return nil
}

// legacyMigrationsHasRows reports whether the pre-goose
// `schema_migrations` table exists AND contains at least one row.
// Non-existence is silently false — the second probe (COUNT) only
// runs once we know the table is there, so we never error on a fresh
// install.
func legacyMigrationsHasRows(db *sql.DB) (bool, error) {
	var hasTable int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`,
	).Scan(&hasTable); err != nil {
		return false, fmt.Errorf("probe sqlite_master for schema_migrations: %w", err)
	}
	if hasTable == 0 {
		return false, nil
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		// A schema_migrations that exists but isn't queryable is
		// genuinely broken — surface it rather than swallowing.
		return false, fmt.Errorf("count schema_migrations: %w", err)
	}
	return count > 0, nil
}

// MigrationStatus prints the per-migration applied/pending state to w.
// Drives the `triagefactory migrate status` operator command. Calls
// importLegacyVersionsIfNeeded first so an existing install (whose
// goose_db_version was stamped lazily on its next server start)
// reports correctly even when status is the first command invoked
// after the goose cutover.
func MigrationStatus(db *sql.DB, w io.Writer) error {
	dialect := detectDialect(db)
	if err := importLegacyVersionsIfNeeded(db, dialect); err != nil {
		return fmt.Errorf("legacy import: %w", err)
	}
	treeFS, treeDir, err := migrationsFor(dialect)
	if err != nil {
		return err
	}
	goose.SetBaseFS(treeFS)
	if err := goose.SetDialect(dialect); err != nil {
		return fmt.Errorf("set dialect %s: %w", dialect, err)
	}
	// We render status ourselves rather than calling goose.Status —
	// goose.Status prints to its own logger which we don't want to
	// thread through the CLI. Quiet goose's chatty default logger so
	// it doesn't mix into our output.
	goose.SetLogger(goose.NopLogger())
	migrations, err := goose.CollectMigrations(treeDir, 0, goose.MaxVersion)
	if err != nil {
		return fmt.Errorf("collect migrations: %w", err)
	}
	current, err := goose.GetDBVersion(db)
	if err != nil {
		return fmt.Errorf("get db version: %w", err)
	}
	fmt.Fprintf(w, "    Status                      Migration\n")
	fmt.Fprintf(w, "    ====================================\n")
	for _, m := range migrations {
		state := "Pending"
		if m.Version <= current {
			state = "Applied"
		}
		fmt.Fprintf(w, "    %-27s %s\n", state, m.Source)
	}
	fmt.Fprintf(w, "\n    db version: %d\n", current)
	return nil
}
