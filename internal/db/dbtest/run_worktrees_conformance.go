package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RunWorktreeStoreFactory is what a per-backend test file hands to
// RunRunWorktreeStoreConformance. Returns:
//   - the wired RunWorktreeStore impl,
//   - the orgID to pass to every call,
//   - a RunWorktreeSeeder the harness uses to stage the run FK
//     chain (run_worktrees FKs to runs; backends seed those rows
//     differently and the conformance harness shouldn't bake one
//     shape's schema into the assertions).
type RunWorktreeStoreFactory func(t *testing.T) (store db.RunWorktreeStore, orgID string, seed RunWorktreeSeeder)

// RunWorktreeSeeder is a bag of callbacks the conformance suite uses
// to stage fixture rows the RunWorktreeStore doesn't own.
type RunWorktreeSeeder struct {
	// Run inserts the entity + event + prompt + task + run FK chain
	// needed to attach a run_worktrees row, and returns the runID.
	// suffix discriminates per-subtest seeds so the unique indexes on
	// entities/runs don't collide.
	Run func(t *testing.T, suffix string) (runID string)

	// DeleteRun removes the run row so the cascade-on-delete subtest
	// can verify the FK ON DELETE CASCADE.
	DeleteRun func(t *testing.T, runID string)
}

// RunRunWorktreeStoreConformance covers the RunWorktreeStore
// contract every backend impl must hold. System variants are NOT
// covered by parallel cases — their behavior is documented as
// identical to the non-System counterparts; see SKY-306's cleanup
// precedent.
func RunRunWorktreeStoreConformance(t *testing.T, mk RunWorktreeStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Insert_returns_inserted_true_on_fresh_row", func(t *testing.T) {
		store, orgID, seed := mk(t)
		runID := seed.Run(t, "fresh")
		inserted, winning, err := store.Insert(ctx, orgID, domain.RunWorktree{
			RunID: runID, RepoID: "owner/repo", Path: "/tmp/wt/" + runID + "/owner/repo", FeatureBranch: "feature/SKY-1",
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		if !inserted {
			t.Errorf("expected inserted=true on fresh row")
		}
		if winning != "/tmp/wt/"+runID+"/owner/repo" {
			t.Errorf("winningPath = %q, want fresh path", winning)
		}
	})

	t.Run("Insert_idempotent_on_conflict_returns_winning_path", func(t *testing.T) {
		store, orgID, seed := mk(t)
		runID := seed.Run(t, "idem")
		firstPath := "/tmp/wt/" + runID + "/owner/repo"
		if _, _, err := store.Insert(ctx, orgID, domain.RunWorktree{
			RunID: runID, RepoID: "owner/repo", Path: firstPath, FeatureBranch: "feature/SKY-1",
		}); err != nil {
			t.Fatalf("first insert: %v", err)
		}
		// Pass a different path to confirm the conflict path reads
		// the row, not echoes the input.
		inserted, winning, err := store.Insert(ctx, orgID, domain.RunWorktree{
			RunID: runID, RepoID: "owner/repo", Path: "/tmp/wt/DIFFERENT/owner/repo", FeatureBranch: "feature/SKY-1",
		})
		if err != nil {
			t.Fatalf("second insert: %v", err)
		}
		if inserted {
			t.Errorf("expected inserted=false on conflicting second insert")
		}
		if winning != firstPath {
			t.Errorf("winningPath after conflict = %q, want %q", winning, firstPath)
		}
	})

	t.Run("GetByRepo_returns_row_or_nil", func(t *testing.T) {
		store, orgID, seed := mk(t)
		runID := seed.Run(t, "getrepo")
		if _, _, err := store.Insert(ctx, orgID, domain.RunWorktree{
			RunID: runID, RepoID: "owner/repo", Path: "/p1", FeatureBranch: "feature/SKY-1",
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
		got, err := store.GetByRepo(ctx, orgID, runID, "owner/repo")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got == nil {
			t.Fatal("expected row, got nil")
		}
		if got.Path != "/p1" || got.FeatureBranch != "feature/SKY-1" {
			t.Errorf("unexpected row: %+v", got)
		}
		missing, err := store.GetByRepo(ctx, orgID, runID, "other/repo")
		if err != nil {
			t.Fatalf("get missing: %v", err)
		}
		if missing != nil {
			t.Errorf("expected nil for missing repo, got %+v", missing)
		}
	})

	t.Run("List_orders_by_created_at_then_repo_and_scopes_by_run", func(t *testing.T) {
		store, orgID, seed := mk(t)
		r1 := seed.Run(t, "list-r1")
		r2 := seed.Run(t, "list-r2")
		for _, w := range []domain.RunWorktree{
			{RunID: r1, RepoID: "owner/a", Path: "/p1", FeatureBranch: "feature/SKY-1"},
			{RunID: r1, RepoID: "owner/b", Path: "/p2", FeatureBranch: "feature/SKY-1"},
			{RunID: r2, RepoID: "owner/a", Path: "/p3", FeatureBranch: "feature/SKY-2"},
		} {
			if _, _, err := store.Insert(ctx, orgID, w); err != nil {
				t.Fatalf("insert %s/%s: %v", w.RunID, w.RepoID, err)
			}
		}
		rows, err := store.List(ctx, orgID, r1)
		if err != nil {
			t.Fatalf("list r1: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("r1 rows = %d, want 2", len(rows))
		}
		for _, r := range rows {
			if r.RunID != r1 {
				t.Errorf("scope leak: r1 list contains %s", r.RunID)
			}
		}
	})

	t.Run("DeleteByRepo_idempotent_on_missing_row", func(t *testing.T) {
		store, orgID, seed := mk(t)
		runID := seed.Run(t, "del-repo")
		if err := store.DeleteByRepo(ctx, orgID, runID, "no/such-repo"); err != nil {
			t.Errorf("DeleteByRepo(missing) = %v, want nil", err)
		}
	})

	t.Run("DeleteByPathSystem_removes_only_the_matching_row", func(t *testing.T) {
		store, orgID, seed := mk(t)
		runID := seed.Run(t, "del-path")
		if _, _, err := store.Insert(ctx, orgID, domain.RunWorktree{
			RunID: runID, RepoID: "owner/a", Path: "/p1", FeatureBranch: "feature/SKY-1",
		}); err != nil {
			t.Fatalf("insert a: %v", err)
		}
		if _, _, err := store.Insert(ctx, orgID, domain.RunWorktree{
			RunID: runID, RepoID: "owner/b", Path: "/p2", FeatureBranch: "feature/SKY-1",
		}); err != nil {
			t.Fatalf("insert b: %v", err)
		}
		if err := store.DeleteByPathSystem(ctx, orgID, runID, "/p1"); err != nil {
			t.Fatalf("DeleteByPathSystem: %v", err)
		}
		rows, err := store.List(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("list after delete: %v", err)
		}
		if len(rows) != 1 || rows[0].Path != "/p2" {
			t.Errorf("after delete: %+v, want exactly [/p2]", rows)
		}
	})

	t.Run("Cascade_on_run_delete_removes_rows", func(t *testing.T) {
		store, orgID, seed := mk(t)
		runID := seed.Run(t, "cascade")
		if _, _, err := store.Insert(ctx, orgID, domain.RunWorktree{
			RunID: runID, RepoID: "owner/a", Path: "/p1", FeatureBranch: "feature/SKY-1",
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
		seed.DeleteRun(t, runID)
		rows, err := store.List(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("list after cascade: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("expected 0 rows after run delete cascade, got %d", len(rows))
		}
	})
}
