package delegate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestMaterializePriorMemories_CreatesDirEvenWithNoPriors guards the
// invariant the prompt + memory-gate retry both depend on: the agent's
// initial cwd has a _scratch/entity-memory/ directory it can ls and
// write into, regardless of whether prior runs ever existed for this
// entity.
//
// Pre-fix, the function early-returned when len(memories)==0, so a
// first-run agent saw a missing directory and either (a) failed `ls
// _scratch/entity-memory/` outright or (b) failed the final memory
// write because the parent dir didn't exist.
func TestMaterializePriorMemories_CreatesDirEvenWithNoPriors(t *testing.T) {
	database := newTakeoverTestDB(t)
	cwd := t.TempDir()

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "jira", "SKY-100", "issue", "T", "https://x/100")
	if err != nil {
		t.Fatalf("entity: %v", err)
	}

	// Sanity: no memories for this entity yet.
	mems, err := db.GetMemoriesForEntity(database, entity.ID)
	if err != nil {
		t.Fatalf("GetMemoriesForEntity: %v", err)
	}
	if len(mems) != 0 {
		t.Fatalf("expected 0 priors for new entity, got %d", len(mems))
	}

	materializePriorMemories(database, cwd, entity.ID)

	memDir := filepath.Join(cwd, "_scratch", "entity-memory")
	info, err := os.Stat(memDir)
	if err != nil {
		t.Fatalf("entity-memory dir not created at %s: %v", memDir, err)
	}
	if !info.IsDir() {
		t.Fatalf("entity-memory exists but is not a directory")
	}

	// And it's empty (no prior memory files materialized).
	entries, err := os.ReadDir(memDir)
	if err != nil {
		t.Fatalf("read entity-memory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty entity-memory dir, found %d entries", len(entries))
	}
}

// TestMaterializePriorMemories_WritesPriors verifies the existing
// happy-path behavior survives the mkdir-first refactor.
func TestMaterializePriorMemories_WritesPriors(t *testing.T) {
	database := newTakeoverTestDB(t)
	cwd := t.TempDir()

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "jira", "SKY-200", "issue", "T", "https://x/200")
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	// Seed task + run + memory chain so GetMemoriesForEntity returns one row.
	evt, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrg, domain.Event{
		EventType: domain.EventJiraIssueAssigned, EntityID: &entity.ID, MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	task, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, entity.ID, domain.EventJiraIssueAssigned, "", evt, 0.5)
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	ensureTestPrompt(t, database, domain.Prompt{ID: "p1", Name: "T", Body: "x", Source: "user"})
	if err := sqlitestore.New(database).AgentRuns.Create(t.Context(), runmode.LocalDefaultOrg, domain.AgentRun{
		ID: "prior-run", TaskID: task.ID, PromptID: "p1", Status: "completed", Model: "m",
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := db.UpsertAgentMemory(database, "prior-run", entity.ID, "what i did last time"); err != nil {
		t.Fatalf("upsert memory: %v", err)
	}

	materializePriorMemories(database, cwd, entity.ID)

	priorPath := filepath.Join(cwd, "_scratch", "entity-memory", "prior-run.md")
	body, err := os.ReadFile(priorPath)
	if err != nil {
		t.Fatalf("expected materialized prior at %s: %v", priorPath, err)
	}
	if string(body) != "what i did last time" {
		t.Errorf("prior content = %q, want %q", string(body), "what i did last time")
	}
}
