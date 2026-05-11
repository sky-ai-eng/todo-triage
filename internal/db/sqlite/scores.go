package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// scoreStore is the SQLite impl of db.ScoreStore. SQL bodies are
// moved verbatim from the pre-D2 internal/db/scores.go; the only
// behavioral change is the orgID assertion at each method entry.
type scoreStore struct{ q queryer }

func newScoreStore(q queryer) db.ScoreStore { return &scoreStore{q: q} }

// Compile-time check that scoreStore satisfies the interface — the
// build breaks immediately if the interface gains a method this impl
// hasn't grown.
var _ db.ScoreStore = (*scoreStore)(nil)

func (s *scoreStore) MarkScoring(ctx context.Context, orgID string, taskIDs []string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	for _, id := range taskIDs {
		if _, err := s.q.ExecContext(ctx, `UPDATE tasks SET scoring_status = 'in_progress' WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *scoreStore) ResetScoringToPending(ctx context.Context, orgID string, taskIDs []string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	for _, id := range taskIDs {
		if _, err := s.q.ExecContext(ctx, `UPDATE tasks SET scoring_status = 'pending' WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *scoreStore) UpdateTaskScores(ctx context.Context, orgID string, updates []domain.TaskScoreUpdate) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	// Single-stmt loop inside whatever tx (or DB) backs s.q. Callers
	// wanting atomicity across the batch route through Stores.Tx.WithTx
	// — outside a tx we still want per-row durability rather than
	// "best effort" partial application, so the loop is fine.
	stmt, err := prepareContext(ctx, s.q, `
		UPDATE tasks
		SET priority_score = ?, autonomy_suitability = ?, ai_summary = ?,
		    priority_reasoning = ?, scoring_status = 'scored'
		WHERE id = ?
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, u := range updates {
		if _, err := stmt.ExecContext(ctx, u.PriorityScore, u.AutonomySuitability, u.Summary, u.PriorityReasoning, u.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *scoreStore) UnscoredTasks(ctx context.Context, orgID string) ([]domain.Task, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.status = 'queued' AND t.scoring_status = 'pending'
		ORDER BY t.created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []domain.Task
	for rows.Next() {
		var t domain.Task
		if err := scanTaskFields(rows, &t); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// assertLocalOrg returns an error if the caller passed anything other
// than runmode.LocalDefaultOrg. Local-mode SQLite tables have no org_id
// column, so any non-default value indicates a confused caller and
// must be rejected loudly.
func assertLocalOrg(orgID string) error {
	if orgID != runmode.LocalDefaultOrg {
		return fmt.Errorf("sqlite store: orgID must be %q in local mode, got %q", runmode.LocalDefaultOrg, orgID)
	}
	return nil
}

// prepareContext smooths over the fact that *sql.DB and *sql.Tx both
// expose PrepareContext but Go's stdlib doesn't give us a common
// interface for it. We type-switch on the queryer; both branches are
// exhaustive for the concrete types we ever construct.
func prepareContext(ctx context.Context, q queryer, query string) (*sql.Stmt, error) {
	switch v := q.(type) {
	case *sql.DB:
		return v.PrepareContext(ctx, query)
	case *sql.Tx:
		return v.PrepareContext(ctx, query)
	default:
		return nil, fmt.Errorf("sqlite store: unexpected queryer type %T", q)
	}
}

// --- scan helpers --------------------------------------------------
//
// TODO(SKY-246 wave 3a): these duplicate the unexported helpers
// (taskColumnsWithEntity, taskScanState, scanFields) currently in
// internal/db/tasks.go. When TaskStore migrates in wave 3a the
// helpers consolidate under sqlite/tasks_scan.go and this duplication
// goes away. Kept local for wave 0 so the pilot doesn't churn the
// still-unconverted package db.

const sqliteTaskColumnsWithEntity = `
	t.id, t.entity_id, t.event_type, t.dedup_key, t.primary_event_id,
	t.status, t.priority_score, t.ai_summary, t.autonomy_suitability,
	t.priority_reasoning, t.scoring_status, t.severity, t.relevance_reason,
	t.source_status, t.snooze_until, t.close_reason, t.close_event_type,
	t.closed_at, t.created_at,
	COALESCE(e.title, ''), COALESCE(e.url, ''), e.source_id, e.source, e.kind,
	COALESCE(
		CASE
			WHEN json_valid(NULLIF(e.snapshot_json, ''))
				THEN json_extract(NULLIF(e.snapshot_json, ''), '$.open_subtask_count')
			ELSE NULL
		END,
		0
	)`

type taskScanState struct {
	priorityScore, autonomySuitability sql.NullFloat64
	aiSummary, priorityReasoning       sql.NullString
	severity, relevanceReason          sql.NullString
	sourceStatus, scoringStatus        sql.NullString
	closeReason, closeEventType        sql.NullString
	snoozeUntil, closedAt              sql.NullTime
}

func (s *taskScanState) targets(t *domain.Task) []any {
	return []any{
		&t.ID, &t.EntityID, &t.EventType, &t.DedupKey, &t.PrimaryEventID,
		&t.Status, &s.priorityScore, &s.aiSummary, &s.autonomySuitability,
		&s.priorityReasoning, &s.scoringStatus, &s.severity, &s.relevanceReason,
		&s.sourceStatus, &s.snoozeUntil, &s.closeReason, &s.closeEventType,
		&s.closedAt, &t.CreatedAt,
		&t.Title, &t.SourceURL, &t.EntitySourceID, &t.EntitySource, &t.EntityKind,
		&t.OpenSubtaskCount,
	}
}

func (s *taskScanState) finalize(t *domain.Task) {
	if s.priorityScore.Valid {
		t.PriorityScore = &s.priorityScore.Float64
	}
	if s.autonomySuitability.Valid {
		t.AutonomySuitability = &s.autonomySuitability.Float64
	}
	t.AISummary = s.aiSummary.String
	t.PriorityReasoning = s.priorityReasoning.String
	t.Severity = s.severity.String
	t.RelevanceReason = s.relevanceReason.String
	t.SourceStatus = s.sourceStatus.String
	t.ScoringStatus = s.scoringStatus.String
	t.CloseReason = s.closeReason.String
	t.CloseEventType = s.closeEventType.String
	if s.snoozeUntil.Valid {
		t.SnoozeUntil = &s.snoozeUntil.Time
	}
	if s.closedAt.Valid {
		t.ClosedAt = &s.closedAt.Time
	}
}

func scanTaskFields(rows *sql.Rows, t *domain.Task) error {
	var s taskScanState
	if err := rows.Scan(s.targets(t)...); err != nil {
		return err
	}
	s.finalize(t)
	return nil
}
