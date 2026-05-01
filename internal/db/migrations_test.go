package db

import (
	"database/sql"
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
// with all baseline tables and exactly one schema_migrations row for
// the baseline version. Hand-checked with the entities table because
// it's the same probe stampBaselineIfNeeded uses, so a regression in
// the probe surface (table renamed, column added before stamp ran)
// would also fail this test.
func TestMigrate_FreshInstall(t *testing.T) {
	database := openMigrationsTestDB(t)
	if err := Migrate(database); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var version string
	if err := database.QueryRow(`SELECT version FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("schema_migrations row: %v", err)
	}
	if version != baselineVersion {
		t.Errorf("version = %q, want %q", version, baselineVersion)
	}

	if !tableExists(t, database, "entities") {
		t.Errorf("entities table missing after fresh Migrate")
	}
	if !tableExists(t, database, "events") {
		t.Errorf("events table missing after fresh Migrate")
	}
	if !tableExists(t, database, "tasks") {
		t.Errorf("tasks table missing after fresh Migrate")
	}
}

// TestMigrate_Idempotent guards the "launch on an up-to-date DB"
// case. Two Migrate calls in a row leave exactly one schema_migrations
// row per version. A regression here would mean a new launch
// accumulates duplicate rows or worse, re-runs a migration that
// happened not to be idempotent at the SQL level.
func TestMigrate_Idempotent(t *testing.T) {
	database := openMigrationsTestDB(t)
	if err := Migrate(database); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := Migrate(database); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	var n int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, baselineVersion,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("schema_migrations[%s] count = %d, want 1", baselineVersion, n)
	}
}

// TestMigrate_StampsBaselineOnExistingInstall is the existing-user
// upgrade regression: a DB whose application tables predate the
// migration system gets the baseline recorded as already-applied
// without re-executing it, and existing rows survive untouched.
//
// We simulate the pre-migration-system state by running Migrate
// (which builds the schema), seeding a real entity, then dropping
// schema_migrations. That leaves a DB shaped exactly like an upgrade
// candidate: all the tables, none of the migration history, real data
// the runner must preserve.
func TestMigrate_StampsBaselineOnExistingInstall(t *testing.T) {
	database := openMigrationsTestDB(t)
	if err := Migrate(database); err != nil {
		t.Fatalf("setup Migrate: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO entities (id, source, source_id, kind) VALUES ('e1', 'github', 'owner/repo#1', 'pr')`,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := database.Exec(`DROP TABLE schema_migrations`); err != nil {
		t.Fatalf("drop schema_migrations: %v", err)
	}

	if err := Migrate(database); err != nil {
		t.Fatalf("Migrate on simulated upgrade: %v", err)
	}

	var version string
	if err := database.QueryRow(
		`SELECT version FROM schema_migrations WHERE version = ?`, baselineVersion,
	).Scan(&version); err != nil {
		t.Fatalf("baseline not stamped: %v", err)
	}

	var entityID string
	if err := database.QueryRow(`SELECT id FROM entities WHERE id = 'e1'`).Scan(&entityID); err != nil {
		t.Fatalf("entity row not preserved: %v", err)
	}
}

// TestMigrate_FreshDB_NoStamp guards the inverse of the above: a
// blank DB (no entities table) must NOT be flagged as an existing
// install. The runner should run the baseline normally — stamping a
// blank DB would skip the only migration that creates tables, leaving
// the install unusable.
func TestMigrate_FreshDB_NoStamp(t *testing.T) {
	database := openMigrationsTestDB(t)

	// Pre-create only schema_migrations as the runner does, then call
	// stampBaselineIfNeeded directly. With no entities table present,
	// it must be a no-op (no row inserted).
	if _, err := database.Exec(`
		CREATE TABLE schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		t.Fatalf("seed schema_migrations: %v", err)
	}
	if err := stampBaselineIfNeeded(database); err != nil {
		t.Fatalf("stampBaselineIfNeeded: %v", err)
	}

	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("stamp inserted %d rows on blank DB; want 0", n)
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
