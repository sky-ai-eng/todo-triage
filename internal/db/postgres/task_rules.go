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

// taskRuleStore is the Postgres impl of db.TaskRuleStore. Mirrors the
// SQLite impl's behavior; differences vs SQLite:
//
//   - scope_predicate_json is JSONB — INSERT params cast via $N::jsonb,
//     reads scan via sql.NullString into *string. The wire shape (JSON
//     text in/out) is identical to SQLite's TEXT column.
//   - org_id NOT NULL: included in every WHERE for RLS defense-in-depth.
//   - creator_user_id NOT NULL: COALESCE(tf.current_user_id(),
//     (SELECT owner_user_id FROM orgs WHERE id = $1)) on every INSERT.
//     Request-path writes audit the caller; system-context writes
//     (Seed at boot, tests against app pool without a user claim) fall
//     back to the org founder. Same pattern as PromptStore.Create.
//   - team_id / visibility: default to NULL / 'private'. The CRUD
//     surface doesn't expose them yet (handlers don't surface
//     team-share UX in v1); SKY-242's collaboration epic owns that.
//   - source_predicate ON CONFLICT (id, org_id) DO NOTHING is the
//     Seed idempotency primitive (vs SQLite's INSERT OR IGNORE).
type taskRuleStore struct{ q queryer }

func newTaskRuleStore(q queryer) db.TaskRuleStore { return &taskRuleStore{q: q} }

var _ db.TaskRuleStore = (*taskRuleStore)(nil)

// pgTaskRuleColumns mirrors the SQLite scan list. Column order must
// match scanTaskRulePG / scanTaskRuleRowPG.
const pgTaskRuleColumns = `id, event_type, scope_predicate_json::text, enabled, name,
       default_priority, sort_order, source, created_at, updated_at`

func (s *taskRuleStore) Seed(ctx context.Context, orgID string) error {
	now := time.Now().UTC()
	var inserted int64
	for _, r := range db.ShippedTaskRules {
		// ::jsonb cast handles both NULL (when r.Predicate == "") and
		// the canonical JSON text. nil → NULL → JSONB NULL, satisfying
		// the nullable column on match-all rules.
		var pred any
		if r.Predicate == "" {
			pred = nil
		} else {
			pred = r.Predicate
		}
		// ON CONFLICT (id) DO NOTHING — task_rules.id is PRIMARY KEY
		// so a re-seed for the same org is a no-op regardless of any
		// user customizations.
		// r.UUID() — slug → deterministic UUID v5 so the same shipped
		// rule lands on the same row identity across installs and
		// across boots. The slug ("system-rule-ci-check-failed") is
		// the authoring handle; the UUID is the storage identity.
		res, err := s.q.ExecContext(ctx, `
			INSERT INTO task_rules
				(id, org_id, creator_user_id, event_type, scope_predicate_json, enabled, name, default_priority, sort_order, source, created_at, updated_at)
			VALUES (
				$1, $2,
				COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
				$3, $4::jsonb, TRUE, $5, $6, $7, 'system', $8, $8
			)
			ON CONFLICT (id) DO NOTHING
		`, r.UUID(), orgID, r.EventType, pred, r.Name, r.DefaultPriority, r.SortOrder, now)
		if err != nil {
			return fmt.Errorf("seed task_rule %s: %w", r.ID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		inserted += n
	}
	log.Printf("[db/pg] seeded %d new task_rules (%d already existed)", inserted, int64(len(db.ShippedTaskRules))-inserted)
	return nil
}

func (s *taskRuleStore) List(ctx context.Context, orgID string) ([]domain.TaskRule, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgTaskRuleColumns+`
		FROM task_rules
		WHERE org_id = $1
		ORDER BY sort_order ASC, name ASC
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []domain.TaskRule
	for rows.Next() {
		r, err := scanTaskRulePG(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

func (s *taskRuleStore) Get(ctx context.Context, orgID string, id string) (*domain.TaskRule, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT `+pgTaskRuleColumns+`
		FROM task_rules
		WHERE org_id = $1 AND id = $2
	`, orgID, id)
	r, err := scanTaskRuleRowPG(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *taskRuleStore) Create(ctx context.Context, orgID string, r domain.TaskRule) error {
	// creator_user_id resolution mirrors PromptStore.Create: prefer
	// the JWT-bound caller, fall back to org owner so system-context
	// writes (tests, future deploy-time provisioning) still satisfy
	// the NOT NULL FK with a meaningful audit target.
	var pred any
	if r.ScopePredicateJSON != nil {
		pred = *r.ScopePredicateJSON
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO task_rules
			(id, org_id, creator_user_id, event_type, scope_predicate_json, enabled, name, default_priority, sort_order, source, created_at, updated_at)
		VALUES (
			$1, $2,
			COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
			$3, $4::jsonb, $5, $6, $7, $8, 'user', now(), now()
		)
	`, r.ID, orgID, r.EventType, pred, r.Enabled, r.Name, r.DefaultPriority, r.SortOrder)
	return err
}

func (s *taskRuleStore) Update(ctx context.Context, orgID string, r domain.TaskRule) error {
	var pred any
	if r.ScopePredicateJSON != nil {
		pred = *r.ScopePredicateJSON
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE task_rules
		SET scope_predicate_json = $1::jsonb, enabled = $2, name = $3,
		    default_priority = $4, sort_order = $5, updated_at = now()
		WHERE org_id = $6 AND id = $7
	`, pred, r.Enabled, r.Name, r.DefaultPriority, r.SortOrder, orgID, r.ID)
	return err
}

func (s *taskRuleStore) SetEnabled(ctx context.Context, orgID string, id string, enabled bool) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE task_rules SET enabled = $1, updated_at = now() WHERE org_id = $2 AND id = $3
	`, enabled, orgID, id)
	return err
}

func (s *taskRuleStore) Delete(ctx context.Context, orgID string, id string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM task_rules WHERE org_id = $1 AND id = $2`, orgID, id)
	return err
}

