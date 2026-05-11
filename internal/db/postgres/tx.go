package postgres

import (
	"context"
	"encoding/json"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// WithTx runs fn inside a single Postgres transaction against the app
// pool (RLS-active). Before fn runs, it calls
//
//	SELECT set_config('request.jwt.claims', $1, true)
//
// so RLS policies + tf.current_user_id() / tf.current_org_id() helpers
// see the right (orgID, userID) for this tx. set_config(..., true)
// scopes the setting to the tx — it doesn't leak to other connections
// in the pool.
//
// Callers always pass orgID + userID explicitly. D7 will replace the
// explicit pass with extraction from a request-scoped context (e.g.
// authctx.ClaimsFromContext(ctx)), but the WithTx shape stays the same.
//
// Closures that need to bypass RLS (system services) shouldn't use
// WithTx at all — they should call store methods directly on the
// admin-wired stores (db.Stores.Scores in wave 0; more in later
// waves). WithTx is purely for the request-handler atomicity boundary.
func (s *Store) WithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	tx, err := s.app.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	claims, err := json.Marshal(map[string]any{
		"sub":    userID,
		"org_id": orgID,
	})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('request.jwt.claims', $1, true)`, string(claims)); err != nil {
		return err
	}

	txStores := db.TxStores{
		Scores:     newScoreStore(tx),
		Prompts:    newTxPromptStore(tx),
		Swipes:     newSwipeStore(tx),
		Dashboard:  newDashboardStore(tx),
		Secrets:    newSecretStore(tx),
		TaskRules:  newTxTaskRuleStore(tx),
		Triggers:   newTxTriggerStore(tx),
		Agents:     newTxAgentStore(tx),
		TeamAgents: newTxTeamAgentStore(tx),
	}
	if err := fn(txStores); err != nil {
		return err
	}
	return tx.Commit()
}
