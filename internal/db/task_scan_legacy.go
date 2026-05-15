package db

import (
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// taskColumnsWithEntity + taskScanState + scanTaskFields used to live
// on the pre-D2 internal/db/tasks.go alongside the raw db.* functions
// they powered. Wave 3a (SKY-283) moved TaskStore behind the
// interface; the SQLite copy of these helpers now lives in
// internal/db/sqlite/tasks.go and the Postgres copy in
// internal/db/postgres/tasks.go.
//
// internal/db/factory.go still uses these helpers because the
// FactoryReadStore migration (wave 3i / SKY-291) hasn't landed yet.
// When that wave lands and factory.go's functions move to a store,
// this file goes away with the rest of the legacy surface.
//
// Keeping these as package-private and in their own file makes the
// transitional state explicit instead of mixed in with tasks.go's
// interface definition.

const taskColumnsWithEntity = `
	t.id, t.entity_id, t.event_type, t.dedup_key, t.primary_event_id,
	t.status, t.priority_score, t.ai_summary, t.autonomy_suitability,
	t.priority_reasoning, t.scoring_status, t.severity, t.relevance_reason,
	t.source_status, t.snooze_until, t.close_reason, t.close_event_type,
	t.closed_at, t.created_at,
	t.claimed_by_agent_id, t.claimed_by_user_id,
	COALESCE(e.title, ''), COALESCE(e.url, ''), e.source_id, e.source, e.kind,
	-- Guard json_extract so malformed or empty legacy snapshots do not fail
	-- the entire task query. Missing paths and null values still fall back
	-- cleanly to 0 so subtask-less entities report "no open subtasks".
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
