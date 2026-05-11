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
// baseline as applied.
func TestMigrate_FreshInstall(t *testing.T) {
	database := openMigrationsTestDB(t)
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var version int64
	if err := database.QueryRow(
		`SELECT version_id FROM goose_db_version WHERE version_id = ?`, baselineVersionID,
	).Scan(&version); err != nil {
		t.Fatalf("baseline not stamped in goose_db_version: %v", err)
	}
	if version != baselineVersionID {
		t.Errorf("version_id = %d, want %d", version, baselineVersionID)
	}

	for _, table := range []string{"entities", "events", "tasks", "runs", "projects", "settings"} {
		if !tableExists(t, database, table) {
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
		`SELECT COUNT(*) FROM goose_db_version WHERE version_id = ?`, baselineVersionID,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("goose_db_version[%d] count = %d, want 1", baselineVersionID, n)
	}
}

// seedFullLegacyState writes all 18 expected legacy version strings
// into a freshly-created schema_migrations table plus an entities row
// that survives the upgrade. Used by tests covering the
// installLegacyRunnerPopulated path; the partial-legacy variant uses
// a similar helper but omits one of the expected versions.
func seedFullLegacyState(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(`
		CREATE TABLE schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE entities (
			id TEXT PRIMARY KEY,
			source TEXT NOT NULL,
			source_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			state TEXT NOT NULL DEFAULT 'active',
			UNIQUE(source, source_id)
		);
		INSERT INTO entities (id, source, source_id, kind) VALUES ('e1', 'github', 'owner/repo#1', 'pr');

		-- Tables that forward SQLite migrations (post-baseline) ALTER.
		-- Real legacy installs ran the pre-goose runner which created
		-- these; the fixture has to mirror that or new migrations
		-- targeting them error on "no such table". Add to this block
		-- whenever a new forward migration alters an existing table.
		CREATE TABLE prompt_triggers (
			id TEXT PRIMARY KEY,
			prompt_id TEXT NOT NULL,
			trigger_type TEXT NOT NULL DEFAULT 'event',
			event_type TEXT NOT NULL,
			scope_predicate_json TEXT,
			breaker_threshold INTEGER NOT NULL DEFAULT 4,
			min_autonomy_suitability REAL NOT NULL DEFAULT 0.0,
			enabled BOOLEAN NOT NULL DEFAULT 1,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
	`); err != nil {
		t.Fatalf("seed schema + data: %v", err)
	}
	for _, v := range expectedLegacyVersions {
		if _, err := database.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, v); err != nil {
			t.Fatalf("seed legacy version %q: %v", v, err)
		}
	}
}

// TestMigrate_StampsBaselineOnExistingInstall is the existing-user
// upgrade regression: a DB that came in through the pre-goose
// `schema_migrations` runner with all 18 expected versions gets the
// baseline stamped in goose_db_version without re-executing the
// consolidated baseline's CREATE statements (which would error
// against tables that already exist with data).
func TestMigrate_StampsBaselineOnExistingInstall(t *testing.T) {
	database := openMigrationsTestDB(t)
	seedFullLegacyState(t, database)

	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate on simulated upgrade: %v", err)
	}

	var version int64
	if err := database.QueryRow(
		`SELECT version_id FROM goose_db_version WHERE version_id = ?`, baselineVersionID,
	).Scan(&version); err != nil {
		t.Fatalf("baseline not stamped: %v", err)
	}

	var entityID string
	if err := database.QueryRow(`SELECT id FROM entities WHERE id = 'e1'`).Scan(&entityID); err != nil {
		t.Fatalf("entity row not preserved: %v", err)
	}

	// Legacy schema_migrations is intentionally left in place as an
	// audit trail / rollback safety net — every expected version
	// stays populated post-stamp.
	var legacyCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&legacyCount); err != nil {
		t.Fatalf("query legacy schema_migrations: %v", err)
	}
	if legacyCount != len(expectedLegacyVersions) {
		t.Errorf("legacy schema_migrations rows = %d, want %d (audit trail must be preserved in full)", legacyCount, len(expectedLegacyVersions))
	}
}

