package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// CreateCuratorRequest inserts a queued request and returns its id.
// The HTTP handler returns 202 + this id immediately; the per-project
// goroutine picks the row up and flips status to running on the next
// tick. uuid generated server-side; callers do not supply ids.
func CreateCuratorRequest(database *sql.DB, projectID, userInput string) (string, error) {
	id := uuid.New().String()
	_, err := database.Exec(`
		INSERT INTO curator_requests (id, project_id, status, user_input, created_at)
		VALUES (?, ?, 'queued', ?, ?)
	`, id, projectID, userInput, time.Now().UTC())
	if err != nil {
		return "", err
	}
	return id, nil
}

// MarkCuratorRequestRunning flips a queued row to running and stamps
// started_at. Returns sql.ErrNoRows if the row is not currently queued
// (for example, it was already claimed or otherwise transitioned).
// This includes the cancel-vs-pickup race: a user fired DELETE while
// the row was queued, the goroutine sees a non-queued row when it
// dequeues, and skips it.
func MarkCuratorRequestRunning(database *sql.DB, id string) error {
	res, err := database.Exec(`
		UPDATE curator_requests
		SET status = 'running', started_at = ?
		WHERE id = ? AND status = 'queued'
	`, time.Now().UTC(), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// CompleteCuratorRequest writes a terminal status and accounting,
// but ONLY if the row is still non-terminal. Status is one of
// done | cancelled | failed. Caller passes 0 for any field that
// wasn't observed (e.g., a failure with no result event).
//
// Returns true if the flip happened. The status filter is the
// single source of truth for "terminal state is final": the
// goroutine that actually ran agentproc and the cancel handler
// can race to write the row, and either order produces the same
// outcome — the first writer wins, the second is a no-op. Without
// this filter, a user-cancel that landed during agentproc.Run
// could be silently overwritten by the goroutine's natural
// completion write seconds later.
//
// Per-project goroutine is the sole caller in normal flow; the
// guard exists for the rare cancel-vs-completion interleaving.
func CompleteCuratorRequest(database *sql.DB, id, status, errMsg string, costUSD float64, durationMs, numTurns int) (bool, error) {
	res, err := database.Exec(`
		UPDATE curator_requests
		SET status = ?, error_msg = ?, cost_usd = ?, duration_ms = ?, num_turns = ?, finished_at = ?
		WHERE id = ? AND status NOT IN ('done', 'cancelled', 'failed')
	`, status, nullIfEmpty(errMsg), costUSD, durationMs, numTurns, time.Now().UTC(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MarkCuratorRequestCancelledIfActive flips any non-terminal row to
// cancelled. Returns true if the flip happened. Used by the cancel
// endpoint and by the project-delete handler so an in-flight request
// for a deleted project lands in the right terminal state. The
// status-NOT-IN filter makes this safe to call from outside the
// per-project goroutine — terminal rows are left alone.
func MarkCuratorRequestCancelledIfActive(database *sql.DB, id, errMsg string) (bool, error) {
	res, err := database.Exec(`
		UPDATE curator_requests
		SET status = 'cancelled', error_msg = ?, finished_at = ?
		WHERE id = ? AND status NOT IN ('done', 'cancelled', 'failed')
	`, nullIfEmpty(errMsg), time.Now().UTC(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// GetCuratorRequest reads a single row, or returns (nil, nil) if not
// found. The handler returns 404 on the nil case.
func GetCuratorRequest(database *sql.DB, id string) (*domain.CuratorRequest, error) {
	row := database.QueryRow(`
		SELECT id, project_id, status, user_input, error_msg,
		       cost_usd, duration_ms, num_turns,
		       started_at, finished_at, created_at
		FROM curator_requests WHERE id = ?
	`, id)
	return scanCuratorRequest(row)
}

// ListCuratorRequestsByProject returns all requests for a project in
// chronological order. No pagination yet — chat history per project
// is bounded by usage and a single SELECT is fine for v1. Caller
// joins curator_messages separately for the agent-side stream.
func ListCuratorRequestsByProject(database *sql.DB, projectID string) ([]domain.CuratorRequest, error) {
	rows, err := database.Query(`
		SELECT id, project_id, status, user_input, error_msg,
		       cost_usd, duration_ms, num_turns,
		       started_at, finished_at, created_at
		FROM curator_requests
		WHERE project_id = ?
		ORDER BY created_at ASC, id ASC
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.CuratorRequest{}
	for rows.Next() {
		req, err := scanCuratorRequest(rows)
		if err != nil {
			return nil, err
		}
		if req != nil {
			out = append(out, *req)
		}
	}
	return out, rows.Err()
}

// InFlightCuratorRequestForProject returns the queued or running
// request for a project, or (nil, nil) if there is none. The cancel
// endpoint uses this to find the row to flip; the per-project
// invariant (one row at a time enters running) means at most one
// active row exists. If for some reason multiple non-terminal rows
// exist (queued + running during a goroutine schedule), the caller
// gets the oldest — running first if present, otherwise the head
// of the queue.
func InFlightCuratorRequestForProject(database *sql.DB, projectID string) (*domain.CuratorRequest, error) {
	row := database.QueryRow(`
		SELECT id, project_id, status, user_input, error_msg,
		       cost_usd, duration_ms, num_turns,
		       started_at, finished_at, created_at
		FROM curator_requests
		WHERE project_id = ? AND status IN ('queued', 'running')
		ORDER BY (status = 'running') DESC, created_at ASC, id ASC
		LIMIT 1
	`, projectID)
	return scanCuratorRequest(row)
}

// QueuedCuratorRequestsForProject returns queued rows in FIFO order.
// Used by Curator.CancelProject as a defensive sweep so a project-
// delete that races a never-picked-up queued row still flips that
// row to a terminal state before the project FK cascade fires.
// Cross-process recovery is out of scope: process-restart cancels
// every non-terminal row at startup (CancelOrphanedNonTerminalCuratorRequests),
// so by the time anything calls this helper, only rows enqueued
// during the current process lifetime can be observed.
func QueuedCuratorRequestsForProject(database *sql.DB, projectID string) ([]domain.CuratorRequest, error) {
	rows, err := database.Query(`
		SELECT id, project_id, status, user_input, error_msg,
		       cost_usd, duration_ms, num_turns,
		       started_at, finished_at, created_at
		FROM curator_requests
		WHERE project_id = ? AND status = 'queued'
		ORDER BY created_at ASC, id ASC
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.CuratorRequest{}
	for rows.Next() {
		req, err := scanCuratorRequest(rows)
		if err != nil {
			return nil, err
		}
		if req != nil {
			out = append(out, *req)
		}
	}
	return out, rows.Err()
}

// CancelOrphanedNonTerminalCuratorRequests is the startup recovery
// pass: any rows left non-terminal from a previous process are
// stranded — running rows lost their goroutine + agentproc
// subprocess at process exit (we can't resume mid-stream), and
// queued rows lost the goroutine that was supposed to pick them up.
//
// Auto-replaying a stale queued row would be more surprising than
// dropping it: the user's mental model after a restart is "the
// app started fresh," and a chat message that suddenly starts
// streaming a reply seconds later — possibly referencing a
// project state that's since changed — would feel like a bug.
// Cancelling both classes lets the user re-send if they actually
// wanted that message processed. Idempotent.
func CancelOrphanedNonTerminalCuratorRequests(database *sql.DB) (int64, error) {
	res, err := database.Exec(`
		UPDATE curator_requests
		SET status = 'cancelled',
		    error_msg = COALESCE(error_msg, 'process restarted'),
		    finished_at = COALESCE(finished_at, ?)
		WHERE status IN ('queued', 'running')
	`, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetProjectCuratorSessionID persists the captured Claude Code
// session_id on the project row. The first message kicks off a fresh
// CC session and captures the id; subsequent messages can resume
// against the persisted session id. This helper always performs the
// UPDATE by project id; callers may choose when to invoke it.
func SetProjectCuratorSessionID(database *sql.DB, projectID, sessionID string) error {
	_, err := database.Exec(`
		UPDATE projects SET curator_session_id = ?, updated_at = ?
		WHERE id = ?
	`, sessionID, time.Now().UTC(), projectID)
	return err
}

// InsertCuratorMessage writes one stream-output row and returns its id.
// Schema mirrors run_messages so the same agent-message accumulator
// (agentproc.StreamState) emits *domain.AgentMessage values that we
// translate to the curator_messages shape via the request_id.
func InsertCuratorMessage(database *sql.DB, msg *domain.CuratorMessage) (int64, error) {
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
		INSERT INTO curator_messages (request_id, role, content, subtype, tool_calls, tool_call_id, is_error, metadata, model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		msg.RequestID, msg.Role, msg.Content, msg.Subtype,
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

// curatorMessageColumns is the SELECT list shared by every helper
// that reads from curator_messages, so scanCuratorMessageRow stays
// tied to a single column ordering.
const curatorMessageColumns = `
	id, request_id, role, content, subtype, tool_calls, tool_call_id, is_error, metadata,
	model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, created_at
`

// scanCuratorMessageRow reads one curator_messages row from a Rows
// cursor — shared between the per-request and batched helpers so the
// nullable-column / JSON-decoding plumbing lives in one place.
func scanCuratorMessageRow(rows *sql.Rows) (domain.CuratorMessage, error) {
	var (
		m             domain.CuratorMessage
		toolCallsJSON sql.NullString
		metadataJSON  sql.NullString
		toolCallID    sql.NullString
		model         sql.NullString
		inputTokens   sql.NullInt64
		outputTokens  sql.NullInt64
		cacheRead     sql.NullInt64
		cacheCreation sql.NullInt64
	)
	if err := rows.Scan(
		&m.ID, &m.RequestID, &m.Role, &m.Content, &m.Subtype,
		&toolCallsJSON, &toolCallID, &m.IsError, &metadataJSON,
		&model, &inputTokens, &outputTokens, &cacheRead, &cacheCreation,
		&m.CreatedAt,
	); err != nil {
		return domain.CuratorMessage{}, err
	}
	if toolCallsJSON.Valid {
		if err := json.Unmarshal([]byte(toolCallsJSON.String), &m.ToolCalls); err != nil {
			return domain.CuratorMessage{}, fmt.Errorf("unmarshal tool_calls: %w", err)
		}
	}
	if metadataJSON.Valid {
		if err := json.Unmarshal([]byte(metadataJSON.String), &m.Metadata); err != nil {
			return domain.CuratorMessage{}, fmt.Errorf("unmarshal metadata: %w", err)
		}
	}
	m.ToolCallID = toolCallID.String
	m.Model = model.String
	if inputTokens.Valid {
		v := int(inputTokens.Int64)
		m.InputTokens = &v
	}
	if outputTokens.Valid {
		v := int(outputTokens.Int64)
		m.OutputTokens = &v
	}
	if cacheRead.Valid {
		v := int(cacheRead.Int64)
		m.CacheReadTokens = &v
	}
	if cacheCreation.Valid {
		v := int(cacheCreation.Int64)
		m.CacheCreationTokens = &v
	}
	return m, nil
}

// ListCuratorMessagesByRequest returns the agent-side stream rows for
// a request in chronological order. Used by the websocket replay path
// and tests; the GET history handler uses the batched
// ListCuratorMessagesByRequestIDs to avoid an N+1 over the request
// list.
func ListCuratorMessagesByRequest(database *sql.DB, requestID string) ([]domain.CuratorMessage, error) {
	rows, err := database.Query(`
		SELECT `+curatorMessageColumns+`
		FROM curator_messages
		WHERE request_id = ?
		ORDER BY created_at ASC, id ASC
	`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.CuratorMessage{}
	for rows.Next() {
		m, err := scanCuratorMessageRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListCuratorMessagesByRequestIDs returns the agent-side stream rows
// for a batch of request ids, grouped by request_id. The history
// handler calls this with every request id from
// ListCuratorRequestsByProject so the whole chat history loads in
// two queries instead of the N+1 the per-request helper would
// produce on a long-running project.
//
// Empty input returns an empty map (not nil) so callers can do a
// uniform map lookup without a nil-check.
//
// Chunking matches ListRecentEventsByEntity: SQLite's default
// SQLITE_LIMIT_VARIABLE_NUMBER is 999 on older builds, 32766 on
// modern ones, but staying at 500 keeps the IN-list comfortably
// inside both. Per-project chat counts are practically far below
// the chunk size; the loop is here for safety, not load.
func ListCuratorMessagesByRequestIDs(database *sql.DB, requestIDs []string) (map[string][]domain.CuratorMessage, error) {
	out := make(map[string][]domain.CuratorMessage)
	if len(requestIDs) == 0 {
		return out, nil
	}
	const chunkSize = 500
	for start := 0; start < len(requestIDs); start += chunkSize {
		end := start + chunkSize
		if end > len(requestIDs) {
			end = len(requestIDs)
		}
		chunk := requestIDs[start:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args[i] = id
		}

		query := `
			SELECT ` + curatorMessageColumns + `
			FROM curator_messages
			WHERE request_id IN (` + strings.Join(placeholders, ",") + `)
			ORDER BY created_at ASC, id ASC
		`
		rows, err := database.Query(query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			m, err := scanCuratorMessageRow(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[m.RequestID] = append(out[m.RequestID], m)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func scanCuratorRequest(row rowScanner) (*domain.CuratorRequest, error) {
	var (
		req        domain.CuratorRequest
		errMsg     sql.NullString
		startedAt  sql.NullTime
		finishedAt sql.NullTime
	)
	err := row.Scan(
		&req.ID, &req.ProjectID, &req.Status, &req.UserInput, &errMsg,
		&req.CostUSD, &req.DurationMs, &req.NumTurns,
		&startedAt, &finishedAt, &req.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	req.ErrorMsg = errMsg.String
	if startedAt.Valid {
		t := startedAt.Time
		req.StartedAt = &t
	}
	if finishedAt.Valid {
		t := finishedAt.Time
		req.FinishedAt = &t
	}
	return &req, nil
}
