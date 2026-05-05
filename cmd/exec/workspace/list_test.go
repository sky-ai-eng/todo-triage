package workspace

import (
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

func TestListWorkspaces_MissingRunID(t *testing.T) {
	database := newTestDB(t)
	if _, err := listWorkspaces(database, ""); !errors.Is(err, errMissingRunID) {
		t.Errorf("err = %v, want errMissingRunID", err)
	}
}

func TestListWorkspaces_RunNotFound(t *testing.T) {
	database := newTestDB(t)
	if _, err := listWorkspaces(database, "missing-run"); !errors.Is(err, errRunNotFound) {
		t.Errorf("err = %v, want errRunNotFound", err)
	}
}

func TestListWorkspaces_RejectsGitHubPRRun(t *testing.T) {
	database := newTestDB(t)
	seedGitHubRun(t, database, "gh-run")

	_, err := listWorkspaces(database, "gh-run")
	if !errors.Is(err, errNotJiraRun) {
		t.Errorf("err = %v, want errNotJiraRun (workspace list must reject GitHub PR runs to keep its contract aligned with workspace add)", err)
	}
}

func TestListWorkspaces_AvailableFiltersOutMaterialized(t *testing.T) {
	database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "owner", "alpha", "https://x", "main")
	seedRepoProfile(t, database, "owner", "beta", "https://x", "main")
	seedRepoProfile(t, database, "owner", "gamma", "https://x", "main")

	// Materialize one of the three.
	if _, _, err := db.InsertRunWorktree(database.Conn, db.RunWorktree{
		RunID: "r1", RepoID: "owner/beta",
		Path: "/tmp/wt/beta", FeatureBranch: "feature/SKY-1",
	}); err != nil {
		t.Fatalf("seed materialized: %v", err)
	}

	out, err := listWorkspaces(database, "r1")
	if err != nil {
		t.Fatalf("listWorkspaces: %v", err)
	}

	// available = configured - materialized.
	availSet := make(map[string]struct{}, len(out.Available))
	for _, a := range out.Available {
		availSet[a.Repo] = struct{}{}
	}
	if _, ok := availSet["owner/beta"]; ok {
		t.Errorf("owner/beta should not appear in available (it's already materialized): %+v", out.Available)
	}
	for _, want := range []string{"owner/alpha", "owner/gamma"} {
		if _, ok := availSet[want]; !ok {
			t.Errorf("expected %q in available, got %+v", want, out.Available)
		}
	}

	// materialized has the one we seeded.
	if len(out.Materialized) != 1 || out.Materialized[0].Repo != "owner/beta" {
		t.Errorf("materialized = %+v, want one entry for owner/beta", out.Materialized)
	}
	if out.Materialized[0].Path != "/tmp/wt/beta" || out.Materialized[0].Branch != "feature/SKY-1" {
		t.Errorf("materialized entry mismatch: %+v", out.Materialized[0])
	}
}

func TestListWorkspaces_NoConfiguredRepos(t *testing.T) {
	database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")

	out, err := listWorkspaces(database, "r1")
	if err != nil {
		t.Fatalf("listWorkspaces: %v", err)
	}
	if len(out.Available) != 0 {
		t.Errorf("available = %+v, want empty", out.Available)
	}
	if len(out.Materialized) != 0 {
		t.Errorf("materialized = %+v, want empty", out.Materialized)
	}
}

func TestListWorkspaces_ScopedToRun(t *testing.T) {
	// Materialized worktrees from a sibling run must NOT leak into r1's list.
	database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedJiraRun(t, database, "r2", "SKY-2")
	seedRepoProfile(t, database, "owner", "shared", "https://x", "main")

	if _, _, err := db.InsertRunWorktree(database.Conn, db.RunWorktree{
		RunID: "r2", RepoID: "owner/shared",
		Path: "/tmp/wt/r2/owner/shared", FeatureBranch: "feature/SKY-2",
	}); err != nil {
		t.Fatalf("seed r2 materialized: %v", err)
	}

	out, err := listWorkspaces(database, "r1")
	if err != nil {
		t.Fatalf("listWorkspaces r1: %v", err)
	}
	if len(out.Materialized) != 0 {
		t.Errorf("r1 materialized = %+v, expected empty (r2's row leaked)", out.Materialized)
	}
	// owner/shared should be available for r1 since r1 hasn't materialized it.
	if len(out.Available) != 1 || out.Available[0].Repo != "owner/shared" {
		t.Errorf("r1 available = %+v, want one entry for owner/shared", out.Available)
	}
}
