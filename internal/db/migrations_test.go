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
// without re-executing it, post-baseline migrations apply normally,
// and existing rows survive untouched.
//
// We simulate the pre-migration-system state by executing the
// baseline SQL directly (no schema_migrations row). That leaves a DB
// shaped exactly like an existing install at the cutover moment: all
// the baseline tables, none of the migration history, real data the
// runner must preserve. Migrate then stamps baseline and applies any
// post-baseline migrations against it.
func TestMigrate_StampsBaselineOnExistingInstall(t *testing.T) {
	database := openMigrationsTestDB(t)

	body, err := migrationsFS.ReadFile(migrationsDir + "/" + baselineVersion + ".sql")
	if err != nil {
		t.Fatalf("read baseline migration: %v", err)
	}
	if _, err := database.Exec(string(body)); err != nil {
		t.Fatalf("apply baseline (simulated existing install): %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO entities (id, source, source_id, kind) VALUES ('e1', 'github', 'owner/repo#1', 'pr')`,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
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

// TestMigrate_SKY204_DataCarryOver pins the data shape change in
// 20260501_002: pre-existing run_memory rows have their `content`
// renamed into `agent_content`; new `original_*` columns on the
// pending-review tables show up as NULL on rows that were inserted
// before the migration; and runs.memory_missing is gone.
//
// Simulates the worst case for a real existing install: a DB at
// baseline with live data in every affected table. Replaying
// 20260501_002 against that shape must keep the data intact while
// reshaping the schema around it.
func TestMigrate_SKY204_DataCarryOver(t *testing.T) {
	database := openMigrationsTestDB(t)

	body, err := migrationsFS.ReadFile(migrationsDir + "/" + baselineVersion + ".sql")
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	if _, err := database.Exec(string(body)); err != nil {
		t.Fatalf("apply baseline: %v", err)
	}

	// Seed the FK chain (events_catalog → entities → events → tasks →
	// runs → run_memory) plus a pending_reviews + pending_review_comments
	// row, all in their pre-migration shape.
	if _, err := database.Exec(`
		INSERT INTO events_catalog (id, source, category, label, description)
		VALUES ('github:pr:opened', 'github', 'pr', 'PR opened', '');
		INSERT INTO entities (id, source, source_id, kind, state)
		VALUES ('e1', 'github', 'owner/repo#1', 'pr', 'active');
		INSERT INTO events (id, entity_id, event_type, dedup_key)
		VALUES ('ev1', 'e1', 'github:pr:opened', '');
		INSERT INTO prompts (id, name, body) VALUES ('p1', 'Test', 'body');
		INSERT INTO tasks (id, entity_id, event_type, primary_event_id)
		VALUES ('t1', 'e1', 'github:pr:opened', 'ev1');
		INSERT INTO runs (id, task_id, prompt_id, status, memory_missing)
		VALUES ('r1', 't1', 'p1', 'completed', 0);
		INSERT INTO run_memory (id, run_id, entity_id, content)
		VALUES ('m1', 'r1', 'e1', 'agent wrote this');
		-- Legacy noncompliance row: pre-migration the column was NOT
		-- NULL but the empty string was a valid value, so a few real
		-- runs landed with content = ''. The migration must collapse
		-- those onto NULL so the new contract (NULL === didn't
		-- comply) actually catches them.
		INSERT INTO runs (id, task_id, prompt_id, status, memory_missing)
		VALUES ('r_empty', 't1', 'p1', 'completed', 1);
		INSERT INTO run_memory (id, run_id, entity_id, content)
		VALUES ('m_empty', 'r_empty', 'e1', '');
		-- Whitespace-only is the same noncompliance state (an agent
		-- that wrote a blank line is not actually conveying anything
		-- to the next run). char(9)=tab, char(10)=newline, char(13)=CR
		-- — the migration's TRIM expression must catch all four.
		INSERT INTO runs (id, task_id, prompt_id, status, memory_missing)
		VALUES ('r_ws', 't1', 'p1', 'completed', 1);
		INSERT INTO run_memory (id, run_id, entity_id, content)
		VALUES ('m_ws', 'r_ws', 'e1', '  ' || char(9) || char(10) || '  ');
		INSERT INTO pending_reviews (id, pr_number, owner, repo, commit_sha, run_id, review_body, review_event)
		VALUES ('pr1', 42, 'owner', 'repo', 'sha1', 'r1', 'pre-migration body', 'COMMENT');
		INSERT INTO pending_review_comments (id, review_id, path, line, body)
		VALUES ('c1', 'pr1', 'foo.go', 10, 'pre-migration comment');
	`); err != nil {
		t.Fatalf("seed pre-migration data: %v", err)
	}

	if err := Migrate(database); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Real-content row: carried into agent_content unchanged.
	var agentContent string
	var humanContent sql.NullString
	if err := database.QueryRow(
		`SELECT agent_content, human_content FROM run_memory WHERE id = 'm1'`,
	).Scan(&agentContent, &humanContent); err != nil {
		t.Fatalf("scan run_memory post-migration: %v", err)
	}
	if agentContent != "agent wrote this" {
		t.Errorf("agent_content = %q, want %q", agentContent, "agent wrote this")
	}
	if humanContent.Valid {
		t.Errorf("human_content = %q, want NULL", humanContent.String)
	}

	// Empty + whitespace-only legacy rows: must land as NULL post-
	// migration so the noncompliance contract is uniform across
	// fresh writes (UpsertAgentMemory already normalizes) and
	// historical data.
	for _, id := range []string{"m_empty", "m_ws"} {
		var agent sql.NullString
		if err := database.QueryRow(
			`SELECT agent_content FROM run_memory WHERE id = ?`, id,
		).Scan(&agent); err != nil {
			t.Fatalf("scan run_memory[%s]: %v", id, err)
		}
		if agent.Valid {
			t.Errorf("agent_content[%s] = %q, want NULL (legacy empty/whitespace must canonicalize)", id, agent.String)
		}
	}

	// pending_reviews + pending_review_comments: original_* NULL on
	// pre-existing rows (no draft to capture retroactively).
	var origReviewBody, origCommentBody sql.NullString
	if err := database.QueryRow(
		`SELECT original_review_body FROM pending_reviews WHERE id = 'pr1'`,
	).Scan(&origReviewBody); err != nil {
		t.Fatalf("scan pending_reviews: %v", err)
	}
	if origReviewBody.Valid {
		t.Errorf("original_review_body = %q, want NULL on pre-existing row", origReviewBody.String)
	}
	if err := database.QueryRow(
		`SELECT original_body FROM pending_review_comments WHERE id = 'c1'`,
	).Scan(&origCommentBody); err != nil {
		t.Fatalf("scan pending_review_comments: %v", err)
	}
	if origCommentBody.Valid {
		t.Errorf("original_body = %q, want NULL on pre-existing row", origCommentBody.String)
	}

	// runs.memory_missing column is gone — selecting it should error.
	if _, err := database.Exec(`SELECT memory_missing FROM runs LIMIT 1`); err == nil {
		t.Errorf("runs.memory_missing column still exists; expected DROP COLUMN to remove it")
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
