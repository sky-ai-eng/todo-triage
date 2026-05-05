package workspace

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// addDeps abstracts the side-effecting collaborators of materializeWorkspace
// so tests can stub the worktree mutations without invoking real git or
// touching the filesystem. Production wiring uses defaultAddDeps which
// delegates to the worktree package.
//
// Only the worktree-mutating surface is injectable. The DB calls go through
// the supplied *db.DB directly because tests already have a real in-memory
// SQLite via newTestDB; mocking the DB layer would test less of the actual
// SQL than letting it run.
type addDeps struct {
	createWorktree func(ctx context.Context, owner, repo, cloneURL, baseBranch, featureBranch, runID, runRoot string) (string, error)
	removeWorktree func(path, runID string) error
}

func defaultAddDeps() addDeps {
	return addDeps{
		createWorktree: worktree.CreateForBranchInRoot,
		removeWorktree: worktree.RemoveAt,
	}
}

// validation errors returned by materializeWorkspace. Callers translate
// these into stderr messages + non-zero exit; tests assert on identity.
var (
	errMissingRunID        = errors.New("workspace add: TRIAGE_FACTORY_RUN_ID not set; this command must be invoked by the delegated agent")
	errInvalidOwnerRepo    = errors.New("workspace add: invalid owner/repo")
	errRunNotFound         = errors.New("workspace add: run not found")
	errTaskNotFound        = errors.New("workspace add: task not found")
	errNotJiraRun          = errors.New("workspace add: only supported for Jira runs; GitHub PR runs have an eagerly-materialized worktree")
	errRepoNotConfigured   = errors.New("workspace add: repo is not configured in Triage Factory; add it on the Settings page first")
	errRepoMissingCloneURL = errors.New("workspace add: repo has no clone URL on its profile; try re-profiling from the Settings page")
)