func (s *taskRuleStore) Reorder(ctx context.Context, orgID string, ids []string) error {
	return s.runInTx(ctx, func(tx *sql.Tx) error {
		for i, id := range ids {
			if _, err := tx.ExecContext(ctx, `
				UPDATE task_rules SET sort_order = $1, updated_at = now() WHERE org_id = $2 AND id = $3
			`, i, orgID, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *taskRuleStore) GetEnabledForEvent(ctx context.Context, orgID string, eventType string) ([]domain.TaskRule, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgTaskRuleColumns+`
		FROM task_rules
		WHERE org_id = $1 AND event_type = $2 AND enabled = TRUE
	`, orgID, eventType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []domain.TaskRule
	for rows.Next() {
		r, err := scanTaskRulePG(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// runInTx mirrors the swipe-store helper: composes with the caller's
// *sql.Tx when wired inside WithTx, opens a fresh tx when wired
// against a *sql.DB. Reorder is the only multi-statement method.
func (s *taskRuleStore) runInTx(ctx context.Context, fn func(*sql.Tx) error) error {
	switch v := s.q.(type) {
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
		return errors.New("postgres task_rules: unexpected queryer type")
	}
}

// scanTaskRulePG / scanTaskRuleRowPG match the column order of
// pgTaskRuleColumns. scope_predicate_json::text is read via NullString
// then funneled into the *string domain field — JSONB NULL becomes
// nil, JSONB with content becomes the canonical text form.
func scanTaskRulePG(rows *sql.Rows) (domain.TaskRule, error) {
	var r domain.TaskRule
	var pred sql.NullString
	if err := rows.Scan(&r.ID, &r.EventType, &pred, &r.Enabled, &r.Name,
		&r.DefaultPriority, &r.SortOrder, &r.Source, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return r, err
	}
	if pred.Valid {
		s := pred.String
		r.ScopePredicateJSON = &s
	}
	return r, nil
}

func scanTaskRuleRowPG(row *sql.Row) (domain.TaskRule, error) {
	var r domain.TaskRule
	var pred sql.NullString
	if err := row.Scan(&r.ID, &r.EventType, &pred, &r.Enabled, &r.Name,
		&r.DefaultPriority, &r.SortOrder, &r.Source, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return r, err
	}
	if pred.Valid {
		s := pred.String
		r.ScopePredicateJSON = &s
	}
	return r, nil
}
