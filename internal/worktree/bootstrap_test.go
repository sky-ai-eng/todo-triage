package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeTestUpstream creates a minimal bare git repository at a tempdir
// path that EnsureBareClone can use as an "origin" URL. The bare gets
// one commit on main so there's a ref to fetch.
func makeTestUpstream(t *testing.T) string {
	t.Helper()
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=main", upstream).CombinedOutput(); err != nil {
		t.Fatalf("git init bare: %v: %s", err, out)
	}

	// Build a commit elsewhere and push it so the bare has a ref.
	work := filepath.Join(t.TempDir(), "work")
	if out, err := exec.Command("git", "init", "-b", "main", work).CombinedOutput(); err != nil {
		t.Fatalf("git init work: %v: %s", err, out)
	}
	cmds := [][]string{
		{"-C", work, "config", "user.email", "test@example.com"},
		{"-C", work, "config", "user.name", "Test"},
		{"-C", work, "commit", "--allow-empty", "-m", "initial"},
		{"-C", work, "remote", "add", "origin", upstream},
		{"-C", work, "push", "origin", "main"},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}
	return upstream
}

// withTestHome points $HOME at a tempdir for the duration of the test
// so repoDir() returns paths under it instead of touching the user's
// real ~/.triagefactory. Also overrides $TMPDIR so worktrees created
// via runDir() land under a tempdir that t.TempDir() will auto-clean.
// os.TempDir() honors $TMPDIR on Unix, so this isolation works without
// any worktree-package changes.
func withTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	return home
}

func TestEnsureBareClone_Idempotent(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	for i := 0; i < 3; i++ {
		if _, err := EnsureBareClone(context.Background(), "owner2", "repo2", upstream); err != nil {
			t.Fatalf("EnsureBareClone iteration %d: %v", i, err)
		}
	}

	bareDir, _ := repoDir("owner2", "repo2")
	if _, err := os.Stat(bareDir); err != nil {
		t.Fatalf("bare dir missing after repeated EnsureBareClone: %v", err)
	}
}

