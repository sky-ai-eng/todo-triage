package db

import (
	"database/sql"
	"testing"

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
