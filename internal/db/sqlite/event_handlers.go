package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// eventHandlerStore is the SQLite impl of db.EventHandlerStore. Post-
// SKY-269 the SQLite schema carries org_id structurally, so every
// method filters by org_id (matching the Postgres impl's WHERE pattern)
// rather than the older assertLocalOrg convention the predecessor
// stores used.
//
// Per-kind CHECK constraints on event_handlers enforce the shape
// pair at the DB level; this store branches on Kind where the SQL
// diverges (Create / Update / Seed write different column sets per
// kind).
type eventHandlerStore struct{ q queryer }

func newEventHandlerStore(q queryer) db.EventHandlerStore {
	return &eventHandlerStore{q: q}
}

var _ db.EventHandlerStore = (*eventHandlerStore)(nil)

// sqliteEventHandlerColumns mirrors the Postgres projection so scan
// helpers stay aligned. Per-kind nullable columns scan into sql.Null*
// and map to the domain pointer fields.
const sqliteEventHandlerColumns = `id, kind, event_type, scope_predicate_json, enabled, source,
       name, default_priority, sort_order,
       prompt_id, breaker_threshold, min_autonomy_suitability,
       created_at, updated_at`

func (s *eventHandlerStore) Seed(ctx context.Context, orgID string) error {
	now := time.Now().UTC()
	var inserted int64
	for _, h := range db.ShippedEventHandlers {
		var pred any
		if h.Predicate != "" {
			pred = h.Predicate
		}

		switch h.Kind {
		case domain.EventHandlerKindRule:
			// Shipped rules ship enabled; visibility='org', source='system',
			// creator_user_id NULL — same shape as Postgres.
			res, err := s.q.ExecContext(ctx, `
				INSERT OR IGNORE INTO event_handlers
					(id, org_id, creator_user_id, visibility, kind, event_type,
					 scope_predicate_json, enabled, source,
					 name, default_priority, sort_order,
					 created_at, updated_at)
				VALUES (?, ?, NULL, 'org', 'rule', ?,
				        ?, 1, 'system',
				        ?, ?, ?,
				        ?, ?)
			`, h.ID, orgID, h.EventType,
				pred,
				h.Name, h.DefaultPriority, h.SortOrder,
				now, now)
			if err != nil {
				return fmt.Errorf("seed event_handler rule %s: %w", h.ID, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			inserted += n

		case domain.EventHandlerKindTrigger:
			// Shipped triggers ship disabled (project convention —
			// users opt in or replace). visibility='org', source='system',
			// creator_user_id NULL.
			res, err := s.q.ExecContext(ctx, `
				INSERT OR IGNORE INTO event_handlers
					(id, org_id, creator_user_id, visibility, kind, event_type,
					 scope_predicate_json, enabled, source,
					 prompt_id, breaker_threshold, min_autonomy_suitability,
					 created_at, updated_at)
				VALUES (?, ?, NULL, 'org', 'trigger', ?,
				        ?, 0, 'system',
				        ?, ?, ?,
				        ?, ?)
			`, h.ID, orgID, h.EventType,
				pred,
				h.PromptID, h.BreakerThreshold, h.MinAutonomySuitability,
				now, now)
			if err != nil {
				return fmt.Errorf("seed event_handler trigger %s: %w", h.ID, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			inserted += n

		default:
			return fmt.Errorf("seed event_handler %s: unknown kind %q", h.ID, h.Kind)
		}
	}
	log.Printf("[db] seeded %d new event_handlers (%d already existed)", inserted, int64(len(db.ShippedEventHandlers))-inserted)
	return nil
}

func (s *eventHandlerStore) List(ctx context.Context, orgID string, kind string) ([]domain.EventHandler, error) {
	q := `SELECT ` + sqliteEventHandlerColumns + ` FROM event_handlers WHERE org_id = ?`
	args := []any{orgID}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	q += `
	      ORDER BY kind ASC,
	               CASE WHEN kind = 'rule' THEN sort_order ELSE 0 END ASC,
	               CASE WHEN kind = 'rule' THEN name ELSE '' END ASC,
	               created_at DESC`
	rows, err := s.q.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEventHandlersSQLite(rows)
}

func (s *eventHandlerStore) Get(ctx context.Context, orgID, id string) (*domain.EventHandler, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT `+sqliteEventHandlerColumns+`
		FROM event_handlers
		WHERE org_id = ? AND id = ?
	`, orgID, id)
	h, err := scanEventHandlerRowSQLite(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

func (s *eventHandlerStore) GetEnabledForEvent(ctx context.Context, orgID, eventType string) ([]domain.EventHandler, error) {
	// Rules-before-triggers order (same as Postgres impl) — preserves
	// the pre-unification observable shape.
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteEventHandlerColumns+`
		FROM event_handlers
		WHERE org_id = ? AND event_type = ? AND enabled = 1
		ORDER BY kind ASC,
		         CASE WHEN kind = 'rule' THEN sort_order ELSE 0 END ASC,
		         created_at DESC
	`, orgID, eventType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEventHandlersSQLite(rows)
}

func (s *eventHandlerStore) ListForPrompt(ctx context.Context, orgID, promptID string) ([]domain.EventHandler, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteEventHandlerColumns+`
		FROM event_handlers
		WHERE org_id = ? AND prompt_id = ? AND kind = 'trigger'
		ORDER BY created_at DESC
	`, orgID, promptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEventHandlersSQLite(rows)
}

func (s *eventHandlerStore) Create(ctx context.Context, orgID string, h domain.EventHandler) error {
	if err := validateForCreateSQLite(&h); err != nil {
		return err
	}
	var pred any
	if h.ScopePredicateJSON != nil {
		pred = *h.ScopePredicateJSON
	}
	now := time.Now().UTC()

	switch h.Kind {
	case domain.EventHandlerKindRule:
		_, err := s.q.ExecContext(ctx, `
			INSERT INTO event_handlers
				(id, org_id, kind, event_type,
				 scope_predicate_json, enabled, source,
				 name, default_priority, sort_order,
				 created_at, updated_at)
			VALUES (?, ?, 'rule', ?,
			        ?, ?, 'user',
			        ?, ?, ?,
			        ?, ?)
		`, h.ID, orgID, h.EventType,
			pred, h.Enabled,
			h.Name, derefFloat(h.DefaultPriority), derefInt(h.SortOrder),
			now, now)
		return err

	case domain.EventHandlerKindTrigger:
		_, err := s.q.ExecContext(ctx, `
			INSERT INTO event_handlers
				(id, org_id, kind, event_type,
				 scope_predicate_json, enabled, source,
				 prompt_id, breaker_threshold, min_autonomy_suitability,
				 created_at, updated_at)
			VALUES (?, ?, 'trigger', ?,
			        ?, ?, 'user',
			        ?, ?, ?,
			        ?, ?)
		`, h.ID, orgID, h.EventType,
			pred, h.Enabled,
			h.PromptID, derefInt(h.BreakerThreshold), derefFloat(h.MinAutonomySuitability),
			now, now)
		return err
	}
	return fmt.Errorf("sqlite event_handlers Create: unknown kind %q", h.Kind)
}

func (s *eventHandlerStore) Update(ctx context.Context, orgID string, h domain.EventHandler) error {
	if err := validateForCreateSQLite(&h); err != nil {
		return err
	}
	var pred any
	if h.ScopePredicateJSON != nil {
		pred = *h.ScopePredicateJSON
	}
	now := time.Now().UTC()

	switch h.Kind {
	case domain.EventHandlerKindRule:
		_, err := s.q.ExecContext(ctx, `
			UPDATE event_handlers
			SET scope_predicate_json = ?, enabled = ?,
			    name = ?, default_priority = ?, sort_order = ?,
			    updated_at = ?
			WHERE org_id = ? AND id = ? AND kind = 'rule'
		`, pred, h.Enabled,
			h.Name, derefFloat(h.DefaultPriority), derefInt(h.SortOrder),
			now, orgID, h.ID)
		return err

	case domain.EventHandlerKindTrigger:
		_, err := s.q.ExecContext(ctx, `
			UPDATE event_handlers
			SET scope_predicate_json = ?, enabled = ?,
			    breaker_threshold = ?, min_autonomy_suitability = ?,
			    updated_at = ?
			WHERE org_id = ? AND id = ? AND kind = 'trigger'
		`, pred, h.Enabled,
			derefInt(h.BreakerThreshold), derefFloat(h.MinAutonomySuitability),
			now, orgID, h.ID)
		return err
	}
	return fmt.Errorf("sqlite event_handlers Update: unknown kind %q", h.Kind)
}

func (s *eventHandlerStore) SetEnabled(ctx context.Context, orgID, id string, enabled bool) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE event_handlers SET enabled = ?, updated_at = ? WHERE org_id = ? AND id = ?
	`, enabled, time.Now().UTC(), orgID, id)
	return err
}

func (s *eventHandlerStore) Delete(ctx context.Context, orgID, id string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM event_handlers WHERE org_id = ? AND id = ?`, orgID, id)
	return err
}

func (s *eventHandlerStore) Reorder(ctx context.Context, orgID string, ids []string) error {
	return inTx(ctx, s.q, func(q queryer) error {
		now := time.Now().UTC()
		for i, id := range ids {
			if _, err := q.ExecContext(ctx, `
				UPDATE event_handlers SET sort_order = ?, updated_at = ?
				WHERE org_id = ? AND id = ? AND kind = 'rule'
			`, i, now, orgID, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *eventHandlerStore) Promote(ctx context.Context, orgID string, id string, t domain.EventHandler) error {
	if t.Kind != domain.EventHandlerKindTrigger {
		return errors.New("sqlite event_handlers Promote: target kind must be 'trigger'")
	}
	if t.PromptID == "" || t.BreakerThreshold == nil || t.MinAutonomySuitability == nil {
		return errors.New("sqlite event_handlers Promote: trigger fields required")
	}
	var pred any
	if t.ScopePredicateJSON != nil {
		pred = *t.ScopePredicateJSON
	}
	// Single UPDATE flips kind, clears rule-only columns, populates
	// trigger-only. The per-kind CHECK constraints validate atomically.
	res, err := s.q.ExecContext(ctx, `
		UPDATE event_handlers
		SET kind = 'trigger',
		    prompt_id = ?, breaker_threshold = ?, min_autonomy_suitability = ?,
		    name = NULL, default_priority = NULL, sort_order = NULL,
		    scope_predicate_json = ?,
		    updated_at = ?
		WHERE org_id = ? AND id = ? AND kind = 'rule'
	`, t.PromptID, *t.BreakerThreshold, *t.MinAutonomySuitability,
		pred, time.Now().UTC(),
		orgID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("sqlite event_handlers Promote: row not found or not a rule")
	}
	return nil
}

func collectEventHandlersSQLite(rows *sql.Rows) ([]domain.EventHandler, error) {
	var out []domain.EventHandler
	for rows.Next() {
		h, err := scanEventHandlerSQLite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func scanEventHandlerSQLite(rows *sql.Rows) (domain.EventHandler, error) {
	return scanEventHandlerFromAnySQLite(rows.Scan)
}

func scanEventHandlerRowSQLite(row *sql.Row) (domain.EventHandler, error) {
	return scanEventHandlerFromAnySQLite(row.Scan)
}

func scanEventHandlerFromAnySQLite(scanFn func(dst ...any) error) (domain.EventHandler, error) {
	var h domain.EventHandler
	var (
		pred          sql.NullString
		nameNS        sql.NullString
		defPriority   sql.NullFloat64
		sortOrder     sql.NullInt64
		promptID      sql.NullString
		breakerNS     sql.NullInt64
		minAutonomyNS sql.NullFloat64
	)
	if err := scanFn(
		&h.ID, &h.Kind, &h.EventType, &pred, &h.Enabled, &h.Source,
		&nameNS, &defPriority, &sortOrder,
		&promptID, &breakerNS, &minAutonomyNS,
		&h.CreatedAt, &h.UpdatedAt,
	); err != nil {
		return h, err
	}
	if pred.Valid {
		s := pred.String
		h.ScopePredicateJSON = &s
	}
	if nameNS.Valid {
		h.Name = nameNS.String
	}
	if defPriority.Valid {
		v := defPriority.Float64
		h.DefaultPriority = &v
	}
	if sortOrder.Valid {
		v := int(sortOrder.Int64)
		h.SortOrder = &v
	}
	if promptID.Valid {
		h.PromptID = promptID.String
	}
	if breakerNS.Valid {
		v := int(breakerNS.Int64)
		h.BreakerThreshold = &v
	}
	if minAutonomyNS.Valid {
		v := minAutonomyNS.Float64
		h.MinAutonomySuitability = &v
	}
	if h.Kind == domain.EventHandlerKindTrigger {
		h.TriggerType = domain.TriggerTypeEvent
	}
	return h, nil
}

func validateForCreateSQLite(h *domain.EventHandler) error {
	switch h.Kind {
	case domain.EventHandlerKindRule:
		if h.Name == "" {
			return errors.New("event_handlers Create: rule requires name")
		}
		if h.DefaultPriority == nil || h.SortOrder == nil {
			return errors.New("event_handlers Create: rule requires default_priority and sort_order")
		}
		if h.PromptID != "" || h.BreakerThreshold != nil || h.MinAutonomySuitability != nil {
			return errors.New("event_handlers Create: rule must not populate trigger-only fields")
		}
	case domain.EventHandlerKindTrigger:
		if h.PromptID == "" {
			return errors.New("event_handlers Create: trigger requires prompt_id")
		}
		if h.BreakerThreshold == nil || h.MinAutonomySuitability == nil {
			return errors.New("event_handlers Create: trigger requires breaker_threshold and min_autonomy_suitability")
		}
		if h.DefaultPriority != nil || h.SortOrder != nil || h.Name != "" {
			return errors.New("event_handlers Create: trigger must not populate rule-only fields")
		}
		if h.TriggerType == "" {
			h.TriggerType = domain.TriggerTypeEvent
		}
		if h.TriggerType != domain.TriggerTypeEvent {
			return fmt.Errorf("event_handlers Create: unsupported trigger_type %q", h.TriggerType)
		}
	default:
		return fmt.Errorf("event_handlers Create: unknown kind %q", h.Kind)
	}
	return nil
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
