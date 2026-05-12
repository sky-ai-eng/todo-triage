package sqlite

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// WithTx runs fn inside a single SQLite transaction. orgID must equal
// runmode.LocalDefaultOrg in local mode — anything else indicates a
// caller that thinks it's in multi mode and would silently misbehave
// against SQLite (no org column to enforce isolation). userID is
// accepted for signature parity with the Postgres impl but otherwise
// ignored; SQLite has no auth concept.
//
// The closure receives a TxStores whose every field is wired against
// the *sql.Tx, so any nested call writes through the same transaction.
// Commit on nil error, rollback on any error — the deferred Rollback
// is a no-op after Commit.
func (s *Store) WithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	if orgID != runmode.LocalDefaultOrg {
		return fmt.Errorf("sqlite WithTx: orgID must be %q in local mode, got %q", runmode.LocalDefaultOrg, orgID)
	}
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	txStores := db.TxStores{
		Scores:        newScoreStore(tx),
		Prompts:       newPromptStore(tx, tx),
		Swipes:        newSwipeStore(tx),
		Dashboard:     newDashboardStore(tx),
		Secrets:       newSecretStore(),
		EventHandlers: newEventHandlerStore(tx),
		Agents:        newAgentStore(tx),
		TeamAgents:    newTeamAgentStore(tx),
	}
	if err := fn(txStores); err != nil {
		return err
	}
	return tx.Commit()
}
