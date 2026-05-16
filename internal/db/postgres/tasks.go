package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// taskStore is the Postgres impl of db.TaskStore. SQL is written fresh
// against D3's schema: org_id in every WHERE clause as defense in depth
// alongside RLS, $N placeholders, JSONB extraction for snapshot_json.
//
// Wired against the app pool (RLS-active) — every TaskStore consumer
// is a request-handler equivalent (server tasks handler, router,
// delegate). The scorer reads tasks via the admin-pooled ScoreStore.
//
// Slice binds pass `[]string` directly into ANY($N) — pgx's
// database/sql adapter handles slice-to-array conversion natively,
// same as scoreStore.MarkScoring's taskIDs binding.
type taskStore struct{ q queryer }

func newTaskStore(q queryer) db.TaskStore { return &taskStore{q: q} }

var _ db.TaskStore = (*taskStore)(nil)

// --- Lookup ---

func (s *taskStore) Get(ctx context.Context, orgID, taskID string) (*domain.Task, error) {
	var t domain.Task
	err := scanTaskFromRow(s.q.QueryRowContext(ctx, `
		SELECT `+pgTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
		WHERE t.org_id = $1 AND t.id = $2
	`, orgID, taskID), &t)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *taskStore) Queued(ctx context.Context, orgID string) ([]domain.Task, error) {
	// SKY-261 B+ derived filter mirrors SQLite. The event_handlers
	// derived table is org-scoped so rules in another org can't
	// influence ordering — load-bearing in multi mode.
	return queryTasksCtx(ctx, s.q, `
		SELECT `+pgTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
		LEFT JOIN (
			SELECT org_id, event_type, MIN(sort_order) AS sort_order
			FROM event_handlers
			WHERE enabled = true AND kind = 'rule'
			GROUP BY org_id, event_type
		) tr ON t.event_type = tr.event_type AND t.org_id = tr.org_id
		WHERE t.org_id = $1
			AND t.status = 'queued'
			AND t.claimed_by_agent_id IS NULL
			AND t.claimed_by_user_id  IS NULL
			AND (t.snooze_until IS NULL OR t.snooze_until <= NOW())
		ORDER BY COALESCE(tr.sort_order, 999) ASC, COALESCE(t.priority_score, 0.5) DESC
	`, orgID)
}

func (s *taskStore) ByStatus(ctx context.Context, orgID, status string) ([]domain.Task, error) {
	switch status {
	case "claimed":
		return queryTasksCtx(ctx, s.q, `
			SELECT `+pgTaskColumnsWithEntity+`
			FROM tasks t
			JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
			WHERE t.org_id = $1
				AND t.claimed_by_user_id IS NOT NULL
				AND t.status NOT IN ('done', 'dismissed')
			ORDER BY COALESCE(t.priority_score, 0.5) DESC
		`, orgID)
	case "delegated":
		return queryTasksCtx(ctx, s.q, `
			SELECT `+pgTaskColumnsWithEntity+`
			FROM tasks t
			JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
			WHERE t.org_id = $1
				AND t.claimed_by_agent_id IS NOT NULL
				AND t.status NOT IN ('done', 'dismissed')
			ORDER BY COALESCE(t.priority_score, 0.5) DESC
		`, orgID)
	}
	return queryTasksCtx(ctx, s.q, `
		SELECT `+pgTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
		WHERE t.org_id = $1 AND t.status = $2
		ORDER BY COALESCE(t.priority_score, 0.5) DESC
	`, orgID, status)
}

func (s *taskStore) FindActiveByEntityAndType(ctx context.Context, orgID, entityID, eventType string) ([]domain.Task, error) {
	return queryTasksCtx(ctx, s.q, `
		SELECT `+pgTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
		WHERE t.org_id = $1 AND t.entity_id = $2 AND t.event_type = $3
			AND t.status NOT IN ('done', 'dismissed')
	`, orgID, entityID, eventType)
}

func (s *taskStore) FindActiveByEntity(ctx context.Context, orgID, entityID string) ([]domain.Task, error) {
	return queryTasksCtx(ctx, s.q, `
		SELECT `+pgTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
		WHERE t.org_id = $1 AND t.entity_id = $2
			AND t.status NOT IN ('done', 'dismissed')
	`, orgID, entityID)
}

func (s *taskStore) ListActiveRefsForEntities(ctx context.Context, orgID string, entityIDs []string) ([]domain.PendingTaskRef, error) {
	if len(entityIDs) == 0 {
		return nil, nil
	}
	// Postgres has no comparable variable-bind cap to SQLite's 999/500;
	// the array bind via lib/pq keeps the whole list in a single
	// placeholder regardless of size, so no chunking is needed.
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, entity_id, event_type, dedup_key
		FROM tasks
		WHERE org_id = $1
			AND entity_id = ANY($2)
			AND status NOT IN ('done', 'dismissed')
		ORDER BY entity_id, event_type, created_at DESC
	`, orgID, entityIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]domain.PendingTaskRef, 0, len(entityIDs))
	for rows.Next() {
		var ref domain.PendingTaskRef
		if err := rows.Scan(&ref.ID, &ref.EntityID, &ref.EventType, &ref.DedupKey); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

func (s *taskStore) EntityIDsWithActiveTasks(ctx context.Context, orgID, source string) (map[string]struct{}, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT DISTINCT t.entity_id
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
		WHERE t.org_id = $1 AND e.source = $2 AND t.status NOT IN ('done', 'dismissed')
	`, orgID, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = struct{}{}
	}
	return ids, rows.Err()
}

// --- Lifecycle ---

func (s *taskStore) FindOrCreate(ctx context.Context, orgID, teamID, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64) (*domain.Task, bool, error) {
	return s.FindOrCreateAt(ctx, orgID, teamID, entityID, eventType, dedupKey, primaryEventID, defaultPriority, time.Now())
}

func (s *taskStore) FindOrCreateAt(ctx context.Context, orgID, teamID, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64, createdAt time.Time) (*domain.Task, bool, error) {
	// SKY-295: team_id is caller-supplied. The router threads
	// handler.TeamID from the matched event_handler; the SQLite-only
	// LocalDefaultTeamID sentinel does not satisfy the tasks_insert
	// RLS policy and is filtered to empty here. Same shape ProjectStore
	// uses. Empty team_id then trips the explicit guard below.
	teamBind := teamID
	if teamBind == runmode.LocalDefaultTeamID {
		teamBind = ""
	}
	if teamBind == "" {
		return nil, false, fmt.Errorf("task store: team_id required for Postgres FindOrCreate (router must thread the user-selected team from the matched event_handler; the SQLite-only LocalDefaultTeamID sentinel does not satisfy the tasks_insert RLS policy)")
	}

	// SELECT first so the common path (task already exists) stays a
	// single round-trip. The partial unique index on
	// (org_id, entity_id, event_type, dedup_key, team_id) WHERE
	// status NOT IN ('done', 'dismissed') is the race backstop on
	// INSERT — SKY-295 made it per-team so the same event matching
	// N teams' rules fans out to N tasks.
	var existing domain.Task
	err := scanTaskFromRow(s.q.QueryRowContext(ctx, `
		SELECT `+pgTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
		WHERE t.org_id = $1 AND t.entity_id = $2 AND t.event_type = $3 AND t.dedup_key = $4
			AND t.team_id = $5::uuid
			AND t.status NOT IN ('done', 'dismissed')
		LIMIT 1
	`, orgID, entityID, eventType, dedupKey, teamBind), &existing)
	if err == nil {
		return &existing, false, nil
	}
	if err != sql.ErrNoRows {
		return nil, false, err
	}

	// ON CONFLICT keys off the partial unique index — on a lost race
	// the INSERT no-ops and we re-read.
	taskID := uuid.New().String()
	res, err := s.q.ExecContext(ctx, `
		INSERT INTO tasks (id, org_id, entity_id, event_type, dedup_key, primary_event_id,
		                   status, priority_score, scoring_status, created_at,
		                   team_id, visibility,
		                   creator_user_id)
		VALUES ($1, $2, $3, $4, $5, $6,
		        'queued', $7, 'pending', $8,
		        $9::uuid,
		        'team',
		        COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)))
		ON CONFLICT DO NOTHING
	`, taskID, orgID, entityID, eventType, dedupKey, primaryEventID, defaultPriority, createdAt, teamBind)
	if err != nil {
		return nil, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Lost the race — another goroutine inserted between our
		// SELECT and INSERT. Re-read to return the winner's row.
		var raced domain.Task
		err2 := scanTaskFromRow(s.q.QueryRowContext(ctx, `
			SELECT `+pgTaskColumnsWithEntity+`
			FROM tasks t
			JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
			WHERE t.org_id = $1 AND t.entity_id = $2 AND t.event_type = $3 AND t.dedup_key = $4
				AND t.team_id = $5::uuid
				AND t.status NOT IN ('done', 'dismissed')
			LIMIT 1
		`, orgID, entityID, eventType, dedupKey, teamBind), &raced)
		if err2 != nil {
			return nil, false, fmt.Errorf("findorcreate: race reread: %w", err2)
		}
		return &raced, false, nil
	}

	task, err := s.Get(ctx, orgID, taskID)
	if err != nil {
		return nil, false, err
	}
	return task, true, nil
}

