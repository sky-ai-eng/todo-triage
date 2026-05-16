package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTaskMemoryStore_SQLite runs the shared conformance suite
// against the SQLite TaskMemoryStore impl. Each subtest opens a
// fresh in-memory DB so run_memory state doesn't leak between
// assertions.
func TestTaskMemoryStore_SQLite(t *testing.T) {
	dbtest.RunTaskMemoryStoreConformance(t, func(t *testing.T) (db.TaskMemoryStore, string, dbtest.TaskMemorySeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		seed := dbtest.TaskMemorySeeder{
			Run: func(t *testing.T, suffix string) (runID, entityID string) {
				t.Helper()
				return seedSQLiteRunForTaskMemory(t, conn, suffix)
			},
		}
		return stores.TaskMemory, runmode.LocalDefaultOrg, seed
	})
}

// TestTaskMemoryStore_SQLite_RejectsNonLocalOrg pins the
// assertLocalOrg gate at every method entry. Non-default org IDs
// indicate a confused caller (they think they're in multi mode) and
// must be rejected loudly so the silent default-org fallthrough
// never happens.
func TestTaskMemoryStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	const badOrg = "11111111-1111-1111-1111-111111111111"
	if err := stores.TaskMemory.UpsertAgentMemory(ctx, badOrg, "r", "e", "x"); err == nil {
		t.Error("UpsertAgentMemory(non-local org) should error")
	}
	if err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, badOrg, "r", "e", "x"); err == nil {
		t.Error("UpsertAgentMemorySystem(non-local org) should error")
	}
	if err := stores.TaskMemory.UpdateRunMemoryHumanContent(ctx, badOrg, "r", "x"); err == nil {
		t.Error("UpdateRunMemoryHumanContent(non-local org) should error")
	}
	if _, err := stores.TaskMemory.GetMemoriesForEntity(ctx, badOrg, "e"); err == nil {
		t.Error("GetMemoriesForEntity(non-local org) should error")
	}
	if _, err := stores.TaskMemory.GetMemoriesForEntitySystem(ctx, badOrg, "e"); err == nil {
		t.Error("GetMemoriesForEntitySystem(non-local org) should error")
	}
	if _, err := stores.TaskMemory.GetRunMemory(ctx, badOrg, "r"); err == nil {
		t.Error("GetRunMemory(non-local org) should error")
	}
}

// seedSQLiteRunForTaskMemory seeds the entity + event + prompt +
// task + run FK chain run_memory needs. Direct INSERTs keep the
// fixture path schema-coupled and short — matches the SwipeStore
// / EventStore conformance seed pattern.
func seedSQLiteRunForTaskMemory(t *testing.T, conn *sql.DB, suffix string) (runID, entityID string) {
	t.Helper()
	entityID = uuid.New().String()
	now := time.Now().UTC()
	sourceID := fmt.Sprintf("task-memory-%s-%d", suffix, now.UnixNano())
	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at, state)
		VALUES (?, 'github', ?, 'pr', 'Task Memory Conformance', 'https://example/x', '{}', ?, 'active')
	`, entityID, sourceID, now); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	eventID := uuid.New().String()
	const eventType = "github:pr:opened"
	if _, err := conn.Exec(`
		INSERT INTO events (id, entity_id, event_type, dedup_key)
		VALUES (?, ?, ?, '')
	`, eventID, entityID, eventType); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT OR IGNORE INTO prompts (id, name, body, creator_user_id, team_id) VALUES ('p_task_memory', 'TaskMemory', 'body', ?, ?)
	`, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	taskID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		VALUES (?, ?, ?, ?, 'completed')
	`, taskID, entityID, eventType, eventID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	runID = uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO runs (id, task_id, prompt_id, status) VALUES (?, ?, 'p_task_memory', 'completed')
	`, runID, taskID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return runID, entityID
}
