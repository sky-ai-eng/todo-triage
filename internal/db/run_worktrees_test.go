package db

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// seedJiraRun creates the entity → event → task → prompt → run chain
// needed for run_worktrees FK satisfaction. The run row IS the prerequisite
// — InsertRunWorktree's FK is on runs(id) ON DELETE CASCADE.
func seedJiraRun(t *testing.T, database *sql.DB, runID string) {
	t.Helper()
	entity, _, err := FindOrCreateEntity(database, "jira", "SKY-"+runID, "issue", "T-"+runID, "https://x/"+runID)
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	evt, err := RecordEvent(database, domain.Event{
		EventType:    domain.EventJiraIssueAssigned,
		EntityID:     &entity.ID,
		MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	task, _, err := FindOrCreateTask(database, entity.ID, domain.EventJiraIssueAssigned, runID, evt, 0.5)
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	createPromptForTest(t, database, domain.Prompt{ID: "p-" + runID, Name: "T", Body: "x", Source: "user"})
	if err := CreateAgentRun(database, domain.AgentRun{
		ID: runID, TaskID: task.ID, PromptID: "p-" + runID,
		Status: "running", Model: "m",
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestInsertRunWorktree_Idempotent(t *testing.T) {
	database := newTestDB(t)
	seedJiraRun(t, database, "r1")

	inserted, winning, err := InsertRunWorktree(database, RunWorktree{
		RunID: "r1", RepoID: "owner/repo", Path: "/tmp/wt/r1/owner/repo", FeatureBranch: "feature/SKY-1",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if !inserted {
		t.Errorf("expected inserted=true on first insert")
	}
	if winning != "/tmp/wt/r1/owner/repo" {
		t.Errorf("winningPath = %q, want /tmp/wt/r1/owner/repo", winning)
	}

	// Idempotent re-insert returns the existing (winning) path with inserted=false.
	// Caller passes a different path to confirm we read the row, not echo the input.
	inserted, winning, err = InsertRunWorktree(database, RunWorktree{
		RunID: "r1", RepoID: "owner/repo", Path: "/tmp/wt/r1-DIFFERENT/owner/repo", FeatureBranch: "feature/SKY-1",
	})
	if err != nil {
		t.Fatalf("insert second: %v", err)
	}
	if inserted {
		t.Errorf("expected inserted=false on conflicting second insert")
	}
	if winning != "/tmp/wt/r1/owner/repo" {
		t.Errorf("winningPath after conflict = %q, want the original /tmp/wt/r1/owner/repo", winning)
	}
}

func TestGetRunWorktreeByRepo(t *testing.T) {
	database := newTestDB(t)
	seedJiraRun(t, database, "r1")

	if _, _, err := InsertRunWorktree(database, RunWorktree{
		RunID: "r1", RepoID: "owner/repo", Path: "/p1", FeatureBranch: "feature/SKY-1",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := GetRunWorktreeByRepo(database, "r1", "owner/repo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("expected row, got nil")
	}
	if got.Path != "/p1" || got.FeatureBranch != "feature/SKY-1" {
		t.Errorf("unexpected row: %+v", got)
	}

	missing, err := GetRunWorktreeByRepo(database, "r1", "other/repo")
	if err != nil {
		t.Fatalf("get missing: %v", err)
	}
	if missing != nil {
		t.Errorf("expected nil for missing repo, got %+v", missing)
	}
}

func TestGetRunWorktrees_OrderAndScope(t *testing.T) {
	database := newTestDB(t)
	seedJiraRun(t, database, "r1")
	seedJiraRun(t, database, "r2")

	for _, w := range []RunWorktree{
		{RunID: "r1", RepoID: "owner/a", Path: "/p1", FeatureBranch: "feature/SKY-1"},
		{RunID: "r1", RepoID: "owner/b", Path: "/p2", FeatureBranch: "feature/SKY-1"},
		{RunID: "r2", RepoID: "owner/a", Path: "/p3", FeatureBranch: "feature/SKY-2"},
	} {
		if _, _, err := InsertRunWorktree(database, w); err != nil {
			t.Fatalf("insert %s: %v", w.RepoID, err)
		}
	}

	rows, err := GetRunWorktrees(database, "r1")
	if err != nil {
		t.Fatalf("list r1: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("r1 rows = %d, want 2", len(rows))
	}
	if rows[0].RepoID != "owner/a" || rows[1].RepoID != "owner/b" {
		t.Errorf("order: %+v", rows)
	}
	for _, r := range rows {
		if r.RunID != "r1" {
			t.Errorf("scope leak: r1 list contains %s", r.RunID)
		}
	}
}

func TestRunWorktrees_CascadeOnRunDelete(t *testing.T) {
	database := newTestDB(t)
	seedJiraRun(t, database, "r1")

	if _, _, err := InsertRunWorktree(database, RunWorktree{
		RunID: "r1", RepoID: "owner/a", Path: "/p1", FeatureBranch: "feature/SKY-1",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if _, err := database.Exec(`DELETE FROM runs WHERE id = ?`, "r1"); err != nil {
		t.Fatalf("delete run: %v", err)
	}

	rows, err := GetRunWorktrees(database, "r1")
	if err != nil {
		t.Fatalf("list after cascade: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows after run delete cascade, got %d", len(rows))
	}
}
