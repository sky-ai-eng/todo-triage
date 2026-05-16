package domain

import "time"

// RunWorktree is one row in the run_worktrees table — a worktree
// reservation associated with one delegated agent run. Lifecycle is
// managed by db.RunWorktreeStore; the value type lives in domain
// because it's passed across packages (cmd/exec/workspace,
// internal/delegate).
type RunWorktree struct {
	RunID         string
	RepoID        string
	Path          string
	FeatureBranch string
	CreatedAt     time.Time
}
