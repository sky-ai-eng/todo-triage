package projectclassify

import (
	"database/sql"
	"log"
	"time"
)

// DefaultWaitTimeout is the spawner's deadline for a fresh
// classification before a delegation proceeds without project KB.
// 90 seconds gives generous headroom for headless `claude` cold-start
// (~5-15s) plus Stage 1 (~3-8s) and a single Stage 2 escalation
// (~10-30s) with margin. Pathological cases still resolve via the
// timeout rather than hanging the spawner indefinitely.
const DefaultWaitTimeout = 90 * time.Second

// pollInterval is how often WaitFor checks classified_at. SQLite
// reads are sub-millisecond so 1s is essentially free; finer
// granularity wouldn't materially change behavior given that
// classifications take seconds.
const pollInterval = 1 * time.Second

// WaitFor blocks until the entity has been classified (classified_at
// IS NOT NULL) or the timeout elapses. Triggers the runner once on
// entry to ensure the classifier wakes up even if no post-poll
// trigger has fired for this entity yet.
//
// Always returns — never propagates error to the caller. A failed DB
// read is treated as "still unclassified" and the caller proceeds
// with whatever project_id is on the row (typically NULL). The
// alternative (returning the error) would force every caller to
// duplicate the "fall back to no project context" branch.
//
// Intended call site: spawner setup, just before reading
// entity.project_id to inject project knowledge into the worktree.
func WaitFor(database *sql.DB, runner *Runner, entityID string, timeout time.Duration) {
	if entityID == "" || database == nil || runner == nil {
		return
	}
	if classified(database, entityID) {
		return
	}
	runner.Trigger()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(pollInterval)
		if classified(database, entityID) {
			return
		}
	}
	log.Printf("[classify] WaitFor timed out for entity %s after %s — proceeding without project context", entityID, timeout)
}

func classified(database *sql.DB, entityID string) bool {
	var ts sql.NullTime
	err := database.QueryRow(`SELECT classified_at FROM entities WHERE id = ?`, entityID).Scan(&ts)
	if err != nil {
		return false
	}
	return ts.Valid
}
