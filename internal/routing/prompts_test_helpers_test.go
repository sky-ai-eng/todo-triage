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
