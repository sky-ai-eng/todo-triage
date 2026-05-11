package sqlite

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// taskRuleStore is the SQLite impl of db.TaskRuleStore. SQL bodies are
// ported verbatim from the pre-D2 internal/db/task_rules.go +
// internal/db/tasks.go:GetEnabledRulesForEvent; behavioral changes are:
//
//   - assertLocalOrg at every method entry,
//   - context propagation on every Exec/Query/Begin,
//   - Reorder wraps the per-id UPDATE loop in inTx so a mid-stream
//     failure doesn't leave half the list re-ordered.
type taskRuleStore struct{ q queryer }

func newTaskRuleStore(q queryer) db.TaskRuleStore { return &taskRuleStore{q: q} }

var _ db.TaskRuleStore = (*taskRuleStore)(nil)

// taskRuleColumns matches the legacy const verbatim so scan order
// stays aligned with rows.Scan / row.Scan on the same column list.
const taskRuleColumns = `id, event_type, scope_predicate_json, enabled, name,
       default_priority, sort_order, source, created_at, updated_at`

func (s *taskRuleStore) Seed(ctx context.Context, orgID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	now := time.Now().UTC()
	var inserted int64
	for _, r := range db.ShippedTaskRules {
		var pred any
		if r.Predicate == "" {
			pred = nil
		} else {
			pred = r.Predicate
		}
		res, err := s.q.ExecContext(ctx, `
			INSERT OR IGNORE INTO task_rules
				(id, event_type, scope_predicate_json, enabled, name, default_priority, sort_order, source, created_at, updated_at)
			VALUES (?, ?, ?, 1, ?, ?, ?, 'system', ?, ?)
		`, r.ID, r.EventType, pred, r.Name, r.DefaultPriority, r.SortOrder, now, now)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		inserted += n
	}
	log.Printf("[db] seeded %d new task_rules (%d already existed)", inserted, int64(len(db.ShippedTaskRules))-inserted)
	return nil
}

func (s *taskRuleStore) List(ctx context.Context, orgID string) ([]domain.TaskRule, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `SELECT `+taskRuleColumns+` FROM task_rules ORDER BY sort_order ASC, name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []domain.TaskRule
	for rows.Next() {
		r, err := scanTaskRule(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

func (s *taskRuleStore) Get(ctx context.Context, orgID string, id string) (*domain.TaskRule, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `SELECT `+taskRuleColumns+` FROM task_rules WHERE id = ?`, id)
	r, err := scanTaskRuleRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *taskRuleStore) Create(ctx context.Context, orgID string, r domain.TaskRule) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO task_rules (id, event_type, scope_predicate_json, enabled, name,
		                        default_priority, sort_order, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'user', ?, ?)
	`, r.ID, r.EventType, r.ScopePredicateJSON, r.Enabled, r.Name,
		r.DefaultPriority, r.SortOrder, now, now)
	return err
}

func (s *taskRuleStore) Update(ctx context.Context, orgID string, r domain.TaskRule) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE task_rules
		SET scope_predicate_json = ?, enabled = ?, name = ?,
		    default_priority = ?, sort_order = ?, updated_at = ?
		WHERE id = ?
	`, r.ScopePredicateJSON, r.Enabled, r.Name,
		r.DefaultPriority, r.SortOrder, time.Now().UTC(), r.ID)
	return err
}

func (s *taskRuleStore) SetEnabled(ctx context.Context, orgID string, id string, enabled bool) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE task_rules SET enabled = ?, updated_at = ? WHERE id = ?
	`, enabled, time.Now().UTC(), id)
	return err
}

func (s *taskRuleStore) Delete(ctx context.Context, orgID string, id string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `DELETE FROM task_rules WHERE id = ?`, id)
	return err
}

func (s *taskRuleStore) Reorder(ctx context.Context, orgID string, ids []string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	return inTx(ctx, s.q, func(q queryer) error {
		now := time.Now().UTC()
		for i, id := range ids {
			if _, err := q.ExecContext(ctx,
				`UPDATE task_rules SET sort_order = ?, updated_at = ? WHERE id = ?`,
				i, now, id,
			); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *taskRuleStore) GetEnabledForEvent(ctx context.Context, orgID string, eventType string) ([]domain.TaskRule, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+taskRuleColumns+`
		FROM task_rules
		WHERE event_type = ? AND enabled = 1
	`, eventType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []domain.TaskRule
	for rows.Next() {
		r, err := scanTaskRule(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// scanTaskRule scans a TaskRule from a sql.Rows iterator. Column
// order must match taskRuleColumns.
func scanTaskRule(rows *sql.Rows) (domain.TaskRule, error) {
	var r domain.TaskRule
	err := rows.Scan(&r.ID, &r.EventType, &r.ScopePredicateJSON, &r.Enabled, &r.Name,
		&r.DefaultPriority, &r.SortOrder, &r.Source, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// scanTaskRuleRow scans a TaskRule from a single sql.Row.
func scanTaskRuleRow(row *sql.Row) (domain.TaskRule, error) {
	var r domain.TaskRule
	err := row.Scan(&r.ID, &r.EventType, &r.ScopePredicateJSON, &r.Enabled, &r.Name,
		&r.DefaultPriority, &r.SortOrder, &r.Source, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}
