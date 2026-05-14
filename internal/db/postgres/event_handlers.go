package postgres

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

// eventHandlerStore is the unified Postgres impl of db.EventHandlerStore.
// Replaces taskRuleStore + triggerStore from before SKY-259.
//
// Per-kind fields are nullable on the column level; the per-kind CHECK
// constraints on event_handlers enforce the shape pair (rule populates
// name/default_priority/sort_order, trigger populates prompt_id/
// breaker_threshold/min_autonomy_suitability). This impl branches on
// the row's Kind where the SQL diverges.
//
// # Pool split (unchanged from the pre-SKY-259 stores)
//
//   - app   — tf_app, RLS-active. Every CRUD method runs here.
//   - admin — supabase_admin, BYPASSRLS. Seed runs here because the
//     event_handlers_insert / event_handlers_update RLS policies gate
//     on creator_user_id = tf.current_user_id() (or admin-on-org for
//     UPDATE); boot-time Seed has no JWT claims and would otherwise
//     fail the WITH CHECK on every shipped row.
//
// Inside WithTx both fields point at the same *sql.Tx, and inTx is
// true. Seed inside WithTx is rejected: escaping to the admin pool
// would break the caller's transaction scope (matches PromptStore +
// the predecessor stores).
type eventHandlerStore struct {
	app   queryer
	admin queryer
	inTx  bool
}

func newEventHandlerStore(app, admin queryer) db.EventHandlerStore {
	return &eventHandlerStore{app: app, admin: admin}
}

// newTxEventHandlerStore composes a tx-bound EventHandlerStore for
// WithTx / NewForTx. Both pools collapse onto the caller's tx; inTx=true
// makes Seed refuse rather than silently bypass the tx scope.
func newTxEventHandlerStore(tx queryer) db.EventHandlerStore {
	return &eventHandlerStore{app: tx, admin: tx, inTx: true}
}

var _ db.EventHandlerStore = (*eventHandlerStore)(nil)

// pgEventHandlerColumns mirrors the unified row. Per-kind nullable
// columns (name + default_priority + sort_order for rules; prompt_id
// + breaker_threshold + min_autonomy_suitability for triggers) are
// scanned via sql.Null* and mapped to the domain type's pointer fields.
const pgEventHandlerColumns = `id, kind, event_type, scope_predicate_json::text, enabled, source,
       name, default_priority, sort_order,
       prompt_id, breaker_threshold, min_autonomy_suitability,
       created_at, updated_at`

