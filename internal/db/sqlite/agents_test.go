package sqlite_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestAgentStore_SQLite runs the shared AgentStore conformance suite
// against the SQLite impl. Each subtest opens a fresh in-memory DB
// so row state doesn't leak across cases. patUserID is an arbitrary
// fresh UUID — SQLite has no FK to a users table so the value just
// has to be UUID-shaped to clear the SetGitHubPATUser input check.
func TestAgentStore_SQLite(t *testing.T) {
	dbtest.RunAgentStoreConformance(t, func(t *testing.T) (db.AgentStore, string, string) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		return stores.Agents, runmode.LocalDefaultOrg, uuid.New().String()
	})
}

// TestAgentStore_SQLite_AssertsLocalOrg pins the local-org guard:
// any orgID other than runmode.LocalDefaultOrg must fail loudly.
func TestAgentStore_SQLite_AssertsLocalOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	if _, err := stores.Agents.GetForOrg(t.Context(), "some-real-uuid"); err == nil {
		t.Fatal("GetForOrg accepted non-local orgID; should reject")
	}
}
