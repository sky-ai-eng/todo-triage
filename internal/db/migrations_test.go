package db

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// openMigrationsTestDB returns a fresh, schema-less in-memory SQLite
// for migration tests. Distinct from newTestDB (which calls Migrate
// for you) — these tests exercise Migrate itself.
func openMigrationsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite memory: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })
	return database
}

// TestMigrate_FreshInstall pins the bootstrap path: a blank DB ends up
// with all baseline tables and a goose_db_version row stamping the
// v1.11.0 baseline as applied.
func TestMigrate_FreshInstall(t *testing.T) {
	database := openMigrationsTestDB(t)
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var version int64
	if err := database.QueryRow(
		`SELECT version_id FROM goose_db_version WHERE version_id = ?`, v1110BaselineVersionID,
	).Scan(&version); err != nil {
		t.Fatalf("baseline not stamped in goose_db_version: %v", err)
	}
	if version != v1110BaselineVersionID {
		t.Errorf("version_id = %d, want %d", version, v1110BaselineVersionID)
	}

	for _, table := range []string{"entities", "events", "tasks", "runs", "projects", "settings", "orgs", "users", "event_handlers"} {
		exists, err := tableExists(database, table)
		if err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if !exists {
			t.Errorf("%s table missing after fresh Migrate", table)
		}
	}
}

// TestMigrate_Idempotent guards the "launch on an up-to-date DB" case.
// Two Migrate calls in a row leave the post-state unchanged.
func TestMigrate_Idempotent(t *testing.T) {
	database := openMigrationsTestDB(t)
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	var n int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM goose_db_version WHERE version_id = ?`, v1110BaselineVersionID,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("goose_db_version[%d] count = %d, want 1", v1110BaselineVersionID, n)
	}
}

// TestMigrate_BricksPreV1110_GooseStampedAtOldBaseline covers the
// most-likely upgrade attempt: a user on the SKY-245 baseline (or any
// pre-v1.11.0 goose-tracked install) tries to upgrade. The
// goose_db_version table exists but doesn't contain the v1.11.0
// baseline version, so the brick check fires.
func TestMigrate_BricksPreV1110_GooseStampedAtOldBaseline(t *testing.T) {
	database := openMigrationsTestDB(t)

	if _, err := database.Exec(`
		CREATE TABLE goose_db_version (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			version_id INTEGER NOT NULL,
			is_applied INTEGER NOT NULL,
			tstamp TIMESTAMP DEFAULT (datetime('now'))
		)`); err != nil {
		t.Fatalf("create goose_db_version: %v", err)
	}
	// Stamp the pre-v1.11.0 SKY-245 baseline (202605090001) and one
	// post-baseline migration — simulating a typical upgrade path.
	for _, v := range []int64{0, 202605090001, 202605120003} {
		if _, err := database.Exec(
			`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, v,
		); err != nil {
			t.Fatalf("stamp version %d: %v", v, err)
		}
	}

	err := Migrate(database, "sqlite3")
	if !errors.Is(err, ErrPreV1110Install) {
		t.Fatalf("Migrate err = %v, want ErrPreV1110Install", err)
	}
	if !strings.Contains(err.Error(), "wipe") && !strings.Contains(err.Error(), "Wipe") {
		t.Errorf("error message should mention the wipe remediation, got: %v", err)
	}
}

// TestMigrate_BricksPreV1110_LegacySchemaMigrations covers the
// pre-goose runner case: the legacy `schema_migrations` table exists
// (left in place as audit trail by every prior install) plus the
// `entities` sentinel, but no goose tracker. Pre-v1.11.0.
func TestMigrate_BricksPreV1110_LegacySchemaMigrations(t *testing.T) {
	database := openMigrationsTestDB(t)

	if _, err := database.Exec(`
		CREATE TABLE schema_migrations (version TEXT PRIMARY KEY);
		INSERT INTO schema_migrations VALUES ('20260501_001_baseline');
		CREATE TABLE entities (id TEXT PRIMARY KEY);
	`); err != nil {
		t.Fatalf("stage legacy state: %v", err)
	}

	err := Migrate(database, "sqlite3")
	if !errors.Is(err, ErrPreV1110Install) {
		t.Fatalf("Migrate err = %v, want ErrPreV1110Install", err)
	}
}

// TestMigrate_BricksPreV1110_PreRunner covers the oldest case: app
// tables present, no version metadata at all. This shape predates
// even the legacy schema_migrations runner.
func TestMigrate_BricksPreV1110_PreRunner(t *testing.T) {
	database := openMigrationsTestDB(t)

	if _, err := database.Exec(`CREATE TABLE entities (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create entities: %v", err)
	}

	err := Migrate(database, "sqlite3")
	if !errors.Is(err, ErrPreV1110Install) {
		t.Fatalf("Migrate err = %v, want ErrPreV1110Install", err)
	}
}

// TestMigrationStatus_BricksPreV1110 covers the operator command
// surface: `triagefactory migrate status` against a pre-v1.11.0 DB
// should refuse cleanly through the same brick path Migrate uses,
// not emit a misleading "all pending" listing.
func TestMigrationStatus_BricksPreV1110(t *testing.T) {
	database := openMigrationsTestDB(t)

	if _, err := database.Exec(`CREATE TABLE entities (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create entities: %v", err)
	}

	var buf strings.Builder
	err := MigrationStatus(database, "sqlite3", &buf)
	if !errors.Is(err, ErrPreV1110Install) {
		t.Fatalf("MigrationStatus err = %v, want ErrPreV1110Install", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output on brick path, got: %q", buf.String())
	}
}
