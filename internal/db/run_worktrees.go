package db

import (
	"database/sql"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// InsertRunWorktree reserves a row for a worktree the caller is about
// to create. Used as the cross-process serialization point: two
// concurrent `workspace add owner/repo` invocations that both passed
// the GetRunWorktreeByRepo "not found" check race here, and the PK
// conflict deterministically picks one winner.
//
// The path the caller passes is the deterministic target
// ({runRoot}/{owner}/{repo}) — `CreateForBranchInRoot` always lands
// there, so we can record the path BEFORE creating the worktree on
// disk. That ordering matters: if create runs before insert, both
// racing processes try `git worktree add` against the same target
// dir and the second fails on "dir already exists" before we ever
// reach the PK conflict that's supposed to handle them. With insert
// first, the loser sees inserted=false and returns the winner's path
// without touching git.
//
// On PK conflict the winning row's path is returned with
// inserted=false so the caller skips its create step entirely.
//
// Caller responsibilities:
//   - On winner (inserted=true): create the worktree on disk. If the
//     create fails, call DeleteRunWorktree to release the reservation
//     so the next attempt can retry.
//   - On loser (inserted=false): do NOT create the worktree; return
//     winningPath so the agent cd's into the canonical location.
func InsertRunWorktree(database *sql.DB, w domain.RunWorktree) (inserted bool, winningPath string, err error) {
	res, err := database.Exec(`
		INSERT OR IGNORE INTO run_worktrees (run_id, repo_id, path, feature_branch)
		VALUES (?, ?, ?, ?)
	`, w.RunID, w.RepoID, w.Path, w.FeatureBranch)
	if err != nil {
		return false, "", fmt.Errorf("insert run_worktree: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, "", fmt.Errorf("rows affected: %w", err)
	}
	if rows == 1 {
		return true, w.Path, nil
	}
	// Row already existed — look up the winning path.
	existing, err := GetRunWorktreeByRepo(database, w.RunID, w.RepoID)
	if err != nil {
		return false, "", fmt.Errorf("read existing run_worktree after conflict: %w", err)
	}
	if existing == nil {
		// Theoretically impossible: INSERT OR IGNORE matched a row that
		// then vanished. Surface as an error rather than papering over
		// a real DB weirdness.
		return false, "", fmt.Errorf("run_worktree row vanished after INSERT OR IGNORE conflict (run_id=%s, repo_id=%s)", w.RunID, w.RepoID)
	}
	return false, existing.Path, nil
}

// DeleteRunWorktree removes the row for a (run_id, repo_id) pair.
// Used to release a reservation after createWorktree fails, or to
// clear a stale row whose on-disk path was reaped (e.g. by startup
// orphan sweep) so a subsequent `workspace add` can re-reserve.
//
// Idempotent: deleting a row that doesn't exist is a no-op (no error).
// The caller's "is the dir actually missing" check happens elsewhere;
// this helper only manipulates the DB row.
func DeleteRunWorktree(database *sql.DB, runID, repoID string) error {
	_, err := database.Exec(`
		DELETE FROM run_worktrees
		 WHERE run_id = ? AND repo_id = ?
	`, runID, repoID)
	return err
}

// GetRunWorktreeByRepo fetches the worktree row for a (run_id, repo_id)
// pair, or (nil, nil) if none exists. Used by the CLI to short-circuit
// the create+insert path when the agent re-invokes `workspace add`
// against an already-materialized repo.
func GetRunWorktreeByRepo(database *sql.DB, runID, repoID string) (*domain.RunWorktree, error) {
	row := database.QueryRow(`
		SELECT run_id, repo_id, path, feature_branch, created_at
		  FROM run_worktrees
		 WHERE run_id = ? AND repo_id = ?
	`, runID, repoID)
	var w domain.RunWorktree
	if err := row.Scan(&w.RunID, &w.RepoID, &w.Path, &w.FeatureBranch, &w.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &w, nil
}

// GetRunWorktrees returns every worktree materialized for a run, in
// insertion order. The spawner's cleanup defer iterates this list and
// calls worktree.RemoveAt on each path before nuking the run-root.
func GetRunWorktrees(database *sql.DB, runID string) ([]domain.RunWorktree, error) {
	rows, err := database.Query(`
		SELECT run_id, repo_id, path, feature_branch, created_at
		  FROM run_worktrees
		 WHERE run_id = ?
		 ORDER BY created_at ASC, repo_id ASC
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.RunWorktree{}
	for rows.Next() {
		var w domain.RunWorktree
		if err := rows.Scan(&w.RunID, &w.RepoID, &w.Path, &w.FeatureBranch, &w.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
