package db

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	_ "modernc.org/sqlite"
)

// newTestDB spins up an in-memory SQLite database with the full schema + seed
// so the package's CRUD tests run against a realistic FK graph (entities,
// events_catalog, task_rules constraints). Each test gets its own isolated DB.
//
// Lives in its own file so the per-store *_test.go files can share it
// without one of them owning the helper.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite memory: %v", err)
	}
	// Force single connection — SQLite :memory: is per-connection, so a
	// pooled second connection would get a blank database without the
	// schema from BootstrapSchemaForTest.
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })

	if err := BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return database
}

// makeEntity inserts a fresh active GitHub PR entity for tests. The
// (source, source_id) pair must be unique per test run; the i argument
// gives a stable per-test-row discriminator. Shared by lifetime_counter
// and events tests after factory_test.go was retired in SKY-291.
func makeEntity(t *testing.T, database *sql.DB, i int) *domain.Entity {
	t.Helper()
	e, _, err := FindOrCreateEntity(
		database, "github", fmt.Sprintf("owner/repo#%d", i), "pr",
		fmt.Sprintf("PR %d", i), fmt.Sprintf("https://github.com/owner/repo/pull/%d", i),
	)
	if err != nil {
		t.Fatalf("FindOrCreateEntity %d: %v", i, err)
	}
	return e
}

// recordEvent inserts a real entity-attached event for tests. Returns
// the event's UUID. Wraps RecordEvent to centralize the t.Fatalf on
// errors. Shared by lifetime_counter and events tests after
// factory_test.go was retired in SKY-291.
func recordEvent(t *testing.T, database *sql.DB, entityID, eventType string) string {
	t.Helper()
	id, err := RecordEvent(database, domain.Event{EntityID: &entityID, EventType: eventType})
	if err != nil {
		t.Fatalf("RecordEvent(%s, %s): %v", entityID, eventType, err)
	}
	return id
}
