package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// scoreStore is the Postgres impl of db.ScoreStore. Wired against the
// admin pool (see postgres.New): the scorer is a system service that
// must read/write every queued task in the org, regardless of who
// created each one. Running as the per-request tf_app role with RLS
// active would only show the scorer its own creator_user_id rows
// (per the tasks_select / tasks_modify policies in D3), which is
// not what we want.
//
// SQL is written fresh against D3's schema: org_id in every WHERE
// clause as defense in depth, $N placeholders, JSONB extraction for
// snapshot_json.
type scoreStore struct{ q queryer }

func newScoreStore(q queryer) db.ScoreStore { return &scoreStore{q: q} }

var _ db.ScoreStore = (*scoreStore)(nil)

func (s *scoreStore) MarkScoring(ctx context.Context, orgID string, taskIDs []string) error {
	if len(taskIDs) == 0 {
		return nil
	}
	_, err := s.q.ExecContext(ctx,
		`UPDATE tasks SET scoring_status = 'in_progress' WHERE org_id = $1 AND id = ANY($2)`,
		orgID, taskIDs)
	return err
}

func (s *scoreStore) ResetScoringToPending(ctx context.Context, orgID string, taskIDs []string) error {
	if len(taskIDs) == 0 {
		return nil
	}
	_, err := s.q.ExecContext(ctx,
		`UPDATE tasks SET scoring_status = 'pending' WHERE org_id = $1 AND id = ANY($2)`,
		orgID, taskIDs)
	return err
}

func (s *scoreStore) UpdateTaskScores(ctx context.Context, orgID string, updates []domain.TaskScoreUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	// Single UPDATE ... FROM (VALUES ...) so the whole batch lands in
	// one round-trip. Atomic by construction (single statement =
	// implicit tx in Postgres), so no inTx wrapper needed. Avoids the
	// N-round-trip-per-cycle bottleneck the per-row loop had.
	//
	// Placeholders are emitted with explicit ::uuid/::real/::text casts
	// because the VALUES literal's types are inferred from the first
	// row, and a NULL or empty string there would make later rows fail
	// to coerce. Explicit casts pin every column's type at parse time.
	var (
		rowExprs []string
		args     = []any{orgID}
		n        = 2 // $1 is orgID
	)
	for _, u := range updates {
		rowExprs = append(rowExprs, fmt.Sprintf(
			"($%d::uuid, $%d::real, $%d::real, $%d::text, $%d::text)",
			n, n+1, n+2, n+3, n+4))
		args = append(args, u.ID, u.PriorityScore, u.AutonomySuitability, u.Summary, u.PriorityReasoning)
		n += 5
	}
	query := fmt.Sprintf(`
		UPDATE tasks t
		SET priority_score = v.priority_score,
		    autonomy_suitability = v.autonomy_suitability,
		    ai_summary = v.ai_summary,
		    priority_reasoning = v.priority_reasoning,
		    scoring_status = 'scored'
		FROM (VALUES %s) AS v(id, priority_score, autonomy_suitability, ai_summary, priority_reasoning)
		WHERE t.id = v.id AND t.org_id = $1
	`, strings.Join(rowExprs, ", "))
	_, err := s.q.ExecContext(ctx, query, args...)
	return err
}

func (s *scoreStore) UnscoredTasks(ctx context.Context, orgID string) ([]domain.Task, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
		WHERE t.org_id = $1 AND t.status = 'queued' AND t.scoring_status = 'pending'
		ORDER BY t.created_at DESC
	`, orgID)
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

// --- scan helpers --------------------------------------------------
//
// TODO(SKY-246 wave 3a): these duplicate the sqlite/scores.go scan
// helpers with Postgres-specific JSONB extraction. When TaskStore
// migrates in wave 3a both backends extract their helpers to a
// shared scan file alongside their tasks.go impl, and these go away.

// pgTaskColumnsWithEntity mirrors the SQLite version with two changes:
//   - snapshot_json is JSONB (always-valid by type), so the json_valid
//     guard is unnecessary; ->> '...' returns NULL for a missing key
//     and COALESCE handles it.
//   - All other columns are name-identical to the SQLite schema.
const pgTaskColumnsWithEntity = `
	t.id, t.entity_id, t.event_type, t.dedup_key, t.primary_event_id,
	t.status, t.priority_score, t.ai_summary, t.autonomy_suitability,
	t.priority_reasoning, t.scoring_status, t.severity, t.relevance_reason,
	t.source_status, t.snooze_until, t.close_reason, t.close_event_type,
	t.closed_at, t.created_at,
	COALESCE(e.title, ''), COALESCE(e.url, ''), e.source_id, e.source, e.kind,
	COALESCE((e.snapshot_json->>'open_subtask_count')::int, 0)`

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
