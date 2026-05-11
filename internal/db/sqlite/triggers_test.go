package sqlite_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTriggerStore_SQLite runs the shared conformance suite against
// the SQLite TriggerStore impl. Each subtest opens a fresh in-memory
// DB so seeded-vs-user state doesn't leak.
func TestTriggerStore_SQLite(t *testing.T) {
	dbtest.RunTriggerStoreConformance(t, func(t *testing.T) (db.TriggerStore, string, dbtest.PromptSeederForTriggers) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		seedPrompts := func(t *testing.T) {
			t.Helper()
			seedSQLitePromptsForTriggers(t, conn, stores.Prompts)
		}
		return stores.Triggers, runmode.LocalDefaultOrg, seedPrompts
	})
}

// TestTriggerStore_SQLite_AssertsLocalOrg pins the local-org guard:
// passing anything other than runmode.LocalDefaultOrg must fail.
func TestTriggerStore_SQLite_AssertsLocalOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	if _, err := stores.Triggers.List(t.Context(), "some-real-uuid"); err == nil {
		t.Fatal("List accepted non-local orgID; should reject")
	}
}

// seedSQLitePromptsForTriggers creates the prompts referenced by
// the shipped triggers via PromptStore so the FK on
// prompt_triggers.prompt_id resolves. Idempotent — call SeedOrUpdate
// for each shipped prompt id; re-calls are no-ops via the version
// sidecar.
func seedSQLitePromptsForTriggers(t *testing.T, _ *sql.DB, prompts db.PromptStore) {
	t.Helper()
	ctx := context.Background()
	// Minimal set covering every PromptID referenced by ShippedPromptTriggers.
	for _, p := range []domain.Prompt{
		{ID: "system-ci-fix", Name: "CI Fix", Body: "x", Source: "system"},
		{ID: "system-conflict-resolution", Name: "Conflict", Body: "x", Source: "system"},
		{ID: "system-jira-implement", Name: "Jira", Body: "x", Source: "system"},
		{ID: "system-pr-review", Name: "PR Review", Body: "x", Source: "system"},
		{ID: "system-fix-review-feedback", Name: "Fix Review", Body: "x", Source: "system"},
	} {
		if err := prompts.SeedOrUpdate(ctx, runmode.LocalDefaultOrg, p); err != nil {
			t.Fatalf("seed prompt %s: %v", p.ID, err)
		}
	}
}
