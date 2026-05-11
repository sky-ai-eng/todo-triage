package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// triggerStore is the SQLite impl of db.TriggerStore. SQL bodies are
// ported from the pre-D2 internal/db/prompt_triggers.go; behavioral
// changes are:
//
//   - assertLocalOrg at every method entry,
//   - context propagation on every Exec/Query,
//   - UTC timestamps everywhere (matches the project-wide convention),
//   - Source pinned at insert: Create → "user"; Seed → "system".
type triggerStore struct{ q queryer }

func newTriggerStore(q queryer) db.TriggerStore { return &triggerStore{q: q} }

var _ db.TriggerStore = (*triggerStore)(nil)

// pgTriggerColumns and sqliteTriggerColumns intentionally mirror each
// other's projection order so the scan helpers can be shared between
// backends if SKY-259's event_handlers unification ever wants that.
// For now they're duplicated to keep each impl readable.
const sqliteTriggerColumns = `id, prompt_id, trigger_type, event_type, scope_predicate_json,
       breaker_threshold, min_autonomy_suitability, enabled, source, created_at, updated_at`

func (s *triggerStore) Seed(ctx context.Context, orgID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	now := time.Now().UTC()
	var inserted int64
	for _, t := range db.ShippedPromptTriggers {
		// All shipped triggers ship disabled (see project memory
		// feedback_system_triggers.md). The user opts in or replaces.
		var pred any
		if t.Predicate == "" {
			pred = nil
		} else {
			pred = t.Predicate
		}
		// INSERT OR IGNORE so re-seeds preserve user customizations
		// (re-enable, retarget, predicate edits) on existing rows.
		res, err := s.q.ExecContext(ctx, `
			INSERT OR IGNORE INTO prompt_triggers
				(id, prompt_id, trigger_type, event_type, scope_predicate_json,
				 breaker_threshold, min_autonomy_suitability, enabled, source, created_at, updated_at)
			VALUES (?, ?, 'event', ?, ?, ?, ?, 0, 'system', ?, ?)
		`, t.ID, t.PromptID, t.EventType, pred,
			t.BreakerThreshold, t.MinAutonomySuitability, now, now)
		if err != nil {
			return fmt.Errorf("seed prompt_trigger %s: %w", t.ID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		inserted += n
	}
	log.Printf("[db] seeded %d new prompt_triggers (%d already existed)", inserted, int64(len(db.ShippedPromptTriggers))-inserted)
	return nil
}

func (s *triggerStore) List(ctx context.Context, orgID string) ([]domain.PromptTrigger, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `SELECT `+sqliteTriggerColumns+` FROM prompt_triggers ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var triggers []domain.PromptTrigger
	for rows.Next() {
		t, err := scanTriggerSQLite(rows)
		if err != nil {
			return nil, err
		}
		triggers = append(triggers, t)
	}
	return triggers, rows.Err()
}

func (s *triggerStore) Get(ctx context.Context, orgID string, id string) (*domain.PromptTrigger, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `SELECT `+sqliteTriggerColumns+` FROM prompt_triggers WHERE id = ?`, id)
	t, err := scanTriggerRowSQLite(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *triggerStore) Create(ctx context.Context, orgID string, t domain.PromptTrigger) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	if t.TriggerType != domain.TriggerTypeEvent {
		return fmt.Errorf("sqlite triggers: unsupported trigger_type %q: only %q is supported", t.TriggerType, domain.TriggerTypeEvent)
	}
	now := time.Now().UTC()
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO prompt_triggers (id, prompt_id, trigger_type, event_type, scope_predicate_json,
		                             breaker_threshold, min_autonomy_suitability, enabled, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'user', ?, ?)
	`, t.ID, t.PromptID, t.TriggerType, t.EventType, t.ScopePredicateJSON,
		t.BreakerThreshold, t.MinAutonomySuitability, t.Enabled, now, now)
	return err
}

func (s *triggerStore) Update(ctx context.Context, orgID string, t domain.PromptTrigger) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE prompt_triggers
		SET scope_predicate_json = ?, breaker_threshold = ?,
		    min_autonomy_suitability = ?, updated_at = ?
		WHERE id = ?
	`, t.ScopePredicateJSON, t.BreakerThreshold,
		t.MinAutonomySuitability, time.Now().UTC(), t.ID)
	return err
}

func (s *triggerStore) SetEnabled(ctx context.Context, orgID string, id string, enabled bool) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE prompt_triggers SET enabled = ?, updated_at = ? WHERE id = ?
	`, enabled, time.Now().UTC(), id)
	return err
}

func (s *triggerStore) Delete(ctx context.Context, orgID string, id string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `DELETE FROM prompt_triggers WHERE id = ?`, id)
	return err
}

func (s *triggerStore) ListForPrompt(ctx context.Context, orgID string, promptID string) ([]domain.PromptTrigger, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteTriggerColumns+`
		FROM prompt_triggers
		WHERE prompt_id = ?
		ORDER BY created_at DESC
	`, promptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var triggers []domain.PromptTrigger
	for rows.Next() {
		t, err := scanTriggerSQLite(rows)
		if err != nil {
			return nil, err
		}
		triggers = append(triggers, t)
	}
	return triggers, rows.Err()
}

func (s *triggerStore) GetActiveForEvent(ctx context.Context, orgID string, eventType string) ([]domain.PromptTrigger, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteTriggerColumns+`
		FROM prompt_triggers
		WHERE event_type = ? AND enabled = 1
	`, eventType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var triggers []domain.PromptTrigger
	for rows.Next() {
		t, err := scanTriggerSQLite(rows)
		if err != nil {
			return nil, err
		}
		triggers = append(triggers, t)
	}
	return triggers, rows.Err()
}

func scanTriggerSQLite(rows *sql.Rows) (domain.PromptTrigger, error) {
	var t domain.PromptTrigger
	err := rows.Scan(&t.ID, &t.PromptID, &t.TriggerType, &t.EventType, &t.ScopePredicateJSON,
		&t.BreakerThreshold, &t.MinAutonomySuitability, &t.Enabled, &t.Source, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}

func scanTriggerRowSQLite(row *sql.Row) (domain.PromptTrigger, error) {
	var t domain.PromptTrigger
	err := row.Scan(&t.ID, &t.PromptID, &t.TriggerType, &t.EventType, &t.ScopePredicateJSON,
		&t.BreakerThreshold, &t.MinAutonomySuitability, &t.Enabled, &t.Source, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}
