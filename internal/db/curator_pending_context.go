package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// InsertPendingContext queues a context-change delta for the next
// curator dispatch on (projectID, sessionID, changeType). Coalescing
// is enforced by the partial unique index on
// (project_id, curator_session_id, change_type) WHERE consumed_at IS
// NULL: a second PATCH between user messages hits ON CONFLICT DO
// NOTHING and the *earliest* baseline_value wins, which is the
// correct "snapshot before the first unconsumed change" anchor for
// diffing at consume time. baselineJSON must be a JSON-encoded
// representation of the value before this PATCH applied (an array
// for pinned_repos, a scalar string or JSON null for tracker keys).
//
// Caller is responsible for ensuring sessionID is non-empty — there
// is no point queueing pending rows for a project whose Curator has
// never been spun up, since the next session's static envelope will
// render fresh values directly.
func InsertPendingContext(database *sql.DB, projectID, sessionID, changeType, baselineJSON string) error {
	_, err := database.Exec(`
		INSERT INTO curator_pending_context
			(project_id, curator_session_id, change_type, baseline_value)
		VALUES (?, ?, ?, ?)
		ON CONFLICT DO NOTHING
	`, projectID, sessionID, changeType, baselineJSON)
	return err
}

// ConsumePendingContext atomically claims every unconsumed row for
// the given project+request and returns them alongside a fresh
// snapshot of the project — both reads happen inside the same
// transaction so the diff at the call site is computed against
// project state that is consistent with the rows being returned. A
// PATCH that lands between an earlier `db.GetProject` and this call
// (and queues a brand-new pending row baselined at the value the
// dispatch already saw) would otherwise be lost: consume claims it,
// the diff against the dispatch's stale envelope sees no change, and
// the row is finalized on `done` having delivered nothing. Reading
// the project inside the consume TX closes that window — the caller
// uses the returned *domain.Project for every downstream decision
// (envelope render, pinned-repo materialization, diff rendering) and
// the row's baseline lines up with the value the diff is computed
// against.
//
// Locking semantics: the first statement in the TX is the UPDATE,
// which causes SQLite to upgrade to a RESERVED lock immediately, so
// any concurrent PATCH that's also a writer waits for COMMIT. The
// follow-up SELECTs inside the TX therefore see a consistent picture.
// The pool is sized to MaxOpenConns=1 in production so concurrent TXs
// are serialized at the Go layer too, but the TX-local consistency
// matters for correctness regardless of pool size.
//
// Session id is read from the project row inside the TX rather than
// taken as a parameter so the consume scopes to whatever session is
// currently set on the project, even if the caller has a stale view
// of it. An empty session id (project never chatted with) yields a
// no-op consume — the next dispatch's static envelope renders fresh
// values directly. Returns (nil project, nil rows, nil error) when
// the project no longer exists; the caller decides whether to fail
// the request.
func ConsumePendingContext(database *sql.DB, projectID, requestID string) (*domain.Project, []domain.CuratorPendingContext, error) {
	tx, err := database.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	project, err := scanProject(tx.QueryRow(`
		SELECT id, name, description, summary_md, summary_stale, curator_session_id, pinned_repos, jira_project_key, linear_project_key, created_at, updated_at
		FROM projects WHERE id = ?
	`, projectID))
	if err != nil {
		return nil, nil, fmt.Errorf("read project: %w", err)
	}
	if project == nil {
		// Project disappeared between the dispatch's earlier checks and
		// here. The caller will surface this as a request failure; we
		// return cleanly so the deferred Rollback fires without noise.
		return nil, []domain.CuratorPendingContext{}, nil
	}
	if project.CuratorSessionID == "" {
		// No session yet — there cannot be any pending rows, since the
		// PATCH handler short-circuits on empty session id. Skip the
		// UPDATE/SELECT entirely and return the fresh project so the
		// caller can still use it for envelope rendering.
		if err := tx.Commit(); err != nil {
			return nil, nil, err
		}
		return project, []domain.CuratorPendingContext{}, nil
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(`
		UPDATE curator_pending_context
		   SET consumed_at = ?, consumed_by_request_id = ?
		 WHERE project_id = ?
		   AND curator_session_id = ?
		   AND consumed_at IS NULL
	`, now, requestID, projectID, project.CuratorSessionID); err != nil {
		return nil, nil, fmt.Errorf("claim pending rows: %w", err)
	}

	rows, err := tx.Query(`
		SELECT id, project_id, curator_session_id, change_type, baseline_value,
		       consumed_at, consumed_by_request_id, created_at
		  FROM curator_pending_context
		 WHERE consumed_by_request_id = ?
		 ORDER BY created_at ASC, id ASC
	`, requestID)
	if err != nil {
		return nil, nil, fmt.Errorf("read claimed rows: %w", err)
	}
	defer rows.Close()

	out := []domain.CuratorPendingContext{}
	for rows.Next() {
		row, err := scanPendingContext(rows)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return project, out, nil
}

// FinalizePendingContext deletes every row consumed by requestID. Called
// from the per-project goroutine after agentproc returns a `done`
// terminal — the agent has seen the deltas, so they can be retired.
// Idempotent: a request that consumed zero rows produces a zero-row
// delete, which is fine.
func FinalizePendingContext(database *sql.DB, requestID string) error {
	_, err := database.Exec(`
		DELETE FROM curator_pending_context
		 WHERE consumed_by_request_id = ?
	`, requestID)
	return err
}

// RevertPendingContext un-consumes the rows claimed by requestID so
// the next user message picks them up again. Used on terminal
// `cancelled` or `failed` so a transient agentproc failure (model auth
// error, network blip, user cancel) doesn't silently lose the user's
// deltas.
//
// Merge: a NEW PATCH may have landed during dispatch (the partial
// unique index excludes consumed rows, so a fresh pending row could be
// inserted alongside the consumed one). Two unconsumed rows for the
// same (session, change_type) would violate the unique constraint on
// revert. The older (consumed-but-being-reverted) row's baseline is
// the truer "earliest unconsumed snapshot" — it covers the entire
// window from the original PATCH through the new one — so we drop
// the newer row in its favor.
func RevertPendingContext(database *sql.DB, requestID string) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		DELETE FROM curator_pending_context
		 WHERE consumed_at IS NULL
		   AND (project_id, curator_session_id, change_type) IN (
		       SELECT project_id, curator_session_id, change_type
		         FROM curator_pending_context
		        WHERE consumed_by_request_id = ?
		   )
	`, requestID); err != nil {
		return fmt.Errorf("merge pending rows: %w", err)
	}

	if _, err := tx.Exec(`
		UPDATE curator_pending_context
		   SET consumed_at = NULL, consumed_by_request_id = NULL
		 WHERE consumed_by_request_id = ?
	`, requestID); err != nil {
		return fmt.Errorf("revert pending rows: %w", err)
	}

	return tx.Commit()
}

// DeletePendingContextForSession removes every pending or consumed row
// for a given (projectID, sessionID) — used when the session is reset
// (orphan-cleanup, future user-driven "fresh chat" action). The new
// session's static envelope renders current values directly, so any
// pending diff against the dead session would just be noise.
//
// Project deletion is handled by the FK CASCADE; this helper is for
// the session-only reset case where the project row stays but its
// curator_session_id flips.
func DeletePendingContextForSession(database *sql.DB, projectID, sessionID string) error {
	_, err := database.Exec(`
		DELETE FROM curator_pending_context
		 WHERE project_id = ? AND curator_session_id = ?
	`, projectID, sessionID)
	return err
}

// ListPendingContext returns every row for a project regardless of
// session or consumption state. Test-only / debugging surface — the
// curator runtime never needs this. Lives in db so tests can assert
// on the raw table shape without poking sql directly.
func ListPendingContext(database *sql.DB, projectID string) ([]domain.CuratorPendingContext, error) {
	rows, err := database.Query(`
		SELECT id, project_id, curator_session_id, change_type, baseline_value,
		       consumed_at, consumed_by_request_id, created_at
		  FROM curator_pending_context
		 WHERE project_id = ?
		 ORDER BY created_at ASC, id ASC
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.CuratorPendingContext{}
	for rows.Next() {
		row, err := scanPendingContext(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// scanPendingContext is rows-only — it doesn't paper over sql.ErrNoRows
// because the only callers iterate via rows.Next() (which surfaces
// "no row" as a false return, never as ErrNoRows). Hiding ErrNoRows
// here would silently swallow the error if a future caller
// mistakenly used QueryRow.Scan; surfacing it lets the misuse
// produce a real, visible error instead.
func scanPendingContext(scanner interface {
	Scan(dest ...any) error
}) (domain.CuratorPendingContext, error) {
	var (
		row        domain.CuratorPendingContext
		consumedAt sql.NullTime
		consumedBy sql.NullString
	)
	if err := scanner.Scan(
		&row.ID, &row.ProjectID, &row.CuratorSessionID, &row.ChangeType,
		&row.BaselineValue, &consumedAt, &consumedBy, &row.CreatedAt,
	); err != nil {
		return domain.CuratorPendingContext{}, err
	}
	if consumedAt.Valid {
		t := consumedAt.Time
		row.ConsumedAt = &t
	}
	row.ConsumedByRequestID = consumedBy.String
	return row, nil
}
