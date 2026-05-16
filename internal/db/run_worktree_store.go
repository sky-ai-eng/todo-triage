package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=RunWorktreeStore --output=./mocks --case=underscore --with-expecter

// RunWorktreeStore owns the run_worktrees table — one row per
// (run_id, repo_id) reservation tracking the lazy worktree
// materializations a Jira-style run accumulates as the agent calls
// `triagefactory exec workspace add` against each repo it needs.
//
// Lifted out of the pre-D2 package-level functions in
// internal/db/run_worktrees.go so multi-mode Postgres callers route
// through $N placeholders + explicit org_id + the dual-pool
// admin/app split.
//
// Method naming follows the dual-pool convention from
// TaskMemoryStore / EventStore / UsersStore:
//
//   - Plain methods (Insert, GetByRepo, List, DeleteByRepo) run on
//     the app pool in Postgres (RLS-active). Callers are the agent
//     CLI subcommand cmd/exec/workspace, which runs as a subprocess
//     of the delegated agent. Today those calls happen without any
//     JWT context; SKY-302 will wrap them in synthetic-claims via
//     the TF_RUN_ID env var. Pre-SKY-302 the SQLite path is
//     unaffected because local mode has no auth concept.
//
//   - `...System` methods (ListSystem, DeleteByPathSystem) run on
//     the admin pool (BYPASSRLS). Consumers are background
//     goroutines without a JWT-claims context — the delegate
//     spawner's runAgent / chain orchestrator clean up materialized
//     worktrees on terminal exit.
//
// SQLite collapses both pools onto the single connection. The
// `...System` methods are thin wrappers around their non-System
// counterparts; assertLocalOrg gates every entry point.
type RunWorktreeStore interface {
	// Insert reserves a row for a worktree the caller is about to
	// create on disk. Used as the cross-process serialization point:
	// two concurrent `workspace add owner/repo` invocations that
	// both passed the GetByRepo "not found" check race here, and
	// the PK conflict deterministically picks one winner.
	//
	// On PK conflict the winning row's path is returned with
	// inserted=false so the caller skips its create step entirely.
	// On a fresh insert returns inserted=true and the same path the
	// caller supplied.
	Insert(ctx context.Context, orgID string, w domain.RunWorktree) (inserted bool, winningPath string, err error)

	// GetByRepo fetches the worktree row for a (run_id, repo_id)
	// pair, or (nil, nil) if none exists. Used by the workspace
	// CLI to short-circuit the create+insert path when the agent
	// re-invokes `workspace add` against an already-materialized
	// repo.
	GetByRepo(ctx context.Context, orgID, runID, repoID string) (*domain.RunWorktree, error)

	// List returns every worktree materialized for a run, in
	// insertion order. The spawner's cleanup defer iterates this
	// list and calls worktree.RemoveAt on each path before nuking
	// the run-root.
	List(ctx context.Context, orgID, runID string) ([]domain.RunWorktree, error)

	// ListSystem mirrors List for goroutine-internal callers. The
	// delegate spawner's runAgent + chain orchestrator cleanup
	// defers iterate this from a context detached from any request
	// — no JWT claims in scope.
	ListSystem(ctx context.Context, orgID, runID string) ([]domain.RunWorktree, error)

	// DeleteByRepo removes the row for a (run_id, repo_id) pair.
	// Used by the workspace CLI to release a reservation after
	// createWorktree fails, or to clear a stale row whose on-disk
	// path was reaped (e.g. startup orphan sweep) so a subsequent
	// `workspace add` can re-reserve. Idempotent: deleting a row
	// that doesn't exist is a no-op (no error).
	DeleteByRepo(ctx context.Context, orgID, runID, repoID string) error

	// DeleteByPathSystem removes the row for a (run_id, path)
	// pair. Used by the spawner cleanup defer that iterates List
	// and removes worktree rows one-by-one as their on-disk dirs
	// are reaped, so a per-path failure to remove from disk leaves
	// the corresponding DB row intact for the next sweep. Admin
	// pool only; the only caller is the delegate goroutine.
	DeleteByPathSystem(ctx context.Context, orgID, runID, path string) error
}
