package db

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	_ "modernc.org/sqlite"
)

// migrateUpTo brings the database forward to the named version using
// the same FS + dialect plumbing runMigrations does. Helper for the
// upgrade-path tests that stage an in-between state before testing
// a specific subsequent migration.
func migrateUpTo(t *testing.T, db *sql.DB, version int64) {
	t.Helper()
	treeFS, treeDir, err := migrationsFor("sqlite3")
	if err != nil {
		t.Fatalf("migrationsFor sqlite3: %v", err)
	}
	goose.SetBaseFS(treeFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}
	if err := goose.UpTo(db, treeDir, version); err != nil {
		t.Fatalf("goose UpTo %d: %v", version, err)
	}
}

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
		CREATE TABLE prompts (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			body TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT 'user',
			usage_count INTEGER DEFAULT 0,
			hidden BOOLEAN DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			user_modified INTEGER NOT NULL DEFAULT 0,
			allowed_tools TEXT NOT NULL DEFAULT ''
		);
		-- Every other table SKY-269's 202605120003_local_tenancy.sql
		-- ALTERs. Minimal column set — just (id) — because the test
		-- migration adds org_id / team_id / creator_user_id via ALTER
		-- TABLE ADD COLUMN, which doesn't care about the existing
		-- column shape. The agents + team_agents tables need full
		-- shape because 202605120003 does a rebuild (INSERT...SELECT
		-- against every column).
		CREATE TABLE projects (id TEXT PRIMARY KEY);
		CREATE TABLE entity_links (id INTEGER PRIMARY KEY);
		CREATE TABLE events (id TEXT PRIMARY KEY);
		-- task_rules carries its full post-baseline column shape because
		-- 202605120008_event_handlers_unification.sql backfills from
		-- task_rules.{event_type, scope_predicate_json, enabled, name,
		-- default_priority, sort_order, source, created_at, updated_at}
		-- in addition to the SKY-269-added columns. INSERT...SELECT
		-- requires those columns to exist even if no rows are present.
		CREATE TABLE task_rules (
			id TEXT PRIMARY KEY,
			event_type TEXT NOT NULL,
			scope_predicate_json TEXT,
			enabled BOOLEAN NOT NULL DEFAULT 1,
			name TEXT NOT NULL,
			default_priority REAL NOT NULL DEFAULT 0.5,
			sort_order INTEGER NOT NULL DEFAULT 0,
			source TEXT NOT NULL DEFAULT 'user',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		-- tasks carries its full pre-tenancy column shape because
		-- 202605120010_d_claims.sql rebuilds the table (table-rebuild
		-- dance to fold in the claim_xor CHECK). INSERT...SELECT in
		-- the rebuild requires every selected column to exist on the
		-- source, even if no rows are present. SKY-269 adds org_id /
		-- team_id / creator_user_id via ALTER TABLE ADD COLUMN; SKY-262
		-- adds visibility — those are NOT in this fixture because the
		-- legacy snapshot pre-dates both migrations.
		CREATE TABLE tasks (
			id                   TEXT PRIMARY KEY,
			entity_id            TEXT,
			event_type           TEXT,
			dedup_key            TEXT NOT NULL DEFAULT '',
			primary_event_id     TEXT,
			status               TEXT NOT NULL DEFAULT 'queued',
			priority_score       REAL,
			ai_summary           TEXT,
			autonomy_suitability REAL,
			priority_reasoning   TEXT,
			scoring_status       TEXT NOT NULL DEFAULT 'pending',
			severity             TEXT,
			relevance_reason     TEXT,
			source_status        TEXT,
			snooze_until         DATETIME,
			close_reason         TEXT,
			close_event_type     TEXT,
			closed_at            DATETIME,
			created_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE task_events (id INTEGER PRIMARY KEY);
		-- runs + pending_firings carry their full column shape because
		-- 202605120008_event_handlers_unification.sql rebuilds both
		-- (table-rebuild dance to swap trigger_id's FK from
		-- prompt_triggers to event_handlers). INSERT...SELECT requires
		-- the columns to exist on the source even if no rows are present.
		CREATE TABLE runs (
			id TEXT PRIMARY KEY,
			task_id TEXT,
			prompt_id TEXT,
			trigger_id TEXT,
			trigger_type TEXT NOT NULL DEFAULT 'manual',
			status TEXT NOT NULL DEFAULT 'cloning',
			model TEXT,
			session_id TEXT,
			worktree_path TEXT,
			result_summary TEXT,
			stop_reason TEXT,
			started_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			completed_at DATETIME,
			duration_ms INTEGER,
			num_turns INTEGER,
			total_cost_usd REAL
		);
		CREATE TABLE run_artifacts (id INTEGER PRIMARY KEY);
		CREATE TABLE run_messages (id INTEGER PRIMARY KEY);
		CREATE TABLE run_memory (id INTEGER PRIMARY KEY);
		CREATE TABLE pending_firings (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			entity_id TEXT,
			task_id TEXT,
			trigger_id TEXT,
			triggering_event_id TEXT,
			status TEXT NOT NULL DEFAULT 'pending',
			skip_reason TEXT,
			queued_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			drained_at DATETIME,
			fired_run_id TEXT
		);
		CREATE TABLE run_worktrees (id INTEGER PRIMARY KEY);
		CREATE TABLE pending_prs (id INTEGER PRIMARY KEY);
		CREATE TABLE swipe_events (id INTEGER PRIMARY KEY);
		CREATE TABLE repo_profiles (id INTEGER PRIMARY KEY);
		CREATE TABLE poller_state (id TEXT PRIMARY KEY);
		CREATE TABLE pending_reviews (id INTEGER PRIMARY KEY);
		CREATE TABLE pending_review_comments (id INTEGER PRIMARY KEY);
		CREATE TABLE curator_requests (id INTEGER PRIMARY KEY);
		CREATE TABLE curator_messages (id INTEGER PRIMARY KEY);
		CREATE TABLE curator_pending_context (id INTEGER PRIMARY KEY);
		-- agents + team_agents shipped in 202605120001_agents.sql with
		-- the full column shape below; 202605120003 rebuilds them so
		-- the fixture must mirror the post-202605120001 columns for
		-- the INSERT...SELECT to find them.
		CREATE TABLE agents (
			id TEXT PRIMARY KEY,
			display_name TEXT NOT NULL DEFAULT 'Triage Factory Bot',
			default_model TEXT,
			default_autonomy_suitability REAL,
			github_app_installation_id TEXT,
			github_pat_user_id TEXT,
			jira_service_account_id TEXT,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE team_agents (
			team_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			per_team_model TEXT,
			per_team_autonomy_suitability REAL,
			added_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (team_id, agent_id)
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

// TestMigrate_SKY269BackfillsPATUserOnUpgrade pins the upgrade-path
// fix from 202605120003_local_tenancy.sql. Pre-269 installs ran the
// SKY-260 migration which created an agents row with
// github_pat_user_id NULL (no users FK target existed locally yet).
// Post-269 the column references users(id) — and BootstrapLocalAgent
// at the next boot would NOT touch the existing row because Create's
// INSERT OR IGNORE conflicts on UNIQUE(org_id). The migration itself
// runs the backfill UPDATE so the upgrade path resolves cleanly.
//
// The test stages: a goose-stamped install at version 202605120002
// (post-prompts_model, pre-local_tenancy) with an agents row in the
// pre-269 shape (NULL github_pat_user_id). Then Migrate forwards
// through 202605120003. After: the agents row's github_pat_user_id
// equals runmode.LocalDefaultUserID.
func TestMigrate_SKY269BackfillsPATUserOnUpgrade(t *testing.T) {
	database := openMigrationsTestDB(t)

	// Apply baseline + everything through 202605120002. Then stop
	// short of 202605120003 so we can stage a pre-269 agents row
	// before the local_tenancy migration runs.
	migrateUpTo(t, database, 202605120002)

	// Stage: a pre-269 agents row with NULL github_pat_user_id. This
	// matches what SKY-260's bootstrap would have inserted on a
	// pre-prompts_model install. id is the BootstrapAgentID value
	// the pre-269 store used (UUID5 derived from "default"); the
	// 202605120003 rebuild rewrites it to LocalDefaultAgentID.
	if _, err := database.Exec(`
		INSERT INTO agents (id, display_name, github_pat_user_id, github_app_installation_id)
		VALUES ('pre-269-derived-id', 'Triage Factory Bot', NULL, NULL)
	`); err != nil {
		t.Fatalf("seed pre-269 agents row: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO team_agents (team_id, agent_id, enabled)
		VALUES ('default', 'pre-269-derived-id', 1)
	`); err != nil {
		t.Fatalf("seed pre-269 team_agents row: %v", err)
	}

	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate forward through 202605120003: %v", err)
	}

	var patUserID sql.NullString
	if err := database.QueryRow(
		`SELECT github_pat_user_id FROM agents WHERE org_id = ?`,
		runmode.LocalDefaultOrgID,
	).Scan(&patUserID); err != nil {
		t.Fatalf("read post-migration agents row: %v", err)
	}
	if !patUserID.Valid {
		t.Fatal("github_pat_user_id is still NULL after upgrade; backfill UPDATE didn't fire")
	}
	if patUserID.String != runmode.LocalDefaultUserID {
		t.Errorf("github_pat_user_id = %q, want %q (sentinel user)",
			patUserID.String, runmode.LocalDefaultUserID)
	}
}

// TestMigrate_SKY269PreservesExistingPATConfig pins the other side of
// the backfill: if a pre-269 install had github_app_installation_id
// or github_pat_user_id set to anything non-NULL, the backfill
// UPDATE must NOT clobber it. The migration gates on both fields
// being NULL exactly for this reason.
func TestMigrate_SKY269PreservesExistingPATConfig(t *testing.T) {
	database := openMigrationsTestDB(t)
	migrateUpTo(t, database, 202605120002)
	// Stage: agents row with github_app_installation_id set — a user
	// who'd configured an App install pre-269. (Hypothetical pre-269
	// because SKY-260 didn't ship a UI for this, but defending in
	// depth against any DB that has it.)
	if _, err := database.Exec(`
		INSERT INTO agents (id, display_name, github_app_installation_id)
		VALUES ('pre-269-with-app', 'Triage Factory Bot', '12345')
	`); err != nil {
		t.Fatalf("seed pre-269 agents row: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO team_agents (team_id, agent_id, enabled)
		VALUES ('default', 'pre-269-with-app', 1)
	`); err != nil {
		t.Fatalf("seed pre-269 team_agents row: %v", err)
	}

	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var installID sql.NullString
	var patUserID sql.NullString
	if err := database.QueryRow(
		`SELECT github_app_installation_id, github_pat_user_id FROM agents WHERE org_id = ?`,
		runmode.LocalDefaultOrgID,
	).Scan(&installID, &patUserID); err != nil {
		t.Fatalf("read post-migration agents row: %v", err)
	}
	if !installID.Valid || installID.String != "12345" {
		t.Errorf("github_app_installation_id = %v; want preserved as '12345'", installID)
	}
	if patUserID.Valid {
		t.Errorf("github_pat_user_id = %q; want NULL (App was already set, backfill must skip)", patUserID.String)
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