func (s *taskStore) Bump(ctx context.Context, orgID, taskID, eventID string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		SET status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END,
		    snooze_until = CASE WHEN status = 'snoozed' THEN NULL ELSE snooze_until END
		WHERE org_id = $1 AND id = $2
	`, orgID, taskID)
	return err
}

func (s *taskStore) Close(ctx context.Context, orgID, taskID, closeReason, closeEventType string) error {
	var cet any
	if closeEventType != "" {
		cet = closeEventType
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE tasks SET status = 'done', close_reason = $1, close_event_type = $2,
		                 closed_at = NOW()
		WHERE org_id = $3 AND id = $4 AND status NOT IN ('done', 'dismissed')
	`, closeReason, cet, orgID, taskID)
	return err
}

func (s *taskStore) CloseAllForEntity(ctx context.Context, orgID, entityID, closeReason string) (int, error) {
	res, err := s.q.ExecContext(ctx, `
		UPDATE tasks SET status = 'done', close_reason = $1, closed_at = NOW()
		WHERE org_id = $2 AND entity_id = $3 AND status NOT IN ('done', 'dismissed')
	`, closeReason, orgID, entityID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *taskStore) SetStatus(ctx context.Context, orgID, taskID, status string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE tasks SET status = $1 WHERE org_id = $2 AND id = $3
	`, status, orgID, taskID)
	return err
}

func (s *taskStore) RecordEvent(ctx context.Context, orgID, taskID, eventID, kind string) error {
	// task_events has org_id NOT NULL in D3 schema; populate it so the
	// composite FK to (org_id, task_id) resolves.
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO task_events (org_id, task_id, event_id, kind, created_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT DO NOTHING
	`, orgID, taskID, eventID, kind)
	return err
}

