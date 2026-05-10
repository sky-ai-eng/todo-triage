package db

import (
	"database/sql"
	"embed"
	"errors"
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

// installState classifies a SQLite DB into one of four shapes the
// migration runner cares about. The fourth — installPreRunner — is
// the case the binary detection (legacy table populated yes/no)
// failed to surface: a DB that has application tables but neither
// the goose tracker nor the pre-goose `schema_migrations` table.
// Those installs predate the hand-rolled runner entirely (the
// runner shipped before this PR's release; any binary that ran it
// at least once left `schema_migrations` populated). Silently
// stamping baseline against that shape would leave the schema
// behind whatever ALTERs subsequent hand-rolled migrations applied,
// because the consolidated baseline is `CREATE TABLE IF NOT EXISTS`
// and won't add missing columns. Easier to fail fast and route the
// operator through a known-good intermediate version.
type installState int

const (
	installFresh                 installState = iota // empty DB
	installAlreadyGooseManaged                       // goose_db_version exists with rows
	installLegacyRunnerPopulated                     // schema_migrations exists with rows
	installPreRunner                                 // app tables present, no version metadata
)

// ErrPreRunnerInstall is returned by Migrate when the DB has
// application tables but neither goose_db_version nor a populated
// schema_migrations. Surfaced verbatim by main.go's log.Fatalf so
// the operator sees the upgrade-path instructions.
var ErrPreRunnerInstall = errors.New(
	"this database appears to predate the schema migration runner; " +
		"to upgrade safely, install and run triagefactory v1.10.1 " +
		"first — it stamps the migration tracking table this version " +
		"reads — then upgrade to this version, or delete " +
		"~/.triagefactory/triagefactory.db to perform a fresh install",
)

// detectInstallState classifies the DB by probing in priority order:
// goose tracker first (already-managed installs short-circuit so the
// rest of the probe sequence stays simple), then the legacy table,
// then a known-stable application table. The application probe uses
// `entities` because it has been load-bearing since long before the
// migration runner existed — the same probe the old
// stampBaselineIfNeeded used.
func detectInstallState(db *sql.DB) (installState, error) {
	gooseTracked, err := tableHasRows(db, "goose_db_version")
	if err != nil {
		return 0, err
	}
	if gooseTracked {
		return installAlreadyGooseManaged, nil
	}

	legacyPopulated, err := tableHasRows(db, "schema_migrations")
	if err != nil {
		return 0, err
	}
	if legacyPopulated {
		return installLegacyRunnerPopulated, nil
	}

	hasApp, err := tableExistsInMaster(db, "entities")
	if err != nil {
		return 0, err
	}
	if hasApp {
		return installPreRunner, nil
	}
	return installFresh, nil
}

// importLegacyVersionsIfNeeded routes the boot through one of four
// install-state branches:
//
//   - installAlreadyGooseManaged → no-op; goose.Up handles its own state.
//   - installFresh               → no-op; goose.Up runs baseline.
//   - installLegacyRunnerPopulated → stamp baseline so goose.Up no-ops.
//   - installPreRunner           → fail fast with ErrPreRunnerInstall
//     so the operator routes through v1.10.1 (which stamps
//     schema_migrations) before reaching this binary.
//
// The legacy `schema_migrations` table is intentionally left in
// place as an audit trail / rollback safety net.
func importLegacyVersionsIfNeeded(db *sql.DB, dialect string) error {
	if dialect != "sqlite3" {
		// Postgres path lands with D6; fresh installs there will
		// have no legacy table to import from.
		return nil
	}

	state, err := detectInstallState(db)
	if err != nil {
		return err
	}
	switch state {
	case installAlreadyGooseManaged, installFresh:
		return nil
	case installPreRunner:
		return ErrPreRunnerInstall
	case installLegacyRunnerPopulated:
		// fall through to the stamp logic below
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

// tableHasRows reports whether the named table both exists AND
// contains at least one row. Used by detectInstallState to probe
// goose_db_version and schema_migrations without erroring on a
// missing table.
func tableHasRows(db *sql.DB, table string) (bool, error) {
	exists, err := tableExistsInMaster(db, table)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	var count int
	if err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)).Scan(&count); err != nil {
		return false, fmt.Errorf("count %s: %w", table, err)
	}
	return count > 0, nil
}

// tableExistsInMaster reports whether sqlite_master has a row for the
// named table. Cheaper than the full tableHasRows when the caller
// only cares about presence (e.g. probing for an application
// sentinel like `entities`).
func tableExistsInMaster(db *sql.DB, table string) (bool, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("probe sqlite_master for %s: %w", table, err)
	}
	return n > 0, nil
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