func TestEnsureBareClone_RepairsOriginURL(t *testing.T) {
	withTestHome(t)
	upstream1 := makeTestUpstream(t)
	upstream2 := makeTestUpstream(t)

	bareDir, err := EnsureBareClone(context.Background(), "owner3", "repo3", upstream1)
	if err != nil {
		t.Fatalf("first EnsureBareClone: %v", err)
	}
	out, err := exec.Command("git", "-C", bareDir, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		t.Fatalf("read origin url: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != upstream1 {
		t.Fatalf("setup: expected origin %q, got %q", upstream1, got)
	}

	if _, err := EnsureBareClone(context.Background(), "owner3", "repo3", upstream2); err != nil {
		t.Fatalf("second EnsureBareClone: %v", err)
	}
	out, err = exec.Command("git", "-C", bareDir, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		t.Fatalf("read origin url after repair: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != upstream2 {
		t.Errorf("expected origin repaired to %q, got %q", upstream2, got)
	}
}

func TestBootstrapBareClones_SkipsEmptyCloneURL(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	BootstrapBareClones(context.Background(), []BootstrapTarget{
		{Owner: "with", Repo: "url", CloneURL: upstream},
		{Owner: "without", Repo: "url", CloneURL: ""},
	})

	withDir, _ := repoDir("with", "url")
	if _, err := os.Stat(withDir); err != nil {
		t.Errorf("expected bare for non-empty URL: %v", err)
	}
	withoutDir, _ := repoDir("without", "url")
	if _, err := os.Stat(withoutDir); !os.IsNotExist(err) {
		t.Errorf("expected no bare for empty URL, got err=%v", err)
	}
}

func TestBootstrapBareClones_EmptyTargets(t *testing.T) {
	BootstrapBareClones(context.Background(), nil)
	BootstrapBareClones(context.Background(), []BootstrapTarget{})
}

// TestCreateForPR_ForkPR is the regression test for the full fork-PR
// flow: fetch via refs/pull/<n>/head, check out under a pr-<n> local
// branch, configure tracking so `git push` (no remote arg) sends
// commits to the fork's actual branch — not to upstream. The
// previous implementation either failed to fetch (fork branch
// missing on origin) or, after the fetch fix, pushed commits to
// upstream where they'd create a stray branch instead of updating
// the contributor's PR.
//
// Setup mirrors GitHub's real fork-PR state:
//
//   - upstream bare repo holds refs/pull/42/head (GitHub's mirror
//     of the PR head). Does NOT have refs/heads/feature-branch.
//   - fork bare repo holds refs/heads/feature-branch (the
//     contributor's actual branch). Does NOT have any pull refs.
//
// After CreateForPR + a simulated agent push:
//
//   - Fork's refs/heads/feature-branch must advance to a new commit.
//   - Upstream must be untouched (no stray branch created).
func TestCreateForPR_ForkPR(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	fork := makeTestUpstream(t) // separate bare; acts as the contributor's fork

	// Seed the fork with feature-branch and the upstream with
	// refs/pull/42/head, both pointing at the same "fork PR commit".
	work := filepath.Join(t.TempDir(), "fork-work")
	if out, err := exec.Command("git", "init", "-b", "main", work).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	cmds := [][]string{
		{"-C", work, "config", "user.email", "fork@example.com"},
		{"-C", work, "config", "user.name", "Forker"},
		{"-C", work, "remote", "add", "fork", fork},
		{"-C", work, "remote", "add", "up", upstream},
		{"-C", work, "fetch", "up", "main"},
		{"-C", work, "checkout", "-b", "feature-branch", "FETCH_HEAD"},
		{"-C", work, "commit", "--allow-empty", "-m", "fork PR commit"},
		{"-C", work, "push", "fork", "feature-branch:refs/heads/feature-branch"},
		{"-C", work, "push", "up", "HEAD:refs/pull/42/head"},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}
	out, err := exec.Command("git", "-C", work, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse fork PR commit: %v", err)
	}
	originalForkCommit := strings.TrimSpace(string(out))

	// Sanity: upstream lacks the head branch.
	if out, err := exec.Command("git", "-C", upstream, "show-ref", "--verify", "refs/heads/feature-branch").CombinedOutput(); err == nil {
		t.Fatalf("test setup: upstream unexpectedly has refs/heads/feature-branch: %s", out)
	}

	wtPath, err := CreateForPR(context.Background(), "owner-fork-test", "repo-fork-test", upstream, fork, "feature-branch", 42, "fork-pr-test-run")
	if err != nil {
		t.Fatalf("CreateForPR for fork PR: %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(wtPath, "fork-pr-test-run") })

	// Worktree HEAD must point at the PR's head commit.
	out, err = exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("worktree rev-parse HEAD: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != originalForkCommit {
		t.Errorf("worktree HEAD = %q, want %q", got, originalForkCommit)
	}

	// For fork PRs the local branch is named pr-<n> to avoid colliding
	// with own-repo runs that may share the head ref name.
	out, err = exec.Command("git", "-C", wtPath, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("worktree rev-parse abbrev: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "pr-42" {
		t.Errorf("worktree branch = %q, want %q", got, "pr-42")
	}

	// Simulate an agent: commit a change in the worktree, push (no
	// remote argument so branch tracking takes effect), then verify
	// the FORK's feature-branch advanced — and the UPSTREAM stayed
	// put. This is the actual user-visible win of the fork-tracking
	// configuration.
	agentCmds := [][]string{
		{"-C", wtPath, "config", "user.email", "agent@example.com"},
		{"-C", wtPath, "config", "user.name", "Agent"},
		{"-C", wtPath, "commit", "--allow-empty", "-m", "agent fix"},
		{"-C", wtPath, "push"},
	}
	for _, c := range agentCmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}

	// Fork's feature-branch must now point at a commit different from
	// the original (the agent's empty commit on top).
	out, err = exec.Command("git", "-C", fork, "rev-parse", "refs/heads/feature-branch").Output()
	if err != nil {
		t.Fatalf("rev-parse fork feature-branch after push: %v", err)
	}
	newForkTip := strings.TrimSpace(string(out))
	if newForkTip == originalForkCommit {
		t.Errorf("fork's feature-branch did not advance after agent push (still %q) — push went somewhere else", newForkTip)
	}

	// Upstream must NOT have grown a stray branch. Pre-tracking-fix,
	// the agent's push would have created refs/heads/pr-42 on
	// upstream; this assertion catches a regression of that bug.
	if out, err := exec.Command("git", "-C", upstream, "show-ref", "--verify", "refs/heads/pr-42").CombinedOutput(); err == nil {
		t.Errorf("upstream gained a stray refs/heads/pr-42 branch — push leaked: %s", out)
	}
	if out, err := exec.Command("git", "-C", upstream, "show-ref", "--verify", "refs/heads/feature-branch").CombinedOutput(); err == nil {
		t.Errorf("upstream gained a stray refs/heads/feature-branch branch — push leaked: %s", out)
	}
}

// TestCreateForPR_OwnRepoPR_FetchesViaPullRef confirms the
// refs/pull/<n>/head fetch path is also correct for the common case
// where the PR is from a branch on the upstream itself. GitHub
// maintains refs/pull/<n>/head for every PR regardless of fork
// status, so the same code path should work uniformly.
func TestCreateForPR_OwnRepoPR_FetchesViaPullRef(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)

	// Push a feature branch directly to upstream AND mirror it as
	// refs/pull/7/head — the state GitHub sets up for an own-repo PR.
	work := filepath.Join(t.TempDir(), "work-own")
	if out, err := exec.Command("git", "init", "-b", "main", work).CombinedOutput(); err != nil {
		t.Fatalf("git init work: %v: %s", err, out)
	}
	cmds := [][]string{
		{"-C", work, "config", "user.email", "me@example.com"},
		{"-C", work, "config", "user.name", "Me"},
		{"-C", work, "remote", "add", "origin", upstream},
		{"-C", work, "fetch", "origin", "main"},
		{"-C", work, "checkout", "-b", "my-feature", "FETCH_HEAD"},
		{"-C", work, "commit", "--allow-empty", "-m", "own-repo PR commit"},
		{"-C", work, "push", "origin", "my-feature:refs/heads/my-feature"},
		{"-C", work, "push", "origin", "my-feature:refs/pull/7/head"},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}
	out, err := exec.Command("git", "-C", work, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse work HEAD: %v", err)
	}
	expected := strings.TrimSpace(string(out))

	// Own-repo PRs pass the same URL as both upstream and head — the
	// PR's head.repo and base.repo are the same repo. CreateForPR
	// detects this and skips the fork-tracking configuration.
	wtPath, err := CreateForPR(context.Background(), "owner-own-test", "repo-own-test", upstream, upstream, "my-feature", 7, "own-pr-test-run")
	if err != nil {
		t.Fatalf("CreateForPR for own-repo PR: %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(wtPath, "own-pr-test-run") })

	out, err = exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("worktree rev-parse HEAD: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != expected {
		t.Errorf("worktree HEAD = %q, want %q", got, expected)
	}
	out, err = exec.Command("git", "-C", wtPath, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("worktree rev-parse abbrev: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "my-feature" {
		t.Errorf("worktree branch = %q, want %q (git push relies on attached branch, not detached HEAD)", got, "my-feature")
	}

	// Tracking is preconfigured so `git push` (no remote argument)
	// works without -u. Envelope guidance tells agents to use this
	// form; verify it actually lands on upstream's my-feature.
	agentCmds := [][]string{
		{"-C", wtPath, "config", "user.email", "agent@example.com"},
		{"-C", wtPath, "config", "user.name", "Agent"},
		{"-C", wtPath, "commit", "--allow-empty", "-m", "agent fix"},
		{"-C", wtPath, "push"},
	}
	for _, c := range agentCmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}
	out, err = exec.Command("git", "-C", upstream, "rev-parse", "refs/heads/my-feature").Output()
	if err != nil {
		t.Fatalf("rev-parse upstream my-feature after push: %v", err)
	}
	if newTip := strings.TrimSpace(string(out)); newTip == expected {
		t.Errorf("upstream's my-feature did not advance after agent push (still %q) — tracking didn't take effect", newTip)
	}
}
