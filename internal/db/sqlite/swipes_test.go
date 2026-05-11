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

// TestSwipeStore_SQLite runs the shared conformance suite against
// the SQLite SwipeStore impl. Each subtest opens a fresh in-memory
// DB so swipe_events state doesn't leak between assertions.
func TestSwipeStore_SQLite(t *testing.T) {
	dbtest.RunSwipeStoreConformance(t, func(t *testing.T) (db.SwipeStore, string, dbtest.TaskSeederForSwipes, dbtest.TaskReaderForSwipes) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)

		seed := func(t *testing.T) string {
			t.Helper()
			return seedSQLiteTaskForSwipes(t, conn)
		}
		read := func(t *testing.T, taskID string) (string, time.Time) {
			t.Helper()
			return readSQLiteTask(t, conn, taskID)
		}
		return stores.Swipes, runmode.LocalDefaultOrg, seed, read
	})
}

// seedSQLiteTaskForSwipes creates an entity + event + task row for
// the swipe conformance suite to swipe against. Returns the task ID.
func seedSQLiteTaskForSwipes(t *testing.T, conn *sql.DB) string {
	t.Helper()
	now := time.Now().UTC()
	entityID := uuid.New().String()
	taskID := uuid.New().String()
	eventID := uuid.New().String()
	sourceID := fmt.Sprintf("swipe-conformance-%d", now.UnixNano())

	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES (?, 'github', ?, 'pr', 'Swipe Conformance', 'https://example/x', '{}', ?)
	`, entityID, sourceID, now); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES (?, ?, 'github:pr:opened', '', '{}', ?)
	`, eventID, entityID, now); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status, created_at)
		VALUES (?, ?, 'github:pr:opened', '', ?, 'queued', 'pending', ?)
	`, taskID, entityID, eventID, now); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return taskID
}

// readSQLiteTask returns status + snooze_until for the harness's
// post-swipe assertions. snooze_until parses from SQLite's text
// timestamp; zero time means NULL.
func readSQLiteTask(t *testing.T, conn *sql.DB, taskID string) (string, time.Time) {
	t.Helper()
	var status string
	var snoozeUntil sql.NullTime
	err := conn.QueryRow(`SELECT status, snooze_until FROM tasks WHERE id = ?`, taskID).Scan(&status, &snoozeUntil)
	if err != nil {
		t.Fatalf("readSQLiteTask %s: %v", taskID, err)
	}
	if snoozeUntil.Valid {
		return status, snoozeUntil.Time
	}
	return status, time.Time{}
}
