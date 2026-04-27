package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
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
func CompleteAgentRun(database *sql.DB, runID, status string, costUSD float64, durationMs, numTurns int, stopReason, resultSummary string) error {
	now := time.Now()
	_, err := database.Exec(`
		UPDATE runs
		SET status = ?, completed_at = ?, total_cost_usd = ?, duration_ms = ?, num_turns = ?, stop_reason = ?, result_summary = ?
		WHERE id = ?
	`, status, now, costUSD, durationMs, numTurns, stopReason, resultSummary, runID)
	return err
}

// GetAgentRun returns a single agent run by ID.
func GetAgentRun(database *sql.DB, runID string) (*domain.AgentRun, error) {
	row := database.QueryRow(`
		SELECT id, task_id, status, model, started_at, completed_at,
		       total_cost_usd, duration_ms, num_turns, stop_reason, worktree_path,
		       result_summary, session_id, memory_missing
		FROM runs WHERE id = ?
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

// AgentRunsForTask returns all runs for a given task.
func AgentRunsForTask(database *sql.DB, taskID string) ([]domain.AgentRun, error) {
	rows, err := database.Query(`
		SELECT id, task_id, status, model, started_at, completed_at,
		       total_cost_usd, duration_ms, num_turns, stop_reason, worktree_path,
		       result_summary, session_id, memory_missing
		FROM runs WHERE task_id = ? ORDER BY started_at DESC
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

// MarkAgentRunMemoryMissing flags a run whose pre-complete memory-file gate
// exhausted all retries without the agent producing a memory file. The run
// still completes (we don't punish the agent for partial success by failing
// the run outright), but downstream UI and diagnostics surface the flag so
// the gap is visible. Called from the write-gate retry loop in SKY-141.
func MarkAgentRunMemoryMissing(database *sql.DB, runID string) error {
	_, err := database.Exec(`
		UPDATE runs SET memory_missing = 1 WHERE id = ?
	`, runID)
	return err
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

// ListTakenOverRunIDs returns the IDs of every run whose final state was
// taken_over. Read at startup so the worktree-cleanup sweep knows to leave
// those runs' ~/.claude/projects entries alone — the JSONL inside is what
// makes `claude --resume` work in the takeover destination.
func ListTakenOverRunIDs(database *sql.DB) ([]string, error) {
	rows, err := database.Query(`SELECT id FROM runs WHERE status = 'taken_over'`)
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

// InsertAgentMessage inserts a message and returns its ID.
func InsertAgentMessage(database *sql.DB, msg domain.AgentMessage) (int64, error) {
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

	result, err := database.Exec(`
		INSERT INTO run_messages (run_id, role, content, subtype, tool_calls, tool_call_id, is_error, metadata, model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		msg.RunID, msg.Role, msg.Content, msg.Subtype,
		toolCallsJSON, nullStr(msg.ToolCallID), msg.IsError, metadataJSON,
		nullStr(msg.Model), nullInt(msg.InputTokens), nullInt(msg.OutputTokens),
		nullInt(msg.CacheReadTokens), nullInt(msg.CacheCreationTokens),
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
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