// materializeWorkspace is the orchestration body of `workspace add`,
// extracted from runAdd so it returns errors instead of os.Exit-ing.
// Returns the absolute worktree path the agent should cd into.
//
// Concurrency: the cross-process serialization point is the
// run_worktrees PK insert (`InsertRunWorktree`'s INSERT OR IGNORE).
// Two concurrent invocations both passing the idempotency precheck
// race at insert time; the loser sees inserted=false and returns the
// winner's path without touching git. Reserving BEFORE the create is
// load-bearing — if we created first, both racing processes would
// hit `git worktree add` against the same deterministic target dir
// and the second would fail on "directory exists" before ever
// reaching the PK conflict.
//
// Order:
//  1. Run + task validation, Jira-vs-GitHub gate.
//  2. Idempotent re-add check: if a row exists AND its path is a
//     live directory, return it. If the row exists but the path is
//     missing/not-a-dir (e.g. wiped by startup orphan sweep), drop
//     the stale row so the reservation step below can re-reserve.
//  3. Repo profile lookup (clone URL required).
//  4. Reserve the run_worktrees row with the deterministic path
//     {runRoot}/{owner}/{repo}. PK conflict picks the winner.
//  5. Loser path: return winner's path immediately.
//  6. Winner path: create the worktree on disk. On failure, release
//     the reservation so the next attempt can retry.
func materializeWorkspace(database *db.DB, runID, ownerRepoArg string, deps addDeps) (string, error) {
	owner, repo, ok := splitOwnerRepo(ownerRepoArg)
	if !ok {
		return "", fmt.Errorf("%w: %q", errInvalidOwnerRepo, ownerRepoArg)
	}
	repoID := owner + "/" + repo

	if runID == "" {
		return "", errMissingRunID
	}

	run, err := db.GetAgentRun(database.Conn, runID)
	if err != nil {
		return "", fmt.Errorf("workspace add: load run: %w", err)
	}
	if run == nil {
		return "", fmt.Errorf("%w: %s", errRunNotFound, runID)
	}
	task, err := db.GetTask(database.Conn, run.TaskID)
	if err != nil {
		return "", fmt.Errorf("workspace add: load task: %w", err)
	}
	if task == nil {
		return "", fmt.Errorf("%w: %s", errTaskNotFound, run.TaskID)
	}
	if task.EntitySource != "jira" {
		return "", fmt.Errorf("%w (run task source is %q)", errNotJiraRun, task.EntitySource)
	}

	// Idempotent re-add. If a row exists for this (run, repo), trust it
	// and return its path — the row is the authoritative reservation.
	//
	// We deliberately do NOT stat-and-drop the row when its on-disk path
	// is missing: the missing-dir window includes the case where another
	// `workspace add` has just reserved the row and its createWorktree is
	// still in flight. Dropping the row in that window would defeat the
	// PK-based serialization and let both processes proceed to create the
	// same target directory. Better to return a path that's about to
	// exist than to undo the reservation.
	//
	// The startup-cleanup-leaves-stale-row scenario this would otherwise
	// have protected against doesn't actually arise: runs don't resume
	// across server restarts, so no agent process invokes workspace add
	// for a run whose dir was wiped post-crash. Genuinely stale rows
	// outlive only their original run and are unreachable.
	existing, err := db.GetRunWorktreeByRepo(database.Conn, runID, repoID)
	if err != nil {
		return "", fmt.Errorf("workspace add: lookup existing worktree: %w", err)
	}
	if existing != nil {
		return existing.Path, nil
	}

	profile, err := db.GetRepoProfile(database.Conn, repoID)
	if err != nil {
		return "", fmt.Errorf("workspace add: load repo profile: %w", err)
	}
	if profile == nil {
		return "", fmt.Errorf("%w: %s", errRepoNotConfigured, repoID)
	}
	if profile.CloneURL == "" {
		return "", fmt.Errorf("%w: %s", errRepoMissingCloneURL, repoID)
	}

	baseBranch := profile.BaseBranch
	if baseBranch == "" {
		baseBranch = profile.DefaultBranch
	}
	featureBranch := "feature/" + task.EntitySourceID
	runRoot := worktree.RunRoot(runID)
	// Path is deterministic from CreateForBranchInRoot's contract:
	// filepath.Join(runRoot, owner, repo). Compute it here so we can
	// reserve the row BEFORE the create runs.
	wtPath := filepath.Join(runRoot, profile.Owner, profile.Repo)

	// Reserve. Two concurrent processes that both reach this point
	// race at the PK; the loser short-circuits before touching git.
	inserted, winningPath, err := db.InsertRunWorktree(database.Conn, db.RunWorktree{
		RunID:         runID,
		RepoID:        repoID,
		Path:          wtPath,
		FeatureBranch: featureBranch,
	})
	if err != nil {
		return "", fmt.Errorf("workspace add: reserve worktree row: %w", err)
	}
	if !inserted {
		// Lost the reservation race. Return the winner's path. There's
		// a tiny window where the winner's createWorktree is still in
		// flight and the path doesn't exist on disk yet; the agent's
		// subsequent `cd` would fail loudly if the create eventually
		// errors (winner deletes the row, next agent retry re-reserves).
		// Polling here would add complexity for negligible gain.
		return winningPath, nil
	}

	// We won. Create the worktree.
	gotPath, err := deps.createWorktree(
		context.Background(),
		profile.Owner, profile.Repo,
		profile.CloneURL,
		baseBranch, featureBranch,
		runID, runRoot,
	)
	if err != nil {
		// Release the reservation so the next attempt can retry.
		// Delete failures are logged but don't shadow the create error
		// the caller actually needs.
		if delErr := db.DeleteRunWorktree(database.Conn, runID, repoID); delErr != nil {
			log.Printf("workspace add: release reservation after create failure: %v", delErr)
		}
		return "", fmt.Errorf("workspace add: create worktree: %w", err)
	}
	if gotPath != wtPath {
		// CreateForBranchInRoot's contract is to land at
		// filepath.Join(runRoot, owner, repo); a divergence means the
		// worktree library changed and our reservation path no longer
		// matches reality. Surface loudly rather than silently storing
		// the wrong path.
		log.Printf("workspace add: created path %q diverges from reserved %q (run=%s repo=%s); investigate", gotPath, wtPath, runID, repoID)
	}

	return wtPath, nil
}

// runAdd is the CLI entrypoint: argv → materializeWorkspace → stdout/stderr.
// All errors translate into exitErr so the caller process gets a non-zero
// exit and the agent sees a clear message on stderr. Successful resolution
// (first add or idempotent re-add) prints the absolute worktree path on
// stdout for `cd "$(... workspace add owner/repo)"`.
func runAdd(database *db.DB, args []string) {
	if len(args) == 0 {
		exitErr("workspace add: missing argument; expected owner/repo")
	}

	path, err := materializeWorkspace(
		database,
		os.Getenv("TRIAGE_FACTORY_RUN_ID"),
		strings.TrimSpace(args[0]),
		defaultAddDeps(),
	)
	if err != nil {
		exitErr(err.Error())
	}
	fmt.Println(path)
}

// splitOwnerRepo splits "owner/repo" once. Both halves must be non-empty.
func splitOwnerRepo(s string) (owner, repo string, ok bool) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
