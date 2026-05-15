package sqlite_test

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestEntityStore_SQLite runs the shared conformance suite against the
// SQLite EntityStore impl. Each subtest opens a fresh in-memory DB so
// lifecycle assertions don't bleed across.
func TestEntityStore_SQLite(t *testing.T) {
	dbtest.RunEntityStoreConformance(t, func(t *testing.T) (db.EntityStore, string, dbtest.EntitySeeder) {
		t.Helper()
		conn := newSQLiteForEntityTest(t)
		seed := newSQLiteEntitySeeder(conn)
		stores := sqlitestore.New(conn)
		return stores.Entities, runmode.LocalDefaultOrgID, seed
	})
}

// TestEntityStore_SQLite_RejectsNonLocalOrg pins the SQLite-side
// assertLocalOrg guard: a multi-mode caller that drifts through with a
// real-looking org UUID must fail loudly rather than silently reading
// the lone local row.
func TestEntityStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := newSQLiteForEntityTest(t)
	stores := sqlitestore.New(conn)

	const bogusOrg = "11111111-1111-1111-1111-111111111111"
	if _, _, err := stores.Entities.FindOrCreate(t.Context(), bogusOrg, "github", "owner/repo#1", "pr", "T", ""); err == nil {
		t.Errorf("expected error for non-local orgID, got nil")
	}
	if _, err := stores.Entities.Get(t.Context(), bogusOrg, "any-id"); err == nil {
		t.Errorf("expected error for non-local orgID on Get, got nil")
	}
}

func newSQLiteForEntityTest(t *testing.T) *sql.DB {
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
	return conn
}

func newSQLiteEntitySeeder(conn *sql.DB) dbtest.EntitySeeder {
	return dbtest.EntitySeeder{
		Project: func(t *testing.T, name string) string {
			t.Helper()
			pid, err := db.CreateProject(conn, domain.Project{Name: name})
			if err != nil {
				t.Fatalf("seed project %s: %v", name, err)
			}
			return pid
		},
	}
}
