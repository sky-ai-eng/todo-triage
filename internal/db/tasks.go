package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// --- Column lists for task queries ----------------------------------------
//
// Every query that feeds into scanTask must use these columns in this order.
// The entity JOIN columns are appended for display.

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

// FindOrCreateTask implements the dedup logic via the partial unique index
// (entity_id, event_type, dedup_key) WHERE status NOT IN ('done','dismissed').
// If an active task exists, returns it with created=false. Otherwise creates
// one with created=true.
func FindOrCreateTask(db *sql.DB, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64) (*domain.Task, bool, error) {
	return FindOrCreateTaskAt(db, entityID, eventType, dedupKey, primaryEventID, defaultPriority, time.Now())
}

// FindOrCreateTaskAt is the same as FindOrCreateTask but stamps a caller-
// supplied createdAt on the new row. Used by initial-discovery backfills
// that represent activity older than "now" — e.g. a pending review request
// observed on a 2-week-old PR should show the PR's age on the card, not
// the moment we first polled. Existing tasks (find branch) keep their
// original created_at.
func FindOrCreateTaskAt(db *sql.DB, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64, createdAt time.Time) (*domain.Task, bool, error) {
	// Try to find an existing active task.
	var existing domain.Task
	err := scanTaskRow(db.QueryRow(`
		SELECT `+taskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.entity_id = ? AND t.event_type = ? AND t.dedup_key = ?
			AND t.status NOT IN ('done', 'dismissed')
		LIMIT 1
	`, entityID, eventType, dedupKey), &existing)

	if err == nil {
		return &existing, false, nil
	}
	if err != sql.ErrNoRows {
		return nil, false, err
	}

	// Create new task. If a concurrent goroutine raced us past the SELECT
	// above, the partial unique index (entity_id, event_type, dedup_key)
	// WHERE status NOT IN ('done','dismissed') will reject the INSERT. In
	// that case, re-read the winner's row.
	id := uuid.New().String()
	// team_id + visibility populated explicitly per SKY-262: post-migration
	// the team-scoped queue derived filter requires team_id on every task,
	// and 'team' is the canonical visibility. In local mode the team is
	// the LocalDefaultTeamID sentinel from SKY-269.
	_, err = db.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id,
		                   status, priority_score, scoring_status, created_at,
		                   team_id, visibility)
		VALUES (?, ?, ?, ?, ?, 'queued', ?, 'pending', ?, ?, 'team')
	`, id, entityID, eventType, dedupKey, primaryEventID, defaultPriority, createdAt, runmode.LocalDefaultTeamID)
	if err != nil {
		// Race: another goroutine created the task between our SELECT and
		// INSERT. Re-read to return the winner's row.
		var raced domain.Task
		err2 := scanTaskRow(db.QueryRow(`
			SELECT `+taskColumnsWithEntity+`
			FROM tasks t
			JOIN entities e ON t.entity_id = e.id
			WHERE t.entity_id = ? AND t.event_type = ? AND t.dedup_key = ?
				AND t.status NOT IN ('done', 'dismissed')
			LIMIT 1
		`, entityID, eventType, dedupKey), &raced)
		if err2 == nil {
			return &raced, false, nil
		}
		// Genuine error (not a race).
		return nil, false, err
	}

	task, err := GetTask(db, id)
	if err != nil {
		return nil, false, err
	}
	return task, true, nil
}

// BumpTask records a new matching event on an existing task. Does NOT update
// primary_event_id — that stays as the original spawning event (the task_events
// junction with kind=bumped tracks subsequent events). If the task is snoozed,
// un-snoozes it (wake-on-bump: the snooze premise "nothing new" is invalidated).
func BumpTask(db *sql.DB, taskID, eventID string) error {
	_, err := db.Exec(`
		UPDATE tasks
		SET status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END,
		    snooze_until = CASE WHEN status = 'snoozed' THEN NULL ELSE snooze_until END
		WHERE id = ?
	`, taskID)
	return err
}

// CloseTask sets a task to done with the given close reason. Used by run-
// completion, inline close checks, and user actions (dismiss/claim-done).
func CloseTask(db *sql.DB, taskID, closeReason, closeEventType string) error {
	now := time.Now()
	var cet *string
	if closeEventType != "" {
		cet = &closeEventType
	}
	_, err := db.Exec(`
		UPDATE tasks SET status = 'done', close_reason = ?, close_event_type = ?,
		                 closed_at = ?
		WHERE id = ? AND status NOT IN ('done', 'dismissed')
	`, closeReason, cet, now, taskID)
	return err
}

// CloseAllEntityTasks closes every active task on an entity with the given
// close reason. Returns the number of tasks closed. Used by entity lifecycle
// (close_reason="entity_closed").
func CloseAllEntityTasks(db *sql.DB, entityID, closeReason string) (int, error) {
	now := time.Now()
	result, err := db.Exec(`
		UPDATE tasks SET status = 'done', close_reason = ?, closed_at = ?
		WHERE entity_id = ? AND status NOT IN ('done', 'dismissed')
	`, closeReason, now, entityID)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// SetTaskStatus updates a task's status on the lifecycle axis only.
// Claim cols are unaffected — callers that want to change
// responsibility use the dedicated claim helpers
// (StampAgentClaimIfUnclaimed, HandoffAgentClaim, etc.). Post-SKY-261 B+
// the only production caller is revertTaskStatus in DrainEntity's
// mark-fired-failure rollback; swipe + close paths route through
// SwipeStore.RecordSwipe + CloseTask. The pre-B+ "queued→delegated /
// queued→claimed" doc was retired when status='delegated' and
// status='claimed' were dropped from the lifecycle enum.
func SetTaskStatus(db *sql.DB, taskID, status string) error {
	_, err := db.Exec(`UPDATE tasks SET status = ? WHERE id = ?`, status, taskID)
	return err
}

// SetTaskClaimedByAgent stamps the agent claim on a task (SKY-261
// D-Claims). Called by the router on trigger match at task-creation and
// by the user-delegate handler when a queued task is dragged to the
// bot. Clears any existing user claim in the same UPDATE so the
// XOR CHECK invariant holds throughout.
func SetTaskClaimedByAgent(db *sql.DB, taskID, agentID string) error {
	var claimedByAgentID any = agentID
	if agentID == "" {
		claimedByAgentID = nil
	}

	_, err := db.Exec(`
		UPDATE tasks
		   SET claimed_by_agent_id = ?,
		       claimed_by_user_id  = NULL
		 WHERE id = ?
	`, claimedByAgentID, taskID)
	return err
}

// SetTaskClaimedByUser stamps the user claim on a task (SKY-261
// D-Claims). Called by the user-claim handler when a user takes a
// queued task themselves AND by the takeover handler when a user
// reclaims a bot-running task. Clears any existing agent claim in the
// same UPDATE so XOR holds.
func SetTaskClaimedByUser(db *sql.DB, taskID, userID string) error {
	var claimedByUserID any = userID
	if userID == "" {
		// Empty string is the domain's NULL convention. Passing it raw
		// would persist "" into the column, which would violate the
		// users(id) FK on the next read and silently mis-render in any
		// JOIN. Map to nil so the column is actually NULL.
		claimedByUserID = nil
	}
	_, err := db.Exec(`
		UPDATE tasks
		   SET claimed_by_user_id  = ?,
		       claimed_by_agent_id = NULL
		 WHERE id = ?
	`, claimedByUserID, taskID)
	return err
}

// StampAgentClaimIfUnclaimed is the race-safe variant of
// SetTaskClaimedByAgent. The unconditional version overwrites any
// existing claim — including a user claim that landed during the
// race window between trigger-match and stamp. That stole the user's
// gesture; this helper refuses to.
//
// Two guards in one UPDATE:
//
//   - claimed_by_user_id IS NULL — won't steal a user claim. If the
//     user beat the bot to it, the bot's auto-trigger silently
//     loses the race (and the drain path's claim_changed guard
//     will then skip any pending firing for this task too — the
//     bot's commitment never lands at all).
//
//   - claimed_by_agent_id IS NULL OR != $1 — skip no-op rewrites
//     when the same agent already holds the claim. Returns ok=false
//     in that case so the caller can skip the broadcast — repeated
//     stamps shouldn't churn the FE with redundant task_claimed
//     events.
//
// Returns ok=true when the claim actually changed (caller should
// broadcast). ok=false (no error) means either user-already-owns
// or bot-already-owns; caller should not broadcast.
func StampAgentClaimIfUnclaimed(db *sql.DB, taskID, agentID string) (bool, error) {
	if agentID == "" {
		return false, fmt.Errorf("StampAgentClaimIfUnclaimed: empty agentID")
	}
	res, err := db.Exec(`
		UPDATE tasks
		   SET claimed_by_agent_id = ?
		 WHERE id = ?
		   AND claimed_by_user_id IS NULL
		   AND (claimed_by_agent_id IS NULL OR claimed_by_agent_id != ?)
	`, agentID, taskID, agentID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// HandoffResult discriminates the three outcomes HandoffAgentClaim
// can produce, so the caller can decide whether to broadcast on the
// claim axis, skip broadcast (no-op), or surface a refusal as 409.
type HandoffResult int

const (
	// HandoffChanged — claim actually moved (unclaimed → bot, or
	// same-user → bot transfer). Caller broadcasts task_claimed.
	HandoffChanged HandoffResult = iota
	// HandoffNoOp — same agent already owns the task. Idempotent;
	// caller skips the broadcast (and any sibling work like a
	// duplicate run spawn if applicable).
	HandoffNoOp
	// HandoffRefused — a different user owns the task (or the task
	// vanished mid-flight). Caller returns 409 — the gesture
	// shouldn't steal.
	HandoffRefused
)

// HandoffAgentClaim is the race-safe "user delegates to bot" helper.
// Three legitimate inputs all land the task in the bot's lane:
//
//   - unclaimed → bot-claimed (matches the auto-trigger-style stamp).
//   - user-claimed by THIS user → bot-claimed (explicit handoff: the
//     Board's "You → Agent" drag, the swipe-up on a user-claimed
//     card, factory drag-to-Agent on a card the user previously
//     claimed).
//   - bot-already-claimed by THIS agent → idempotent no-op.
//
// One refusal: a DIFFERENT user owns the task. The bot mustn't steal
// what another teammate took on.
//
// All four cases resolve in a single guarded UPDATE; on 0 rows
// affected the helper re-reads the row to differentiate no-op from
// refused. (At N=1 the "different agent owns it" case isn't
// reachable; if v2 multi-agent lands, that case currently falls
// through to "stamp our agent" via the agent != $1 guard — revisit
// the policy at that point.)
//
// This is distinct from StampAgentClaimIfUnclaimed: that helper is
// for the AUTO-trigger path which has no user identity to transfer
// from, so it refuses on any non-NULL claimed_by_user_id. Handoff
// is for explicit user gestures where the user has a userID and
// can legitimately transfer their own claim.
func HandoffAgentClaim(db *sql.DB, taskID, agentID, userID string) (HandoffResult, error) {
	if agentID == "" {
		return HandoffRefused, fmt.Errorf("HandoffAgentClaim: empty agentID")
	}
	if userID == "" {
		return HandoffRefused, fmt.Errorf("HandoffAgentClaim: empty userID")
	}
	res, err := db.Exec(`
		UPDATE tasks
		   SET claimed_by_agent_id = ?,
		       claimed_by_user_id  = NULL
		 WHERE id = ?
		   AND (claimed_by_user_id  IS NULL OR claimed_by_user_id  = ?)
		   AND (claimed_by_agent_id IS NULL OR claimed_by_agent_id != ?)
	`, agentID, taskID, userID, agentID)
	if err != nil {
		return HandoffRefused, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return HandoffRefused, err
	}
	if n > 0 {
		return HandoffChanged, nil
	}
	// 0 rows — re-read to figure out which guard tripped.
	var curUser, curAgent sql.NullString
	err = db.QueryRow(
		`SELECT claimed_by_user_id, claimed_by_agent_id FROM tasks WHERE id = ?`,
		taskID,
	).Scan(&curUser, &curAgent)
	if err == sql.ErrNoRows {
		return HandoffRefused, nil
	}
	if err != nil {
		return HandoffRefused, err
	}
	if curAgent.Valid && curAgent.String == agentID {
		// Same agent already owns it — idempotent.
		return HandoffNoOp, nil
	}
	// Either a different user owns it, or some other race state.
	// Treat as refused so the caller doesn't accidentally proceed
	// as if the claim landed.
	return HandoffRefused, nil
}

// TakeoverClaimFromAgent atomically flips a bot-claimed task to a
// user claim. Race-safe variant of SetTaskClaimedByUser for the
// "user swipes claim on a bot-claimed task" path: guards on the
// bot still holding the claim AND no other user owning it, so
// concurrent takeovers don't trample each other.
//
// Returns ok=true when the flip landed (caller broadcasts +
// proceeds). ok=false (no error) means the race was lost — either
// another user took over first, or the bot's claim was cleared
// (requeue) between the load and the UPDATE; caller surfaces 409
// and the FE refetches.
func TakeoverClaimFromAgent(db *sql.DB, taskID, userID string) (bool, error) {
	if userID == "" {
		return false, fmt.Errorf("TakeoverClaimFromAgent: empty userID")
	}
	res, err := db.Exec(`
		UPDATE tasks
		   SET claimed_by_user_id  = ?,
		       claimed_by_agent_id = NULL
		 WHERE id = ?
		   AND claimed_by_agent_id IS NOT NULL
		   AND claimed_by_user_id  IS NULL
	`, userID, taskID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ClaimQueuedTaskForUser is the user-claim handler's atomic
// "take this task off the queue" — succeeds only if the task is
// (a) status='queued' AND (b) currently unclaimed by anyone (both
// claim cols NULL). Returns true if the claim landed, false on any
// guard violation: the task is already claimed (race lost), or the
// task is no longer queued (snoozed / closed mid-gesture). The
// caller refetches on false and surfaces the current state.
//
// The status='queued' guard is load-bearing: without it, the
// optimistic claim could land on a snoozed (deferred) task or a
// terminal (done/dismissed) row, which is surprising — "I'll handle
// this" against a snoozed task should require requeuing it first,
// and claiming a finished task makes no semantic sense. Other claim
// transitions (swipe-delegate, takeover) operate on active-but-
// not-necessarily-queued tasks via SetTaskClaimedByUser /
// SetTaskClaimedByAgent, which don't carry this restriction.
//
// userID="" is rejected outright (returns false, error) — an empty
// claim is the same as no claim, and persisting "" would violate
// the users(id) FK on the next read. Caller must supply a real
// user id.
func ClaimQueuedTaskForUser(db *sql.DB, taskID, userID string) (bool, error) {
	if userID == "" {
		return false, fmt.Errorf("ClaimQueuedTaskForUser: empty userID is not a valid claimant")
	}
	res, err := db.Exec(`
		UPDATE tasks
		   SET claimed_by_user_id = ?
		 WHERE id = ?
		   AND status = 'queued'
		   AND claimed_by_user_id  IS NULL
		   AND claimed_by_agent_id IS NULL
	`, userID, taskID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// RecordTaskEvent inserts into the task_events junction table.
func RecordTaskEvent(db *sql.DB, taskID, eventID, kind string) error {
	_, err := db.Exec(`
		INSERT OR IGNORE INTO task_events (task_id, event_id, kind, created_at)
		VALUES (?, ?, ?, ?)
	`, taskID, eventID, kind, time.Now())
	return err
}

// FindActiveTasksByEntityAndType returns all non-terminal tasks for an entity
// matching the given event type. Used by inline close checks to find sibling
// tasks to close.
func FindActiveTasksByEntityAndType(db *sql.DB, entityID, eventType string) ([]domain.Task, error) {
	return queryTasks(db, `
		SELECT `+taskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.entity_id = ? AND t.event_type = ? AND t.status NOT IN ('done', 'dismissed')
	`, entityID, eventType)
}

// FindActiveTasksByEntity returns all non-terminal tasks for an entity,
// regardless of event type. Used by entity lifecycle to close everything.
func FindActiveTasksByEntity(db *sql.DB, entityID string) ([]domain.Task, error) {
	return queryTasks(db, `
		SELECT `+taskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.entity_id = ? AND t.status NOT IN ('done', 'dismissed')
	`, entityID)
}

// ListActiveTaskRefsForEntities returns minimal active-task refs (id,
// entity_id, event_type, dedup_key) for any entity in entityIDs. Used
// by the factory snapshot to attach pending_tasks per entity in a
// single round-trip — no entity JSON join, no json_extract for
// open_subtask_count, no priority/scoring columns.
//
// Chunks on SQLite's variable limit (500) the same way
// ListRecentEventsByEntity does.
func ListActiveTaskRefsForEntities(database *sql.DB, entityIDs []string) ([]domain.PendingTaskRef, error) {
	if len(entityIDs) == 0 {
		return nil, nil
	}
	const chunkSize = 500
	out := make([]domain.PendingTaskRef, 0, len(entityIDs))
	for start := 0; start < len(entityIDs); start += chunkSize {
		end := start + chunkSize
		if end > len(entityIDs) {
			end = len(entityIDs)
		}
		chunk := entityIDs[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		rows, err := database.Query(`
			SELECT id, entity_id, event_type, dedup_key
			FROM tasks
			WHERE entity_id IN (`+strings.Join(placeholders, ",")+`)
			  AND status NOT IN ('done', 'dismissed')
			ORDER BY entity_id, event_type, created_at DESC, rowid DESC
		`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var ref domain.PendingTaskRef
			if err := rows.Scan(&ref.ID, &ref.EntityID, &ref.EventType, &ref.DedupKey); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, ref)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// EntityIDsWithActiveTasks returns the set of entity IDs that have at least
// one non-terminal task, scoped to the given entity source (e.g. "jira").
// Used to batch-check active-task membership in one query instead of N.
func EntityIDsWithActiveTasks(db *sql.DB, source string) (map[string]struct{}, error) {
	rows, err := db.Query(`
		SELECT DISTINCT t.entity_id
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE e.source = ? AND t.status NOT IN ('done', 'dismissed')
	`, source)
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

// GetTask returns a single task by ID, joined with its entity for display fields.
func GetTask(db *sql.DB, id string) (*domain.Task, error) {
	var t domain.Task
	err := scanTaskRow(db.QueryRow(`
		SELECT `+taskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.id = ?
	`, id), &t)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// QueuedTasks returns queued tasks ordered by the matching rule's
// sort_order (category ordering) then priority_score DESC within each tier.
// JOINs entities for display; the rule_order derived table picks the
// MIN(sort_order) per (org_id, event_type) so the outer query stays
// one-row-per-task and stays tenant-correct.
//
// A direct LEFT JOIN on event_handlers would multiply each task row by the
// number of enabled kind='rule' handlers for its event_type — two rules on
// the same event_type would surface every matching task twice with
// nondeterministic ordering. The derived table collapses that to one row
// per (org_id, event_type) before the join.
//
// org_id is part of the derived-table GROUP BY + the outer ON clause so
// rules in one org can't influence task ordering in another (latent at
// N=1 in local mode today, load-bearing once multi-mode shares a DB).
func QueuedTasks(db *sql.DB) ([]domain.Task, error) {
	// SKY-261 B+ derived filter: queue = status='queued' + both claim
	// cols NULL + not future-snoozed. The claim-col exclusion is what
	// makes the queue genuinely "unclaimed work" rather than "anything
	// with status=queued" (post-B+ a user- or bot-claimed task also
	// reads status='queued', but it's owned, not in the triage queue).
	return queryTasks(db, `
		SELECT `+taskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		LEFT JOIN (
			SELECT org_id, event_type, MIN(sort_order) AS sort_order
			FROM event_handlers
			WHERE enabled = 1 AND kind = 'rule'
			GROUP BY org_id, event_type
		) tr ON t.event_type = tr.event_type AND t.org_id = tr.org_id
		WHERE t.status = 'queued'
			AND t.claimed_by_agent_id IS NULL
			AND t.claimed_by_user_id  IS NULL
			AND (t.snooze_until IS NULL OR t.snooze_until <= datetime('now'))
		ORDER BY COALESCE(tr.sort_order, 999) ASC, COALESCE(t.priority_score, 0.5) DESC
	`)
}

// TasksByStatus returns tasks with the given status, ordered by priority.
//
// SKY-261 B+: 'claimed' and 'delegated' are no longer real status values
// — they're derived filters on the claim columns. This helper preserves
// the API surface (callers can still query "?status=claimed") by
// interpreting those two values as their claim-axis equivalents:
//
//   - status="claimed"   → claimed_by_user_id IS NOT NULL + active
//   - status="delegated" → claimed_by_agent_id IS NOT NULL + active
//
// "Active" here means status NOT IN ('done', 'dismissed') so the
// per-claim views show both the in-flight and the awaiting-action
// rows; closed rows fall under status='done' via the regular path.
// The 'queued' / 'snoozed' / 'done' / 'dismissed' paths stay literal
// — those are genuine status values.
func TasksByStatus(db *sql.DB, status string) ([]domain.Task, error) {
	switch status {
	case "claimed":
		return queryTasks(db, `
			SELECT `+taskColumnsWithEntity+`
			FROM tasks t
			JOIN entities e ON t.entity_id = e.id
			WHERE t.claimed_by_user_id IS NOT NULL
				AND t.status NOT IN ('done', 'dismissed')
			ORDER BY COALESCE(t.priority_score, 0.5) DESC
		`)
	case "delegated":
		return queryTasks(db, `
			SELECT `+taskColumnsWithEntity+`
			FROM tasks t
			JOIN entities e ON t.entity_id = e.id
			WHERE t.claimed_by_agent_id IS NOT NULL
				AND t.status NOT IN ('done', 'dismissed')
			ORDER BY COALESCE(t.priority_score, 0.5) DESC
		`)
	}
	return queryTasks(db, `
		SELECT `+taskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.status = ?
		ORDER BY COALESCE(t.priority_score, 0.5) DESC
	`, status)
}

// --- Breaker queries (query-based, no counter column) --------------------

// CountConsecutiveFailedRuns counts consecutive non-success auto-runs at the
// tail of runs for (entity_id, prompt_id), stopping at the first 'completed'
// row. Used by the router to check the breaker threshold.
func CountConsecutiveFailedRuns(db *sql.DB, entityID, promptID string) (int, error) {
	var count int
	err := db.QueryRow(`
		WITH recent AS (
			SELECT r.status, r.started_at
			FROM runs r
			JOIN tasks t ON r.task_id = t.id
			WHERE t.entity_id = ?
				AND r.prompt_id = ?
				AND r.trigger_type = 'event'
			ORDER BY r.started_at DESC
			LIMIT 20
		)
		SELECT COUNT(*)
		FROM recent
		WHERE status IN ('failed', 'task_unsolvable')
			AND started_at > (
				SELECT COALESCE(MAX(started_at), '1970-01-01')
				FROM recent WHERE status = 'completed'
			)
	`, entityID, promptID).Scan(&count)
	return count, err
}

// --- Internal query helpers -----------------------------------------------

func queryTasks(database *sql.DB, query string, args ...any) ([]domain.Task, error) {
	rows, err := database.Query(query, args...)
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

func scanTaskRow(row *sql.Row, t *domain.Task) error {
	return scanFields(row, t)
}

// taskScanState holds the NullX intermediates for one row of
// taskColumnsWithEntity. Declare it on the caller's stack (var s
// taskScanState), call s.targets(t) to get scan-destination pointers, then
// s.finalize(t) after Scan returns to copy the nullable values into t.
// Keeping state in a struct (rather than a closure) avoids a per-row heap
// allocation for the captured NullX variables and the closure itself.
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
		// Entity JOIN columns:
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

// scanFields works for both *sql.Row and *sql.Rows via the Scanner interface.
func scanFields(scanner interface{ Scan(...any) error }, t *domain.Task) error {
	var s taskScanState
	if err := scanner.Scan(s.targets(t)...); err != nil {
		return err
	}
	s.finalize(t)
	return nil
}

func scanTaskFields(rows *sql.Rows, t *domain.Task) error {
	return scanFields(rows, t)
}