// TestMigrate_PartialLegacyInstallErrors covers the case where
// schema_migrations is populated but missing one of the 18 expected
// versions — the silent-corruption hole flagged when a real user hit
// it (their DB had 17 of 18 rows, missing
// 20260507_001_prompt_allowed_tools, so the prompts table never got
// the allowed_tools column despite goose stamping baseline as
// applied). Migrate must refuse with ErrPartialLegacyInstall pointing
// the operator at v1.10.1 and must NOT create goose_db_version (no
// half-stamping).
func TestMigrate_PartialLegacyInstallErrors(t *testing.T) {
	database := openMigrationsTestDB(t)
	if _, err := database.Exec(`
		CREATE TABLE schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE entities (
			id TEXT PRIMARY KEY,
			source TEXT NOT NULL,
			source_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			UNIQUE(source, source_id)
		);
	`); err != nil {
		t.Fatalf("seed bare schema: %v", err)
	}
	// Seed 17 of 18 — omit 20260507_001_prompt_allowed_tools, the
	// exact version the real user's DB was missing.
	for _, v := range expectedLegacyVersions {
		if v == "20260507_001_prompt_allowed_tools" {
			continue
		}
		if _, err := database.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, v); err != nil {
			t.Fatalf("seed version %q: %v", v, err)
		}
	}

	err := Migrate(database, "sqlite3")
	if err == nil {
		t.Fatalf("Migrate against partial-legacy DB should have errored")
	}
	if !errors.Is(err, ErrPartialLegacyInstall) {
		t.Fatalf("Migrate err = %v, want wraps ErrPartialLegacyInstall", err)
	}
	if !strings.Contains(err.Error(), "v1.10.1") {
		t.Errorf("error message must reference v1.10.1; got %q", err.Error())
	}

	// Sanity: nothing was stamped — partial-state must not lead to
	// half-applied goose state.
	if tableExists(t, database, "goose_db_version") {
		t.Errorf("goose_db_version was created — partial-legacy detection should have refused before any write")
	}
}

// TestMigrate_EmptyLegacyTableRunsBaseline guards the inverse: a DB
// that has the legacy `schema_migrations` table but no rows (rare but
// possible if a prior boot crashed mid-Migrate) must NOT be flagged
// as an existing install. The runner should run the baseline
// normally.
func TestMigrate_EmptyLegacyTableRunsBaseline(t *testing.T) {
	database := openMigrationsTestDB(t)
	if _, err := database.Exec(`
		CREATE TABLE schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		t.Fatalf("seed empty schema_migrations: %v", err)
	}

	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate with empty legacy table: %v", err)
	}

	if !tableExists(t, database, "entities") {
		t.Errorf("entities table missing — baseline should have run on empty legacy table")
	}
}

// TestMigrate_PreRunnerInstallErrors covers the install shape that
// would otherwise corrupt silently: application tables present
// (entities here) but neither goose_db_version nor schema_migrations.
// That state predates the hand-rolled runner — any binary that ran
// it left schema_migrations behind. Migrate must refuse to stamp
// baseline against this shape and return ErrPreRunnerInstall with a
// pointer at the v1.10.1 intermediate-upgrade path.
func TestMigrate_PreRunnerInstallErrors(t *testing.T) {
	database := openMigrationsTestDB(t)
	if _, err := database.Exec(`
		CREATE TABLE entities (
			id TEXT PRIMARY KEY,
			source TEXT NOT NULL,
			source_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			UNIQUE(source, source_id)
		);
		INSERT INTO entities (id, source, source_id, kind) VALUES ('e1', 'github', 'a/b#1', 'pr');
	`); err != nil {
		t.Fatalf("seed pre-runner state: %v", err)
	}

	err := Migrate(database, "sqlite3")
	if err == nil {
		t.Fatalf("Migrate against pre-runner DB should have errored")
	}
	if !errors.Is(err, ErrPreRunnerInstall) {
		t.Fatalf("Migrate err = %v, want wraps ErrPreRunnerInstall", err)
	}
	if !strings.Contains(err.Error(), "v1.10.1") {
		t.Errorf("error message must reference v1.10.1; got %q", err.Error())
	}

	// Sanity: nothing was stamped, baseline did not run, the seeded
	// row is still there.
	if tableExists(t, database, "goose_db_version") {
		t.Errorf("goose_db_version was created — pre-runner detection should have refused before any write")
	}
	var seedID string
	if err := database.QueryRow(`SELECT id FROM entities WHERE id = 'e1'`).Scan(&seedID); err != nil {
		t.Fatalf("seed row not preserved: %v", err)
	}
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name,
	).Scan(&n); err != nil {
		t.Fatalf("probe table %q: %v", name, err)
	}
	return n == 1
}
