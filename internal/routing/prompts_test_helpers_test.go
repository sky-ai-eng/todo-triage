package routing

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// testPromptStore returns a real SQLite-backed PromptStore for test
// fixtures. Tests pass this to NewRouter so prompt-loading code
// paths exercise the same store interface production uses.
func testPromptStore(database *sql.DB) db.PromptStore {
	if database == nil {
		return nil
	}
	return sqlitestore.New(database).Prompts
}

// testTaskRuleStore returns a SQLite-backed TaskRuleStore for routing
// tests. Mirrors testPromptStore — the router takes a TaskRuleStore
// argument now that GetEnabledRulesForEvent moved off raw db.* and
// onto the store interface.
func testTaskRuleStore(database *sql.DB) db.TaskRuleStore {
	if database == nil {
		return nil
	}
	return sqlitestore.New(database).TaskRules
}

// testTriggerStore returns a SQLite-backed TriggerStore for routing
// tests. Same pattern — the router now reads triggers through the
// store interface, so tests construct it the same way.
func testTriggerStore(database *sql.DB) db.TriggerStore {
	if database == nil {
		return nil
	}
	return sqlitestore.New(database).Triggers
}

// createTriggerForTestRouting + setTriggerEnabledForTestRouting are
// the raw-SQL replacements for db.SavePromptTrigger /
// db.SetTriggerEnabled used by drain_test + rederive_test. They use
// the store interface to keep the seed path consistent with what
// production uses.
func createTriggerForTestRouting(t *testing.T, database *sql.DB, trig domain.PromptTrigger) {
	t.Helper()
	if err := testTriggerStore(database).Create(context.Background(), runmode.LocalDefaultOrg, trig); err != nil {
		t.Fatalf("createTriggerForTestRouting %s: %v", trig.ID, err)
	}
}

func setTriggerEnabledForTestRouting(t *testing.T, database *sql.DB, id string, enabled bool) {
	t.Helper()
	if err := testTriggerStore(database).SetEnabled(context.Background(), runmode.LocalDefaultOrg, id, enabled); err != nil {
		t.Fatalf("setTriggerEnabledForTestRouting %s: %v", id, err)
	}
}

// createTestPrompt is the replacement for the deleted db.CreatePrompt
// free-function. Routes through the store so test fixtures and
// production share the same insert SQL.
func createTestPrompt(t *testing.T, database *sql.DB, p domain.Prompt) {
	t.Helper()
	store := testPromptStore(database)
	if err := store.Create(context.Background(), runmode.LocalDefaultOrg, p); err != nil {
		t.Fatalf("createTestPrompt %s: %v", p.ID, err)
	}
}