// --- Claim mutations ---

func (s *taskStore) SetClaimedByAgent(ctx context.Context, orgID, taskID, agentID string) error {
	var a any
	if agentID != "" {
		a = agentID
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_agent_id = $1,
		       claimed_by_user_id  = NULL
		 WHERE org_id = $2 AND id = $3
	`, a, orgID, taskID)
	return err
}

func (s *taskStore) SetClaimedByUser(ctx context.Context, orgID, taskID, userID string) error {
	var u any
	if userID != "" {
		u = userID
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_user_id  = $1,
		       claimed_by_agent_id = NULL
		 WHERE org_id = $2 AND id = $3
	`, u, orgID, taskID)
	return err
}

func (s *taskStore) StampAgentClaimIfUnclaimed(ctx context.Context, orgID, taskID, agentID string) (bool, error) {
	if agentID == "" {
		return false, errors.New("StampAgentClaimIfUnclaimed: empty agentID")
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_agent_id = $1,
		       snooze_until = NULL,
		       status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END
		 WHERE org_id = $2 AND id = $3
		   AND claimed_by_user_id IS NULL
		   AND (claimed_by_agent_id IS NULL OR claimed_by_agent_id != $1)
		   AND status NOT IN ('done', 'dismissed')
	`, agentID, orgID, taskID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *taskStore) HandoffAgentClaim(ctx context.Context, orgID, taskID, agentID, userID string) (db.HandoffResult, error) {
	if agentID == "" {
		return db.HandoffRefused, errors.New("HandoffAgentClaim: empty agentID")
	}
	if userID == "" {
		return db.HandoffRefused, errors.New("HandoffAgentClaim: empty userID")
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_agent_id = $1,
		       claimed_by_user_id  = NULL,
		       snooze_until = NULL,
		       status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END
		 WHERE org_id = $2 AND id = $3
		   AND (claimed_by_user_id  IS NULL OR claimed_by_user_id  = $4)
		   AND (claimed_by_agent_id IS NULL OR claimed_by_agent_id != $1)
		   AND status NOT IN ('done', 'dismissed')
	`, agentID, orgID, taskID, userID)
	if err != nil {
		return db.HandoffRefused, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return db.HandoffRefused, err
	}
	if n > 0 {
		return db.HandoffChanged, nil
	}
	// 0 rows — re-read to distinguish refused vs no-op. Terminal-status
	// check takes precedence over same-agent so a sticky-claim on a
	// closed task doesn't fall through to HandoffNoOp.
	var curUser, curAgent sql.NullString
	var curStatus string
	err = s.q.QueryRowContext(ctx,
		`SELECT claimed_by_user_id, claimed_by_agent_id, status FROM tasks WHERE org_id = $1 AND id = $2`,
		orgID, taskID,
	).Scan(&curUser, &curAgent, &curStatus)
	if err == sql.ErrNoRows {
		return db.HandoffRefused, nil
	}
	if err != nil {
		return db.HandoffRefused, err
	}
	if curStatus == "done" || curStatus == "dismissed" {
		return db.HandoffRefused, nil
	}
	if curAgent.Valid && curAgent.String == agentID {
		return db.HandoffNoOp, nil
	}
	return db.HandoffRefused, nil
}