func (s *eventHandlerStore) Seed(ctx context.Context, orgID string) error {
	if s.inTx {
		return errors.New("postgres event_handlers: Seed must not be called inside WithTx; call stores.EventHandlers.Seed directly")
	}
	now := time.Now().UTC()
	var inserted int64
	for _, h := range db.ShippedEventHandlers {
		var pred any
		if h.Predicate != "" {
			pred = h.Predicate
		}

		switch h.Kind {
		case domain.EventHandlerKindRule:
			// Rule: name + default_priority + sort_order populated;
			// trigger-only columns NULL. Shipped rules are
			// source='system', visibility='org', creator NULL.
			// enabled=TRUE (rules are not opt-in).
			res, err := s.admin.ExecContext(ctx, `
				INSERT INTO event_handlers
					(id, org_id, creator_user_id, kind, event_type,
					 scope_predicate_json, enabled, source, visibility,
					 name, default_priority, sort_order,
					 created_at, updated_at)
				VALUES (
					$1, $2, NULL, 'rule', $3,
					$4::jsonb, TRUE, 'system', 'org',
					$5, $6, $7,
					$8, $8
				)
				ON CONFLICT (org_id, id) DO NOTHING
			`, h.UUIDFor(orgID), orgID, h.EventType,
				pred, h.Name, h.DefaultPriority, h.SortOrder, now)
			if err != nil {
				return fmt.Errorf("seed event_handler rule %s: %w", h.ID, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			inserted += n

		case domain.EventHandlerKindTrigger:
			// Trigger: prompt_id + breaker_threshold +
			// min_autonomy_suitability populated; rule-only columns NULL.
			// Shipped triggers ship disabled (project convention —
			// users opt in).
			res, err := s.admin.ExecContext(ctx, `
				INSERT INTO event_handlers
					(id, org_id, creator_user_id, kind, event_type,
					 scope_predicate_json, enabled, source, visibility,
					 prompt_id, breaker_threshold, min_autonomy_suitability,
					 created_at, updated_at)
				VALUES (
					$1, $2, NULL, 'trigger', $3,
					$4::jsonb, FALSE, 'system', 'org',
					$5, $6, $7,
					$8, $8
				)
				ON CONFLICT (org_id, id) DO NOTHING
			`, h.UUIDFor(orgID), orgID, h.EventType,
				pred, h.PromptID, h.BreakerThreshold, h.MinAutonomySuitability, now)
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
	log.Printf("[db/pg] seeded %d new event_handlers (%d already existed)", inserted, int64(len(db.ShippedEventHandlers))-inserted)
	return nil
}

func (s *eventHandlerStore) List(ctx context.Context, orgID string, kind string) ([]domain.EventHandler, error) {
	query, args := s.buildListQuery(orgID, kind, "")
	return s.scanList(ctx, query, args)
}

func (s *eventHandlerStore) Get(ctx context.Context, orgID, id string) (*domain.EventHandler, error) {
	if !isValidUUID(id) {
		return nil, nil
	}
	row := s.app.QueryRowContext(ctx, `
		SELECT `+pgEventHandlerColumns+`
		FROM event_handlers
		WHERE org_id = $1 AND id = $2
	`, orgID, id)
	h, err := scanEventHandlerRowPG(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

func (s *eventHandlerStore) GetEnabledForEvent(ctx context.Context, orgID, eventType string) ([]domain.EventHandler, error) {
	// kind ordering: 'rule' < 'trigger' alphabetically, so a plain
	// ORDER BY kind ASC keeps the rules-before-triggers invariant
	// that the router relies on (same observable behavior as the
	// pre-unification two-phase loop). sort_order then breaks ties
	// among rules; created_at DESC orders triggers.
	rows, err := s.app.QueryContext(ctx, `
		SELECT `+pgEventHandlerColumns+`
		FROM event_handlers
		WHERE org_id = $1 AND event_type = $2 AND enabled = TRUE
		ORDER BY kind ASC,
		         CASE WHEN kind = 'rule' THEN sort_order ELSE 0 END ASC,
		         created_at DESC
	`, orgID, eventType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEventHandlers(rows)
}

func (s *eventHandlerStore) ListForPrompt(ctx context.Context, orgID, promptID string) ([]domain.EventHandler, error) {
	rows, err := s.app.QueryContext(ctx, `
		SELECT `+pgEventHandlerColumns+`
		FROM event_handlers
		WHERE org_id = $1 AND prompt_id = $2 AND kind = 'trigger'
		ORDER BY created_at DESC
	`, orgID, promptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEventHandlers(rows)
}

func (s *eventHandlerStore) Create(ctx context.Context, orgID string, h domain.EventHandler) error {
	if err := db.ValidateEventHandlerForCreate(&h); err != nil {
		return err
	}
	var pred any
	if h.ScopePredicateJSON != nil {
		pred = *h.ScopePredicateJSON
	}

	// Post-SKY-262, user-source event_handlers default to visibility=
	// 'team' and the team_visibility_requires_team CHECK forces team_id
	// IS NOT NULL whenever visibility='team'. event_handlers.team_id
	// itself stays nullable at the column level so shipped system rows
	// (creator_user_id NULL + visibility='org' + team_id NULL) remain
	// valid. team_id below is derived from the caller's primary team
	// membership; admin/test fallback picks any team in the org.
	switch h.Kind {
	case domain.EventHandlerKindRule:
		_, err := s.app.ExecContext(ctx, `
			INSERT INTO event_handlers
				(id, org_id, creator_user_id, team_id, visibility, kind, event_type,
				 scope_predicate_json, enabled, source,
				 name, default_priority, sort_order,
				 created_at, updated_at)
			VALUES (
				$1, $2,
				COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
				COALESCE(
					(SELECT m.team_id FROM memberships m
					   JOIN teams t ON t.id = m.team_id
					  WHERE m.user_id = tf.current_user_id() AND t.org_id = $2
					  ORDER BY m.created_at ASC LIMIT 1),
					(SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1)
				),
				'team',
				'rule', $3,
				$4::jsonb, $5, 'user',
				$6, $7, $8,
				now(), now()
			)
		`, h.ID, orgID, h.EventType, pred, h.Enabled,
			h.Name, derefFloat(h.DefaultPriority), derefInt(h.SortOrder))
		return err

	case domain.EventHandlerKindTrigger:
		_, err := s.app.ExecContext(ctx, `
			INSERT INTO event_handlers
				(id, org_id, creator_user_id, team_id, visibility, kind, event_type,
				 scope_predicate_json, enabled, source,
				 prompt_id, breaker_threshold, min_autonomy_suitability,
				 created_at, updated_at)
			VALUES (
				$1, $2,
				COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
				COALESCE(
					(SELECT m.team_id FROM memberships m
					   JOIN teams t ON t.id = m.team_id
					  WHERE m.user_id = tf.current_user_id() AND t.org_id = $2
					  ORDER BY m.created_at ASC LIMIT 1),
					(SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1)
				),
				'team',
				'trigger', $3,
				$4::jsonb, $5, 'user',
				$6, $7, $8,
				now(), now()
			)
		`, h.ID, orgID, h.EventType, pred, h.Enabled,
			h.PromptID, derefInt(h.BreakerThreshold), derefFloat(h.MinAutonomySuitability))
		return err
	}
	return fmt.Errorf("postgres event_handlers Create: unknown kind %q", h.Kind)
}

func (s *eventHandlerStore) Update(ctx context.Context, orgID string, h domain.EventHandler) error {
	if !isValidUUID(h.ID) {
		return nil
	}
	if err := db.ValidateEventHandlerForCreate(&h); err != nil {
		return err
	}
	var pred any
	if h.ScopePredicateJSON != nil {
		pred = *h.ScopePredicateJSON
	}

	switch h.Kind {
	case domain.EventHandlerKindRule:
		_, err := s.app.ExecContext(ctx, `
			UPDATE event_handlers
			SET scope_predicate_json = $1::jsonb, enabled = $2,
			    name = $3, default_priority = $4, sort_order = $5,
			    updated_at = now()
			WHERE org_id = $6 AND id = $7 AND kind = 'rule'
		`, pred, h.Enabled, h.Name,
			derefFloat(h.DefaultPriority), derefInt(h.SortOrder),
			orgID, h.ID)
		return err

	case domain.EventHandlerKindTrigger:
		// prompt_id is immutable on trigger update — change requires
		// delete + recreate (handler enforces).
		_, err := s.app.ExecContext(ctx, `
			UPDATE event_handlers
			SET scope_predicate_json = $1::jsonb, enabled = $2,
			    breaker_threshold = $3, min_autonomy_suitability = $4,
			    updated_at = now()
			WHERE org_id = $5 AND id = $6 AND kind = 'trigger'
		`, pred, h.Enabled,
			derefInt(h.BreakerThreshold), derefFloat(h.MinAutonomySuitability),
			orgID, h.ID)
		return err
	}
	return fmt.Errorf("postgres event_handlers Update: unknown kind %q", h.Kind)
}

func (s *eventHandlerStore) SetEnabled(ctx context.Context, orgID, id string, enabled bool) error {
	if !isValidUUID(id) {
		return nil
	}
	_, err := s.app.ExecContext(ctx, `
		UPDATE event_handlers SET enabled = $1, updated_at = now() WHERE org_id = $2 AND id = $3
	`, enabled, orgID, id)
	return err
}

func (s *eventHandlerStore) Delete(ctx context.Context, orgID, id string) error {
	if !isValidUUID(id) {
		return nil
	}
	_, err := s.app.ExecContext(ctx, `DELETE FROM event_handlers WHERE org_id = $1 AND id = $2`, orgID, id)
	return err
}

func (s *eventHandlerStore) Reorder(ctx context.Context, orgID string, ids []string) error {
	return s.runInTx(ctx, func(tx *sql.Tx) error {
		for i, id := range ids {
			if !isValidUUID(id) {
				continue
			}
			// kind='rule' filter ensures trigger IDs in the list are
			// silently skipped — sort_order is rule-only by CHECK
			// constraint and a trigger row's sort_order is NULL.
			if _, err := tx.ExecContext(ctx, `
				UPDATE event_handlers SET sort_order = $1, updated_at = now()
				WHERE org_id = $2 AND id = $3 AND kind = 'rule'
			`, i, orgID, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *eventHandlerStore) Promote(ctx context.Context, orgID string, id string, t domain.EventHandler) error {
	if !isValidUUID(id) {
		return errors.New("postgres event_handlers Promote: invalid id")
	}
	if t.Kind != domain.EventHandlerKindTrigger {
		return errors.New("postgres event_handlers Promote: target kind must be 'trigger'")
	}
	if t.PromptID == "" || t.BreakerThreshold == nil || t.MinAutonomySuitability == nil {
		return errors.New("postgres event_handlers Promote: trigger fields required (prompt_id, breaker_threshold, min_autonomy_suitability)")
	}
	var pred any
	if t.ScopePredicateJSON != nil {
		pred = *t.ScopePredicateJSON
	}
	// Single UPDATE: clear rule-only fields, populate trigger-only,
	// flip kind. The per-kind CHECK constraints validate atomically —
	// any mid-state would fail the rule_shape or trigger_shape check.
	res, err := s.app.ExecContext(ctx, `
		UPDATE event_handlers
		SET kind = 'trigger',
		    prompt_id = $1, breaker_threshold = $2, min_autonomy_suitability = $3,
		    name = NULL, default_priority = NULL, sort_order = NULL,
		    scope_predicate_json = $4::jsonb,
		    updated_at = now()
		WHERE org_id = $5 AND id = $6 AND kind = 'rule'
	`, t.PromptID, *t.BreakerThreshold, *t.MinAutonomySuitability,
		pred, orgID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("postgres event_handlers Promote: row not found or not a rule")
	}
	return nil
}

// buildListQuery composes the WHERE for List, with optional kind filter.
// kind="" returns both kinds.
func (s *eventHandlerStore) buildListQuery(orgID, kind, _ string) (string, []any) {
	args := []any{orgID}
	q := `SELECT ` + pgEventHandlerColumns + `
	      FROM event_handlers
	      WHERE org_id = $1`
	if kind != "" {
		q += " AND kind = $2"
		args = append(args, kind)
	}
	// Order: rules first (sort_order ASC, name ASC), then triggers
	// (created_at DESC). Same shape as the predecessor stores' List
	// methods so handler-level callers get identical ordering.
	q += `
	      ORDER BY kind ASC,
	               CASE WHEN kind = 'rule' THEN sort_order ELSE 0 END ASC,
	               CASE WHEN kind = 'rule' THEN name ELSE '' END ASC,
	               created_at DESC`
	return q, args
}

func (s *eventHandlerStore) scanList(ctx context.Context, query string, args []any) ([]domain.EventHandler, error) {
	rows, err := s.app.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEventHandlers(rows)
}

func (s *eventHandlerStore) runInTx(ctx context.Context, fn func(*sql.Tx) error) error {
	switch v := s.app.(type) {
	case *sql.Tx:
		return fn(v)
	case *sql.DB:
		tx, err := v.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	default:
		return errors.New("postgres event_handlers: unexpected queryer type")
	}
}

func collectEventHandlers(rows *sql.Rows) ([]domain.EventHandler, error) {
	var out []domain.EventHandler
	for rows.Next() {
		h, err := scanEventHandlerPG(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func scanEventHandlerPG(rows *sql.Rows) (domain.EventHandler, error) {
	return scanEventHandlerFromAny(rows.Scan)
}

func scanEventHandlerRowPG(row *sql.Row) (domain.EventHandler, error) {
	return scanEventHandlerFromAny(row.Scan)
}

// scanEventHandlerFromAny is the shared row decoder. Per-kind nullable
// columns scan into sql.Null* / sql.NullString then map onto the
// domain pointer fields; rule rows have NULL trigger-only fields and
// vice versa.
func scanEventHandlerFromAny(scanFn func(dst ...any) error) (domain.EventHandler, error) {
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
	// TriggerType is not stored in event_handlers (v1 was always
	// 'event'); set it on read so downstream code that still inspects
	// h.TriggerType behaves identically to the pre-unification shape.
	if h.Kind == domain.EventHandlerKindTrigger {
		h.TriggerType = domain.TriggerTypeEvent
	}
	return h, nil
}

// derefFloat / derefInt unwrap nullable domain fields for INSERTs that
// have already passed db.ValidateEventHandlerForCreate (guaranteed non-nil
// for the kind's required fields).
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
