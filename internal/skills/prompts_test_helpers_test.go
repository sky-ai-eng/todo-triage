package skills

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
// fixtures so ImportAll exercises the same store interface
// production uses.
func testPromptStore(database *sql.DB) db.PromptStore {
	if database == nil {
		return nil
	}
	return sqlitestore.New(database).Prompts
}

// testTriggerStore mirrors testPromptStore for the trigger-touching
// tests after the db.SavePromptTrigger / db.GetPromptTrigger free-
// functions moved into TriggerStore (SKY-246 wave 1).
func testTriggerStore(database *sql.DB) db.TriggerStore {
	if database == nil {
		return nil
	}
	return sqlitestore.New(database).Triggers
}

// createTriggerForTestSkills + getTriggerForTestSkills are the
// raw-SQL replacements for the deleted db.SavePromptTrigger +
// db.GetPromptTrigger free-functions used by importer_test.go.
func createTriggerForTestSkills(t *testing.T, database *sql.DB, trig domain.PromptTrigger) {
	t.Helper()
	if err := testTriggerStore(database).Create(context.Background(), runmode.LocalDefaultOrg, trig); err != nil {
		t.Fatalf("createTriggerForTestSkills %s: %v", trig.ID, err)
	}
}

func getTriggerForTestSkills(t *testing.T, database *sql.DB, id string) *domain.PromptTrigger {
	t.Helper()
	got, err := testTriggerStore(database).Get(context.Background(), runmode.LocalDefaultOrg, id)
	if err != nil {
		t.Fatalf("getTriggerForTestSkills %s: %v", id, err)
	}
	return got
}