func (s *taskStore) TakeoverClaimFromAgent(ctx context.Context, orgID, taskID, userID string) (bool, error) {
	if userID == "" {
		return false, errors.New("TakeoverClaimFromAgent: empty userID")
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_user_id  = $1,
		       claimed_by_agent_id = NULL,
		       snooze_until = NULL,
		       status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END
		 WHERE org_id = $2 AND id = $3
		   AND claimed_by_agent_id IS NOT NULL
		   AND claimed_by_user_id  IS NULL
		   AND status NOT IN ('done', 'dismissed')
	`, userID, orgID, taskID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *taskStore) ClaimQueuedForUser(ctx context.Context, orgID, taskID, userID string) (bool, error) {
	if userID == "" {
		return false, errors.New("ClaimQueuedForUser: empty userID is not a valid claimant")
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_user_id = $1,
		       snooze_until = NULL,
		       status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END
		 WHERE org_id = $2 AND id = $3
		   AND status IN ('queued', 'snoozed')
		   AND claimed_by_user_id  IS NULL
		   AND claimed_by_agent_id IS NULL
	`, userID, orgID, taskID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// --- Breaker ---

func (s *taskStore) CountConsecutiveFailedRuns(ctx context.Context, orgID, entityID, promptID string) (int, error) {
	// Same shape as SQLite. Postgres' ROW_NUMBER / OVER / CTEs are
	// identical syntax-wise; the only difference is the started_at
	// fallback literal (Postgres requires a typed cast on the
	// '1970-01-01' default for the comparison).
	var count int
	err := s.q.QueryRowContext(ctx, `
		WITH recent AS (
			SELECT
				CASE
					WHEN r.chain_run_id IS NULL THEN 'leaf'
					ELSE 'chain'
				END AS kind,
				r.chain_run_id,
				COALESCE(cr.status, r.status) AS status,
				COALESCE(cr.started_at, r.started_at) AS started_at,
				ROW_NUMBER() OVER (
					PARTITION BY COALESCE(r.chain_run_id, r.id)
					ORDER BY r.started_at ASC
				) AS step_rank
			FROM runs r
			JOIN tasks t ON r.task_id = t.id AND r.org_id = t.org_id
			LEFT JOIN chain_runs cr ON cr.id = r.chain_run_id AND cr.org_id = r.org_id
			WHERE r.org_id = $1
				AND t.entity_id = $2
				AND (
					(r.chain_run_id IS NULL AND r.prompt_id = $3)
					OR (cr.chain_prompt_id = $3)
				)
				AND r.trigger_type = 'event'
		),
		dedup AS (
			SELECT status, started_at
			FROM recent
			WHERE step_rank = 1
			ORDER BY started_at DESC
			LIMIT 20
		)
		SELECT COUNT(*)
		FROM dedup
		WHERE status IN ('failed', 'task_unsolvable', 'aborted')
			AND started_at > (
				SELECT COALESCE(MAX(started_at), TIMESTAMPTZ '1970-01-01')
				FROM dedup WHERE status = 'completed'
			)
	`, orgID, entityID, promptID).Scan(&count)
	return count, err
}

// --- Internal helpers ---

// pgTaskColumnsWithEntity is the canonical column list for every task
// query that feeds scanTaskFields. Owned here on TaskStore;
// ScoreStore.UnscoredTasks references it via the same-package import.
//
// Two notable differences from SQLite:
//   - snapshot_json is JSONB (always-valid by type), so the json_valid
//     guard is unnecessary; ->> '...' returns NULL for a missing key
//     and COALESCE picks the 0 default.
//   - Time columns are TIMESTAMPTZ; sql.NullTime scans them directly.
const pgTaskColumnsWithEntity = `
	t.id, t.entity_id, t.event_type, t.dedup_key, t.primary_event_id,
	t.status, t.priority_score, t.ai_summary, t.autonomy_suitability,
	t.priority_reasoning, t.scoring_status, t.severity, t.relevance_reason,
	t.source_status, t.snooze_until, t.close_reason, t.close_event_type,
	t.closed_at, t.created_at,
	t.claimed_by_agent_id, t.claimed_by_user_id,
	COALESCE(e.title, ''), COALESCE(e.url, ''), e.source_id, e.source, e.kind,
	COALESCE((e.snapshot_json->>'open_subtask_count')::int, 0)`

type taskScanState struct {
	priorityScore, autonomySuitability sql.NullFloat64
	aiSummary, priorityReasoning       sql.NullString
	severity, relevanceReason          sql.NullString
	sourceStatus, scoringStatus        sql.NullString
	closeReason, closeEventType        sql.NullString
	snoozeUntil, closedAt              sql.NullTime
	claimedByAgentID, claimedByUserID  sql.NullString
}

func (s *taskScanState) targets(t *domain.Task) []any {
	return []any{
		&t.ID, &t.EntityID, &t.EventType, &t.DedupKey, &t.PrimaryEventID,
		&t.Status, &s.priorityScore, &s.aiSummary, &s.autonomySuitability,
		&s.priorityReasoning, &s.scoringStatus, &s.severity, &s.relevanceReason,
		&s.sourceStatus, &s.snoozeUntil, &s.closeReason, &s.closeEventType,
		&s.closedAt, &t.CreatedAt,
		&s.claimedByAgentID, &s.claimedByUserID,
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
	t.ClaimedByAgentID = s.claimedByAgentID.String
	t.ClaimedByUserID = s.claimedByUserID.String
}

func scanTaskFields(rows *sql.Rows, t *domain.Task) error {
	var s taskScanState
	if err := rows.Scan(s.targets(t)...); err != nil {
		return err
	}
	s.finalize(t)
	return nil
}

func scanTaskFromRow(row *sql.Row, t *domain.Task) error {
	var s taskScanState
	if err := row.Scan(s.targets(t)...); err != nil {
		return err
	}
	s.finalize(t)
	return nil
}

func queryTasksCtx(ctx context.Context, q queryer, query string, args ...any) ([]domain.Task, error) {
	rows, err := q.QueryContext(ctx, query, args...)
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
