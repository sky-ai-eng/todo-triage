// Package routing handles event routing: task creation, auto-delegation,
// inline close checks, and entity lifecycle transitions. It replaces the
// old auto-delegate hook in internal/delegate/auto.go.
package routing

import (
	"database/sql"
	"log"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// EntityTerminatingEvents is the set of event types that trigger an entity
// lifecycle close (active → closed). When one of these fires, the entity
// transitions to closed and all its active tasks are cascade-closed with
// close_reason="entity_closed".
var EntityTerminatingEvents = map[string]bool{
	domain.EventGitHubPRMerged:     true,
	domain.EventGitHubPRClosed:     true,
	domain.EventJiraIssueCompleted: true,
}

// HandleEntityClose transitions an entity to closed state and cascade-closes
// all active tasks on it. Returns the number of tasks closed.
//
// This is the "entity lifecycle" close path — uniform, doesn't enumerate
// event types. Separate from inline close checks (per-event-type resolution)
// and run-completion close (spawner sets close_reason=run_completed).
func HandleEntityClose(database *sql.DB, entityID string) (int, error) {
	if err := db.CloseEntity(database, entityID); err != nil {
		return 0, err
	}

	closed, err := db.CloseAllEntityTasks(database, entityID, "entity_closed")
	if err != nil {
		return 0, err
	}

	if closed > 0 {
		log.Printf("[lifecycle] entity %s closed → %d tasks cascade-closed", entityID, closed)
	}
	return closed, nil
}
