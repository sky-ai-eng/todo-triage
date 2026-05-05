package workspace

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
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
// Order matters:
//  1. Run + task validation (and Jira-vs-GitHub gate).
//  2. Idempotency check via run_worktrees — second add for same (run, repo)
//     short-circuits before we touch git or repo profiles.
//  3. Repo-profile lookup (must be configured AND have a clone URL).
//  4. Worktree create on disk.
//  5. DB record. On insert failure (write error or race-loss against a
//     concurrent add) the just-created worktree is rolled back via
//     removeWorktree so the spawner's cleanup defer (which lists
//     run_worktrees) doesn't leak an orphan entry.
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

	// Idempotent re-add: reuse an existing worktree only if the recorded path
	// still exists on disk. Startup cleanup can remove worktree directories
	// without clearing run_worktrees rows, so stale records must be ignored.
	existing, err := db.GetRunWorktreeByRepo(database.Conn, runID, repoID)
	if err != nil {
		return "", fmt.Errorf("workspace add: lookup existing worktree: %w", err)
	}
	if existing != nil {
		info, statErr := os.Stat(existing.Path)
		switch {
		case statErr == nil && info.IsDir():
			return existing.Path, nil
		case statErr == nil && !info.IsDir():
			log.Printf("workspace add: ignoring stale worktree record for run %s repo %s: path is not a directory: %s", runID, repoID, existing.Path)
		case errors.Is(statErr, os.ErrNotExist):
			log.Printf("workspace add: ignoring stale worktree record for run %s repo %s: path missing: %s", runID, repoID, existing.Path)
		default:
			return "", fmt.Errorf("workspace add: stat existing worktree path %q: %w", existing.Path, statErr)
		}
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

	wtPath, err := deps.createWorktree(
		context.Background(),
		profile.Owner, profile.Repo,
		profile.CloneURL,
		baseBranch, featureBranch,
		runID, runRoot,
	)
	if err != nil {
		return "", fmt.Errorf("workspace add: create worktree: %w", err)
	}

	inserted, winningPath, err := db.InsertRunWorktree(database.Conn, db.RunWorktree{
		RunID:         runID,
		RepoID:        repoID,
		Path:          wtPath,
		FeatureBranch: featureBranch,
	})
	if err != nil {
		// Insert failed — the worktree is on disk but the spawner's
		// cleanup defer (which lists run_worktrees) won't see it. Roll
		// back the on-disk side so we don't leak. Rollback failures are
		// logged but don't shadow the original error the caller needs.
		if rmErr := deps.removeWorktree(wtPath, runID); rmErr != nil {
			log.Printf("workspace add: rollback after insert failure: %v", rmErr)
		}
		return "", fmt.Errorf("workspace add: record worktree: %w", err)
	}
	if !inserted {
		// Race-loss: another concurrent `workspace add` for the same
		// (run, repo) won the PK conflict. Clean up our duplicate
		// on-disk worktree (same target path, but RemoveAt + pruneAll
		// still resets the bare's tracking cleanly) and return the
		// winning row's path so the agent cd's into the canonical one.
		if rmErr := deps.removeWorktree(wtPath, runID); rmErr != nil {
			log.Printf("workspace add: cleanup loser worktree after race: %v", rmErr)
		}
		return winningPath, nil
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
