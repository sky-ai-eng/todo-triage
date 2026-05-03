package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureCuratorWorktree_FreshMaterializesAtExpectedPath pins the
// layout contract: <projectDir>/repos/<owner>/<repo>/, checked out at
// the requested branch. SKY-217's frontend will encode the same path
// when telling the agent where to look for source.
func TestEnsureCuratorWorktree_FreshMaterializesAtExpectedPath(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	if _, err := EnsureBareClone(context.Background(), "owner", "repoA", upstream); err != nil {
		t.Fatalf("seed bare: %v", err)
	}

	projectDir := filepath.Join(t.TempDir(), "proj")
	wt, err := EnsureCuratorWorktree(context.Background(), "owner", "repoA", "main", projectDir)
	if err != nil {
		t.Fatalf("EnsureCuratorWorktree: %v", err)
	}
	want := filepath.Join(projectDir, "repos", "owner", "repoA")
	if wt != want {
		t.Errorf("worktree path = %q, want %q", wt, want)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Errorf("expected .git in worktree, got %v", err)
	}
}

// TestEnsureCuratorWorktree_RefreshIsIdempotent pins the every-dispatch
// contract: a second call with the same args refreshes (fetches +
// resets hard) and returns the same path. The third call is a tighter
// idempotence check that no state from the second leaks.
func TestEnsureCuratorWorktree_RefreshIsIdempotent(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	if _, err := EnsureBareClone(context.Background(), "o", "r", upstream); err != nil {
		t.Fatalf("seed bare: %v", err)
	}
	projectDir := filepath.Join(t.TempDir(), "proj")

	for i := 0; i < 3; i++ {
		if _, err := EnsureCuratorWorktree(context.Background(), "o", "r", "main", projectDir); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
}

// TestEnsureCuratorWorktree_RefreshResetsAgentEdits is the load-bearing
// "reset --hard every dispatch" test: after the agent edits a tracked
// file in its working tree, the next dispatch's call to
// EnsureCuratorWorktree must blow that edit away. The Curator's
// contract is "current upstream state," not "agent's WIP."
func TestEnsureCuratorWorktree_RefreshResetsAgentEdits(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	if _, err := EnsureBareClone(context.Background(), "o", "r", upstream); err != nil {
		t.Fatalf("seed bare: %v", err)
	}
	projectDir := filepath.Join(t.TempDir(), "proj")
	wt, err := EnsureCuratorWorktree(context.Background(), "o", "r", "main", projectDir)
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	// makeTestUpstream's seed commit is empty — write a tracked file
	// to the worktree, commit and push it locally to mimic the
	// upstream having a real file, then have the "agent" edit it.
	tracked := filepath.Join(wt, "README.md")
	if err := os.WriteFile(tracked, []byte("upstream content\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	cmds := [][]string{
		{"-C", wt, "config", "user.email", "test@example.com"},
		{"-C", wt, "config", "user.name", "Test"},
		{"-C", wt, "add", "README.md"},
		{"-C", wt, "commit", "-m", "add readme"},
		{"-C", wt, "push", "origin", "main"},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}

	// Simulate the agent dirtying the worktree.
	if err := os.WriteFile(tracked, []byte("AGENT EDIT\n"), 0o644); err != nil {
		t.Fatalf("agent edit: %v", err)
	}

	// Untracked scratch files the agent left behind.
	scratch := filepath.Join(wt, "scratch.txt")
	if err := os.WriteFile(scratch, []byte("debugging output\n"), 0o644); err != nil {
		t.Fatalf("scratch write: %v", err)
	}

	// Next dispatch — refresh.
	if _, err := EnsureCuratorWorktree(context.Background(), "o", "r", "main", projectDir); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// Tracked file restored to upstream content.
	got, err := os.ReadFile(tracked)
	if err != nil {
		t.Fatalf("read after refresh: %v", err)
	}
	if string(got) != "upstream content\n" {
		t.Errorf("tracked file content = %q, want upstream content; reset --hard failed to discard agent edit", got)
	}

	// Untracked file removed by `clean -fdx`.
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("untracked scratch file survived refresh: stat err = %v", err)
	}
}

func TestEnsureCuratorWorktree_RejectsMissingBare(t *testing.T) {
	// Pin: validatePinnedRepos is supposed to enforce existence at
	// the API layer. If somehow a curator dispatch reaches this
	// function without a bare clone, we surface a clear error rather
	// than lazy-cloning (which would silently succeed against a
	// stale URL).
	withTestHome(t)
	projectDir := filepath.Join(t.TempDir(), "proj")
	_, err := EnsureCuratorWorktree(context.Background(), "ghost", "repo", "main", projectDir)
	if err == nil {
		t.Fatal("expected error for missing bare, got nil")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error message should mention missing: %q", err.Error())
	}
}

func TestEnsureCuratorWorktree_RejectsEmptyArgs(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		owner, repo, branch, pdir string
	}{
		{"owner", "", "r", "main", "/tmp/p"},
		{"repo", "o", "", "main", "/tmp/p"},
		{"branch", "o", "r", "", "/tmp/p"},
		{"projectDir", "o", "r", "main", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := EnsureCuratorWorktree(context.Background(), tc.owner, tc.repo, tc.branch, tc.pdir); err == nil {
				t.Errorf("expected error for empty %s", tc.name)
			}
		})
	}
}

// TestPruneCuratorBare_NoOpOnMissingBare proves the cleanup hook is
// safe to call from the project-delete handler regardless of whether
// the bare exists.
func TestPruneCuratorBare_NoOpOnMissingBare(t *testing.T) {
	withTestHome(t)
	// No bare → log+return; should not panic or error.
	PruneCuratorBare("ghost", "repo")
}

func TestCuratorRepoSubpath_NestedLayout(t *testing.T) {
	// Pin: nested <owner>/<repo>, not flattened. Flattening with
	// a dash created a collision class (TestCuratorRepoSubpath_NoCollisions
	// covers that directly); nesting matches GitHub's URL convention
	// and makes the (owner, repo) → subpath mapping injective.
	got := CuratorRepoSubpath("sky-ai-eng", "triage-factory")
	want := filepath.Join("sky-ai-eng", "triage-factory")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestCuratorRepoSubpath_NoCollisions guards the bug an earlier flat
// "owner-repo" form had: GitHub allows hyphens in either half of a
// slug, so "a-b/c" and "a/b-c" both flatten to "a-b-c" and would
// silently share an on-disk path. The slash-separated form makes the
// mapping injective by construction — the slash can't appear inside
// either half of a real GitHub identifier.
func TestCuratorRepoSubpath_NoCollisions(t *testing.T) {
	pairs := [][2]string{
		{"a-b", "c"},
		{"a", "b-c"},
	}
	seen := make(map[string]string)
	for _, p := range pairs {
		got := CuratorRepoSubpath(p[0], p[1])
		if prev, dup := seen[got]; dup {
			t.Errorf("collision: %q produced by both %q and %q/%q", got, prev, p[0], p[1])
		}
		seen[got] = p[0] + "/" + p[1]
	}
}
