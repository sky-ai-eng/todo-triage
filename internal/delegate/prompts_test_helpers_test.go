package delegate

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
// fixtures. Tests pass this to NewSpawner so prompt-loading code
// paths exercise the same store interface production uses. Returns
// nil when database is nil (used by smoke tests that don't touch
// any DB).
func testPromptStore(database *sql.DB) db.PromptStore {
	if database == nil {
		return nil
	}
	return sqlitestore.New(database).Prompts
}

// ensureTestPrompt is the FindOrCreate replacement for the deleted
// db.GetPrompt + db.CreatePrompt pair tests used to seed a prompt
// row for run fixtures. Idempotent: subsequent calls on the same
// ID are no-ops.
func ensureTestPrompt(t *testing.T, database *sql.DB, p domain.Prompt) {
	t.Helper()
	store := testPromptStore(database)
	ctx := context.Background()
	existing, _ := store.Get(ctx, runmode.LocalDefaultOrg, p.ID)
	if existing != nil {
		return
	}
	if err := store.Create(ctx, runmode.LocalDefaultOrg, p); err != nil {
		t.Fatalf("ensureTestPrompt %s: %v", p.ID, err)
	}
}
