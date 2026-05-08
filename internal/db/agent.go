package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// CreateAgentRun inserts a new agent run.
func CreateAgentRun(database *sql.DB, run domain.AgentRun) error {
	triggerType := run.TriggerType
	if triggerType == "" {
		triggerType = "manual"
	}
	_, err := database.Exec(`
		INSERT INTO runs (id, task_id, prompt_id, status, model, worktree_path, trigger_type, trigger_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, run.ID, run.TaskID, nullIfEmpty(run.PromptID), run.Status, run.Model, run.WorktreePath, triggerType, nullIfEmpty(run.TriggerID))
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// CompleteAgentRun updates a run with completion info.
//
// total_cost_usd / duration_ms / num_turns are *added* to existing
// values rather than overwritten. For runs that never yielded the
// columns are NULL coming in, so COALESCE(NULL, 0) + x = x — same
// result as the previous assignment behavior. For yield-and-resume
// runs (SKY-139) the partial totals from each yielded invocation are
// already on the row via AddAgentRunPartialTotals; this final call
// folds in the terminal invocation's totals to produce the correct
// cumulative spend.
func CompleteAgentRun(database *sql.DB, runID, status string, costUSD float64, durationMs, numTurns int, stopReason, resultSummary string) error {
	now := time.Now()
	_, err := database.Exec(`
		UPDATE runs
		SET status = ?,
		    completed_at = ?,
		    total_cost_usd = COALESCE(total_cost_usd, 0) + ?,
		    duration_ms = COALESCE(duration_ms, 0) + ?,
		    num_turns = COALESCE(num_turns, 0) + ?,
		    stop_reason = ?,
		    result_summary = ?
		WHERE id = ?
	`, status, now, costUSD, durationMs, numTurns, stopReason, resultSummary, runID)
	return err
}

// AddAgentRunPartialTotals adds an invocation's cost/duration/turns to
// the run's running totals without flipping status or completed_at.
// Called when a run yields mid-execution so accumulated spend is
// visible to the UI while the agent is parked in awaiting_input, and
// so the eventual CompleteAgentRun produces a correct cumulative total
// when it adds the terminal invocation's deltas on top. SKY-139.
func AddAgentRunPartialTotals(database *sql.DB, runID string, costUSD float64, durationMs, numTurns int) error {
	_, err := database.Exec(`
		UPDATE runs
		SET total_cost_usd = COALESCE(total_cost_usd, 0) + ?,
		    duration_ms = COALESCE(duration_ms, 0) + ?,
		    num_turns = COALESCE(num_turns, 0) + ?
		WHERE id = ?
	`, costUSD, durationMs, numTurns, runID)
	return err
}

// MarkAgentRunAwaitingInput flips a running run to awaiting_input
// without writing a terminal completed_at — the agent will be resumed
// once the user responds. Guarded against concurrent terminal flips
// (cancellation, takeover) by the status-NOT-IN filter; returns
// ok=false (no error) if the row already reached a terminal state.
//
// SKY-139.
func MarkAgentRunAwaitingInput(database *sql.DB, runID string) (bool, error) {
	res, err := database.Exec(`
		UPDATE runs
		SET status = 'awaiting_input'
		WHERE id = ?
		  AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable', 'pending_approval', 'taken_over', 'awaiting_input')
	`, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkAgentRunResuming flips an awaiting_input run back to running
// when the user responds and a resume goroutine is about to spawn.
// Returns ok=false (no error) if the row isn't in awaiting_input —
// either the run was cancelled while the user was deciding, or two
// respond submissions raced and the second lost. The caller must
// treat ok=false as "don't spawn the resume" to avoid double-resume.
//
// SKY-139.
func MarkAgentRunResuming(database *sql.DB, runID string) (bool, error) {
	res, err := database.Exec(`
		UPDATE runs SET status = 'running'
		WHERE id = ? AND status = 'awaiting_input'
	`, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// GetAgentRun returns a single agent run by ID. MemoryMissing is
// derived from run_memory rather than read off a column on runs —
// the row is absent (or has agent_content NULL) when the agent didn't
// pass through the memory gate. See SKY-204 for the move from a
// denormalized boolean to a JOIN-derived projection.
func GetAgentRun(database *sql.DB, runID string) (*domain.AgentRun, error) {
	row := database.QueryRow(`
		SELECT r.id, r.task_id, r.status, r.model, r.started_at, r.completed_at,
		       r.total_cost_usd, r.duration_ms, r.num_turns, r.stop_reason, r.worktree_path,
		       r.result_summary, r.session_id,
		       (NULLIF(TRIM(rm.agent_content, ' ' || char(9) || char(10) || char(13)), '') IS NULL) AS memory_missing
		FROM runs r
		LEFT JOIN run_memory rm ON rm.run_id = r.id
		WHERE r.id = ?
	`, runID)

	var r domain.AgentRun
	var completedAt sql.NullTime
	var costUSD sql.NullFloat64
	var durationMs, numTurns sql.NullInt64
	var stopReason, worktreePath, model, resultSummary, sessionID sql.NullString

	err := row.Scan(&r.ID, &r.TaskID, &r.Status, &model, &r.StartedAt, &completedAt,
		&costUSD, &durationMs, &numTurns, &stopReason, &worktreePath,
		&resultSummary, &sessionID, &r.MemoryMissing)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	r.Model = model.String
	r.StopReason = stopReason.String
	r.WorktreePath = worktreePath.String
	r.ResultSummary = resultSummary.String
	r.SessionID = sessionID.String
	if completedAt.Valid {
		r.CompletedAt = &completedAt.Time
	}
	if costUSD.Valid {
		r.TotalCostUSD = &costUSD.Float64
	}
	if durationMs.Valid {
		v := int(durationMs.Int64)
		r.DurationMs = &v
	}
	if numTurns.Valid {
		v := int(numTurns.Int64)
		r.NumTurns = &v
	}

	return &r, nil
}

// AgentRunsForTask returns all runs for a given task. See GetAgentRun
// for the MemoryMissing derivation. NULLIF(TRIM(...), ”) guards
// against any row whose agent_content was written as the empty string
// (legacy data carried over from before SKY-204, or a future writer
// that bypasses UpsertAgentMemory) — both NULL and "" mean "agent
// didn't comply with the gate."
func AgentRunsForTask(database *sql.DB, taskID string) ([]domain.AgentRun, error) {
	rows, err := database.Query(`
		SELECT r.id, r.task_id, r.status, r.model, r.started_at, r.completed_at,
		       r.total_cost_usd, r.duration_ms, r.num_turns, r.stop_reason, r.worktree_path,
		       r.result_summary, r.session_id,
		       (NULLIF(TRIM(rm.agent_content, ' ' || char(9) || char(10) || char(13)), '') IS NULL) AS memory_missing
		FROM runs r
		LEFT JOIN run_memory rm ON rm.run_id = r.id
		WHERE r.task_id = ?
		ORDER BY r.started_at DESC
	`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []domain.AgentRun
	for rows.Next() {
		var r domain.AgentRun
		var completedAt sql.NullTime
		var costUSD sql.NullFloat64
		var durationMs, numTurns sql.NullInt64
		var stopReason, worktreePath, model, resultSummary, sessionID sql.NullString

		if err := rows.Scan(&r.ID, &r.TaskID, &r.Status, &model, &r.StartedAt, &completedAt,
			&costUSD, &durationMs, &numTurns, &stopReason, &worktreePath,
			&resultSummary, &sessionID, &r.MemoryMissing); err != nil {
			return nil, err
		}

		r.Model = model.String
		r.StopReason = stopReason.String
		r.WorktreePath = worktreePath.String
		r.ResultSummary = resultSummary.String
		r.SessionID = sessionID.String
		if completedAt.Valid {
			r.CompletedAt = &completedAt.Time
		}
		if costUSD.Valid {
			r.TotalCostUSD = &costUSD.Float64
		}
		if durationMs.Valid {
			v := int(durationMs.Int64)
			r.DurationMs = &v
		}
		if numTurns.Valid {
			v := int(numTurns.Int64)
			r.NumTurns = &v
		}

		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// SetAgentRunSession stores the Claude Code session_id captured from
// `claude -p --output-format json` output. Called as soon as the spawner
// parses the init event from the stream so subsequent resume calls have a
// session to attach to. Separate from CompleteAgentRun because the session
// id needs to be persisted mid-run, before any terminal state is reached —
// the write-gate retry loop in SKY-141 depends on being able to resume a
// run whose initial invocation returned but failed the memory-file check.
func SetAgentRunSession(database *sql.DB, runID, sessionID string) error {
	_, err := database.Exec(`
		UPDATE runs SET session_id = ? WHERE id = ?
	`, sessionID, runID)
	return err
}

// MarkAgentRunTakenOver finalizes a run as terminal in the "user pulled it
// out for interactive resume" sense. Distinct from cancelled because the
// session lives on under the user's control.
//
// Updates worktree_path to point at the takeover destination — the
// original /tmp worktree is removed by Spawner.Takeover, so leaving the
// column pointing at a now-deleted path would be actively misleading.
// Subsequent GetAgentRun calls return the live takeover dir as the
// structured location of the run's working tree. result_summary
// duplicates this in human-readable form for log/audit display.
//
// Cost/duration accounting is left blank: the headless invocation
// didn't finish a turn and any meaningful spend belongs to whatever
// the user does next, which we no longer track.
//
// Guarded against late-completion races: the UPDATE only fires when
// the row is still in a non-terminal status. Returns ok=false (no
// error) when the row already reached a terminal state — the caller
// treats that as "the run finished on its own; takeover came too
// late."
func MarkAgentRunTakenOver(database *sql.DB, runID, takeoverPath string) (bool, error) {
	now := time.Now()
	res, err := database.Exec(`
		UPDATE runs
		SET status = 'taken_over',
		    completed_at = ?,
		    stop_reason = 'user_takeover',
		    result_summary = ?,
		    worktree_path = ?
		WHERE id = ?
		  AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable', 'pending_approval', 'taken_over')
	`, now, "Taken over by user → "+takeoverPath, takeoverPath, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkAgentRunReleased flips a held takeover into the "released" sub-state:
// status stays 'taken_over' (the audit trail keeps "the user took this over"
// readable), but worktree_path is cleared and result_summary appended so the
// resume picker / Held Takeovers banner / startup cleanup-preserve sweep all
// drop the row from their working sets.
//
// Guarded by status='taken_over' AND non-empty worktree_path so a double-click
// or a release on a never-taken-over row returns ok=false (no error). Caller
// uses ok=false to skip the websocket broadcast and surface a 409 to the UI.
func MarkAgentRunReleased(database *sql.DB, runID string) (bool, error) {
	res, err := database.Exec(`
		UPDATE runs
		SET worktree_path = '',
		    result_summary = CASE
		        WHEN COALESCE(result_summary, '') = '' THEN 'released by user'
		        ELSE result_summary || '; released by user'
		    END
		WHERE id = ?
		  AND status = 'taken_over'
		  AND COALESCE(worktree_path, '') != ''
	`, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkAgentRunCancelledIfActive marks a run cancelled with the given
// stop_reason / summary, but only if the row hasn't already reached a
// terminal state. Returns ok=false (no error) when the row is already
// terminal — used by takeover-rollback so the rollback can recover from
// either "we cancelled the goroutine and need to write the terminal
// status ourselves" or "the goroutine completed naturally before our
// takeover landed; leave its real outcome alone." Either way, the row
// ends up in a sensible terminal state and isn't left stuck on
// 'running'.
func MarkAgentRunCancelledIfActive(database *sql.DB, runID, stopReason, summary string) (bool, error) {
	now := time.Now()
	res, err := database.Exec(`
		UPDATE runs
		SET status = 'cancelled', completed_at = ?, stop_reason = ?, result_summary = ?
		WHERE id = ?
		  AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable', 'pending_approval', 'taken_over')
	`, now, stopReason, summary, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkAgentRunDiscarded marks a pending_approval run as cancelled
// when the user requeues / dismisses the task without submitting the
// review the agent prepared. Mirrors MarkAgentRunCancelledIfActive
// but specifically targets pending_approval — that helper deliberately
// excludes pending_approval from its terminal-status filter so the
// "process exited cleanly, awaiting user input" state isn't trampled
// by a late takeover-rollback. Here we DO want to flip it: the user
// has explicitly chosen to discard.
//
// The agent process has already exited by the time pending_approval
// is reached (the spawner's runAgent defer ran), so there's nothing
// to cancel at the process level — this is purely a DB cleanup.
//
// Idempotent against concurrent calls via the status='pending_approval'
// guard: a second call against an already-cancelled row affects 0
// rows. Returns ok=false in that case so the caller can skip the
// websocket broadcast (no actual state change to push).
func MarkAgentRunDiscarded(database *sql.DB, runID, stopReason string) (bool, error) {
	now := time.Now()
	res, err := database.Exec(`
		UPDATE runs
		SET status = 'cancelled',
		    completed_at = COALESCE(completed_at, ?),
		    stop_reason = ?,
		    result_summary = COALESCE(NULLIF(result_summary, ''), ?)
		WHERE id = ? AND status = 'pending_approval'
	`, now, stopReason, "Review discarded by user.", runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// PendingApprovalRunIDForTask returns the id of the (single)
// pending_approval run on a task, or "" if none. Used by the
// requeue-finalizer to decide whether the discard cleanup needs to
// run. Bounded to one row by construction — the spawner only flips
// to pending_approval after CompleteAgentRun, and a task only has
// one in-flight delegation at a time.
func PendingApprovalRunIDForTask(database *sql.DB, taskID string) (string, error) {
	var id string
	err := database.QueryRow(
		`SELECT id FROM runs WHERE task_id = ? AND status = 'pending_approval' LIMIT 1`,
		taskID,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

// HasActiveRunForTask returns true if the task has any agent run that hasn't
// reached a terminal state. Used as an in-flight gate for auto-delegation.
func HasActiveRunForTask(database *sql.DB, taskID string) (bool, error) {
	var count int
	err := database.QueryRow(`
		SELECT COUNT(*) FROM runs
		WHERE task_id = ? AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable', 'pending_approval', 'taken_over')
	`, taskID).Scan(&count)
	return count > 0, err
}

// ActiveRunIDsForTask returns the IDs of runs on the task that haven't
// reached a terminal state. Used by the task-close → run-cancel cascade
// so any in-flight agent stops work the moment the system decides the
// task is resolved (auto close, entity close, user dismiss).
//
// The terminal-state list matches HasActiveRunForTask — same answer to
// "is this run still running," different shape. pending_approval counts
// as terminal here: the process has exited and the user is deliberating,
// cancelling it would discard work that's ready to apply.
func ActiveRunIDsForTask(database *sql.DB, taskID string) ([]string, error) {
	rows, err := database.Query(`
		SELECT id FROM runs
		WHERE task_id = ? AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable', 'pending_approval', 'taken_over')
	`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListTakenOverRunIDs returns the IDs of every run currently held in the
// taken_over state with a live takeover dir. Read at startup so the
// worktree-cleanup sweep knows to leave those runs' ~/.claude/projects
// entries alone — the JSONL inside is what makes `claude --resume` work in
// the takeover destination. Released takeovers (status='taken_over' with
// empty worktree_path) are excluded: their dirs are gone, so the projects
// entry has nothing left to preserve.
func ListTakenOverRunIDs(database *sql.DB) ([]string, error) {
	rows, err := database.Query(`SELECT id FROM runs WHERE status = 'taken_over' AND COALESCE(worktree_path, '') != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// TakenOverRun is a slim view of a taken-over run for the
// `triagefactory resume` CLI's picker. Carries just enough to render
// the user's choices and exec into the right session — the path,
// session id, and a task title for context. SourceID lets us show
// "PR #42" or "SKY-194" so the user can tell takeovers apart.
type TakenOverRun struct {
	RunID        string
	SessionID    string
	WorktreePath string
	TaskTitle    string
	SourceID     string
	CompletedAt  time.Time
}

// ListTakenOverRunsForResume returns every taken-over run in the local
// DB, joined with its task + entity for display, ordered newest-first.
// Used by the CLI's resume command — the bare-call default picks
// runs[0], the picker shows the whole list. Filters out rows missing
// the session_id or worktree_path (shouldn't happen — Spawner.Takeover
// requires both — but defensive against historical data).
//
// Title and source_id live on entities, not tasks: the join chain is
// runs.task_id → tasks.entity_id → entities. LEFT JOIN throughout so
// a deleted task or entity doesn't drop the run from the list — the
// user can still resume even if the upstream task got cleaned up.
func ListTakenOverRunsForResume(database *sql.DB) ([]TakenOverRun, error) {
	// completed_at and started_at returned as raw columns rather than
	// COALESCE'd into one — the SQLite driver can scan a column of
	// declared DATETIME type into sql.NullTime, but a COALESCE
	// expression strips the type metadata and the result comes back
	// as an unparseable string. Sort uses COALESCE because string
	// sort over ISO-8601 happens to be correct ordering.
	rows, err := database.Query(`
		SELECT r.id, COALESCE(r.session_id, ''), COALESCE(r.worktree_path, ''),
		       COALESCE(e.title, ''), COALESCE(e.source_id, ''),
		       r.completed_at, r.started_at
		FROM runs r
		LEFT JOIN tasks t ON t.id = r.task_id
		LEFT JOIN entities e ON e.id = t.entity_id
		WHERE r.status = 'taken_over'
		  AND COALESCE(r.worktree_path, '') != ''
		ORDER BY COALESCE(r.completed_at, r.started_at) DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TakenOverRun
	for rows.Next() {
		var r TakenOverRun
		var completedAt, startedAt sql.NullTime
		if err := rows.Scan(&r.RunID, &r.SessionID, &r.WorktreePath, &r.TaskTitle, &r.SourceID, &completedAt, &startedAt); err != nil {
			return nil, err
		}
		if r.SessionID == "" || r.WorktreePath == "" {
			continue
		}
		switch {
		case completedAt.Valid:
			r.CompletedAt = completedAt.Time
		case startedAt.Valid:
			r.CompletedAt = startedAt.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InsertAgentMessage inserts a message and returns its ID. If msg.CreatedAt
// is zero, it is stamped with time.Now().UTC() and written back to the caller
// so a subsequent WS broadcast can carry the same value as the DB row without
// a re-read.
func InsertAgentMessage(database *sql.DB, msg *domain.AgentMessage) (int64, error) {
	var toolCallsJSON, metadataJSON sql.NullString

	if len(msg.ToolCalls) > 0 {
		b, err := json.Marshal(msg.ToolCalls)
		if err != nil {
			return 0, fmt.Errorf("marshal tool_calls: %w", err)
		}
		toolCallsJSON = sql.NullString{String: string(b), Valid: true}
	}
	if len(msg.Metadata) > 0 {
		b, err := json.Marshal(msg.Metadata)
		if err != nil {
			return 0, fmt.Errorf("marshal metadata: %w", err)
		}
		metadataJSON = sql.NullString{String: string(b), Valid: true}
	}

	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}

	result, err := database.Exec(`
		INSERT INTO run_messages (run_id, role, content, subtype, tool_calls, tool_call_id, is_error, metadata, model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		msg.RunID, msg.Role, msg.Content, msg.Subtype,
		toolCallsJSON, nullStr(msg.ToolCallID), msg.IsError, metadataJSON,
		nullStr(msg.Model), nullInt(msg.InputTokens), nullInt(msg.OutputTokens),
		nullInt(msg.CacheReadTokens), nullInt(msg.CacheCreationTokens),
		msg.CreatedAt,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// EntitiesWithAwaitingInputRuns returns the subset of entityIDs that
// have at least one run currently in awaiting_input. Used by the
// factory snapshot to paint a "waiting for response" badge on the
// chip without the frontend having to walk every run. Bounded to
// snapshot's entity set (≤ factoryEntityLimit), so the IN-list stays
// well under SQLite's variable cap and a single round trip suffices.
// SKY-139.
func EntitiesWithAwaitingInputRuns(database *sql.DB, entityIDs []string) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	if len(entityIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(entityIDs))
	args := make([]any, 0, len(entityIDs))
	for i, id := range entityIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	query := `
		SELECT DISTINCT t.entity_id
		FROM runs r
		JOIN tasks t ON t.id = r.task_id
		WHERE r.status = 'awaiting_input'
		  AND t.entity_id IN (` + strings.Join(placeholders, ",") + `)
	`
	rows, err := database.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// run_messages subtypes used by the SKY-139 yield-resume flow. Stored
// in the existing transcript stream rather than dedicated tables so
// the UI can render Q+A pairs inline with the rest of the run's
// conversation, and so the run_messages-driven token/cost analytics
// don't need to know yield exists.
const (
	YieldRequestSubtype  = "yield_request"
	YieldResponseSubtype = "yield_response"
)

// InsertYieldRequest records the agent's yield request as an
// assistant-role message with subtype yield_request. content is the
// JSON-marshalled YieldRequest payload — the frontend parses it to
// pick a renderer and the respond endpoint reads it back to validate
// that a submitted response matches the open request's type.
//
// Returns the inserted message (ID populated, CreatedAt stamped) so
// the caller can broadcast it without a re-read.
func InsertYieldRequest(database *sql.DB, runID string, req *domain.YieldRequest) (*domain.AgentMessage, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal yield request: %w", err)
	}
	msg := &domain.AgentMessage{
		RunID:   runID,
		Role:    "assistant",
		Subtype: YieldRequestSubtype,
		Content: string(payload),
	}
	id, err := InsertAgentMessage(database, msg)
	if err != nil {
		return nil, err
	}
	msg.ID = int(id)
	return msg, nil
}

// InsertYieldResponse records the user's response to an open yield as
// a user-role message with subtype yield_response. content is the
// human-readable display rendering (e.g. "Approved", "Rebase onto
// main", or the raw prompt text); metadata carries the structured
// YieldResponse JSON so the backend can replay the answer later if
// needed.
//
// The agent-facing plain-text rendering is computed at resume time
// and not persisted on this row — it's a function of (request,
// response) and reproducible from those two stored shapes.
func InsertYieldResponse(database *sql.DB, runID string, resp *domain.YieldResponse, displayContent string) (*domain.AgentMessage, error) {
	payload, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("marshal yield response: %w", err)
	}
	msg := &domain.AgentMessage{
		RunID:   runID,
		Role:    "user",
		Subtype: YieldResponseSubtype,
		Content: displayContent,
		Metadata: map[string]any{
			"yield_response": json.RawMessage(payload),
		},
	}
	id, err := InsertAgentMessage(database, msg)
	if err != nil {
		return nil, err
	}
	msg.ID = int(id)
	return msg, nil
}

// LatestYieldRequest returns the most recent yield_request for a run,
// or (nil, nil) if none exists. Used by the respond endpoint to read
// back the open question so it can validate the submitted response's
// shape against the request's type.
//
// "Most recent" is correct for "currently open" because once a yield
// is answered, the run flips back to running and the agent has to
// emit a fresh yield envelope to park again — each new park is a
// new yield_request row that supersedes prior ones for the
// "current open yield" purpose.
func LatestYieldRequest(database *sql.DB, runID string) (*domain.YieldRequest, error) {
	row := database.QueryRow(`
		SELECT content FROM run_messages
		WHERE run_id = ? AND subtype = ?
		ORDER BY id DESC LIMIT 1
	`, runID, YieldRequestSubtype)
	var content sql.NullString
	if err := row.Scan(&content); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if !content.Valid || content.String == "" {
		return nil, nil
	}
	var req domain.YieldRequest
	if err := json.Unmarshal([]byte(content.String), &req); err != nil {
		return nil, fmt.Errorf("unmarshal yield request: %w", err)
	}
	return &req, nil
}

// MessagesForRun returns all messages for a given agent run, ordered by ID.
func MessagesForRun(database *sql.DB, runID string) ([]domain.AgentMessage, error) {
	rows, err := database.Query(`
		SELECT id, run_id, role, content, subtype, tool_calls, tool_call_id, is_error, metadata,
		       model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, created_at
		FROM run_messages WHERE run_id = ? ORDER BY id ASC
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []domain.AgentMessage
	for rows.Next() {
		var m domain.AgentMessage
		var content, subtype, toolCallsStr, toolCallID, metadataStr, model sql.NullString
		var inputTok, outputTok, cacheReadTok, cacheCreateTok sql.NullInt64

		if err := rows.Scan(
			&m.ID, &m.RunID, &m.Role, &content, &subtype, &toolCallsStr,
			&toolCallID, &m.IsError, &metadataStr, &model,
			&inputTok, &outputTok, &cacheReadTok, &cacheCreateTok, &m.CreatedAt,
		); err != nil {
			return nil, err
		}

		m.Content = content.String
		m.Subtype = subtype.String
		m.ToolCallID = toolCallID.String
		m.Model = model.String

		if toolCallsStr.Valid {
			_ = json.Unmarshal([]byte(toolCallsStr.String), &m.ToolCalls)
		}
		if metadataStr.Valid {
			_ = json.Unmarshal([]byte(metadataStr.String), &m.Metadata)
		}
		if inputTok.Valid {
			v := int(inputTok.Int64)
			m.InputTokens = &v
		}
		if outputTok.Valid {
			v := int(outputTok.Int64)
			m.OutputTokens = &v
		}
		if cacheReadTok.Valid {
			v := int(cacheReadTok.Int64)
			m.CacheReadTokens = &v
		}
		if cacheCreateTok.Valid {
			v := int(cacheCreateTok.Int64)
			m.CacheCreationTokens = &v
		}

		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// TokenTotals holds summed token counts for a run.
type TokenTotals struct {
	Model               string
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
	NumTurns            int
}

// RunTokenTotals sums token usage across all assistant messages in a run.
func RunTokenTotals(database *sql.DB, runID string) (*TokenTotals, error) {
	row := database.QueryRow(`
		SELECT COALESCE(MAX(model), ''),
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0),
		       COALESCE(SUM(cache_creation_tokens), 0),
		       COUNT(*)
		FROM run_messages
		WHERE run_id = ? AND role = 'assistant'
	`, runID)

	var t TokenTotals
	if err := row.Scan(&t.Model, &t.InputTokens, &t.OutputTokens, &t.CacheReadTokens, &t.CacheCreationTokens, &t.NumTurns); err != nil {
		return nil, err
	}
	return &t, nil
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func nullInt(p *int) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*p), Valid: true}
}
