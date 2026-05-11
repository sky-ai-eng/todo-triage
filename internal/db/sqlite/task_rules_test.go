package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTaskRuleStore_SQLite runs the shared TaskRuleStore conformance
// suite against the SQLite impl. Each subtest opens a fresh in-memory
// DB so seeded-vs-user-created row state doesn't leak.
func TestTaskRuleStore_SQLite(t *testing.T) {
	dbtest.RunTaskRuleStoreConformance(t, func(t *testing.T) (db.TaskRuleStore, string) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		return stores.TaskRules, runmode.LocalDefaultOrg
	})
}

// TestTaskRuleStore_SQLite_AssertsLocalOrg pins the local-org guard:
// any orgID other than runmode.LocalDefaultOrg must fail loudly.
func TestTaskRuleStore_SQLite_AssertsLocalOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	if _, err := stores.TaskRules.List(t.Context(), "some-real-uuid"); err == nil {
		t.Fatal("List accepted non-local orgID; should reject")
	}
}
