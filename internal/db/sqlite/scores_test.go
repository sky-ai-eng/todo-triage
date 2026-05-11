package sqlite_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestScoreStore_SQLite runs the shared conformance suite against the
// SQLite ScoreStore impl. The factory opens a fresh in-memory DB per
// test, bootstraps the schema, and supplies a seeder that creates
// queued+pending task rows the harness asserts against. See
// internal/db/dbtest for the assertion bodies.
func TestScoreStore_SQLite(t *testing.T) {
	dbtest.RunScoreStoreConformance(t, func(t *testing.T) (db.ScoreStore, string, dbtest.ScoreSeeder) {
		t.Helper()
		conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
		if err != nil {
			t.Fatalf("open in-memory db: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		conn.SetMaxOpenConns(1)
		conn.SetMaxIdleConns(1)

		if err := db.BootstrapSchemaForTest(conn); err != nil {
			t.Fatalf("bootstrap schema: %v", err)
		}

		stores := sqlitestore.New(conn)
		seeder := func(t *testing.T, n int) []string {
			t.Helper()
			return seedSQLiteTasks(t, conn, n)
		}
		return stores.Scores, runmode.LocalDefaultOrg, seeder
	})
}

// seedSQLiteTasks inserts n rows of (entity + task) directly via raw
// SQL. TaskStore hasn't migrated yet (wave 3a), so the seeder owns
// schema knowledge — the conformance harness is intentionally
// schema-blind. When TaskStore lands this collapses into a
// stores.Tasks.FindOrCreate call.
func seedSQLiteTasks(t *testing.T, conn *sql.DB, n int) []string {
	t.Helper()
	now := time.Now().UTC()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		entityID := uuid.New().String()
		taskID := uuid.New().String()
		eventID := uuid.New().String()
		sourceID := fmt.Sprintf("conformance-pr-%d-%d", i, now.UnixNano())
		// events_catalog must include the event_type before the FK
		// fires. The bootstrap seed includes the standard catalog;
		// "github:pr:opened" is a stable entry that matches
		// domain.EventGitHubPROpened.
		eventType := "github:pr:opened"

		if _, err := conn.Exec(`
			INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at)
			VALUES (?, 'github', ?, 'pr', ?, ?, '{}', ?)
		`, entityID, sourceID, fmt.Sprintf("Conformance PR %d", i), "https://example/pr/"+sourceID, now); err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		if _, err := conn.Exec(`
			INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at)
			VALUES (?, ?, ?, '', '{}', ?)
		`, eventID, entityID, eventType, now); err != nil {
			t.Fatalf("seed event: %v", err)
		}
		if _, err := conn.Exec(`
			INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id,
			                   status, scoring_status, created_at)
			VALUES (?, ?, ?, '', ?, 'queued', 'pending', ?)
		`, taskID, entityID, eventType, eventID, now); err != nil {
			t.Fatalf("seed task: %v", err)
		}
		ids = append(ids, taskID)
	}
	return ids
}
