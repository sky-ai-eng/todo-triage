package domain

import "time"

// TaskMemory is a durable per-run narrative of what an agent tried on a task
// and why. Written to `./task_memory/<run_id>.md` in the worktree during the
// run, then ingested into the `run_memory` table before worktree teardown.
// Materialized back into future runs' worktrees so iterations on the same
// entity can read what prior attempts tried.
//
// Stored in the `run_memory` table with a denormalized `entity_id` for fast
// entity-scoped queries (memory materialization walks the entity graph).
type TaskMemory struct {
	ID        string
	RunID     string
	EntityID  string // denormalized from run→task→entity for fast entity-scoped queries
	Content   string
	CreatedAt time.Time
}
