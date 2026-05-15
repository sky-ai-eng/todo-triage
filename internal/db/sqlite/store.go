// Package sqlite is the SQLite-backed implementation of the
// per-resource store interfaces declared in package db. Local-mode
// installs of triagefactory wire this implementation at startup
// (multi-mode wires internal/db/postgres). See the SKY-246 D2 spec
// at docs/specs/sky-246-d2-store-abstraction.html for the full
// design.
package sqlite

import (
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// Store holds the SQLite connection + the bundle of resource-store
// implementations wired against it. New returns the assembled
// db.Stores bundle for application startup wiring; handlers should
// depend only on the specific store interfaces they need.
type Store struct {
	conn *sql.DB

	stores db.Stores
}

// New wires a db.Stores bundle backed by SQLite. Wave 0 ships only
// ScoreStore + the TxRunner; subsequent waves populate the remaining
// 21 fields on the bundle.
func New(conn *sql.DB) db.Stores {
	s := &Store{conn: conn}
	users := newUsersStore(conn)
	s.stores = db.Stores{
		Scores:        newScoreStore(conn),
		Prompts:       newPromptStore(conn, conn),
		Swipes:        newSwipeStore(conn),
		Dashboard:     newDashboardStore(conn),
		Secrets:       newSecretStore(),
		EventHandlers: newEventHandlerStore(conn, users),
		Chains:        newChainStore(conn),
		Agents:        newAgentStore(conn),
		TeamAgents:    newTeamAgentStore(conn),
		Users:         users,
		Tasks:         newTaskStore(conn),
		Factory:       newFactoryReadStore(conn),
		AgentRuns:     newAgentRunStore(conn),
		Entities:      newEntityStore(conn),
		Reviews:       newReviewStore(conn),
		PendingPRs:    newPendingPRStore(conn),
		Tx:            s,
	}
	return s.stores
}
