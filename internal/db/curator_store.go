package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// CuratorStore owns the three curator-runtime tables: curator_requests,
// curator_messages, curator_pending_context. Each write attributes to
// the requesting user via creator_user_id — the multi-tenant RLS
// policies (curator_requests_modify, curator_messages_modify,
// curator_pending_context_modify) gate every row on the (org_id,
// creator_user_id) pair matching tf.current_user_id() /
// tf.current_org_id(), so every method here must run inside a
// SyntheticClaimsWithTx (or admin pool) in Postgres.
//
// Wires the app pool in Postgres for the curator goroutine's normal
// dispatch path — each turn opens short-lived txs under the
// requesting user's identity (read from curator_requests.creator_user_id
// at dequeue time). System-service paths that lack a real user
// (process shutdown, project-delete fan-out, handler cancels prior to
// D9) still call the package-level helpers in internal/db/curator.go
// against *sql.DB; those are out of scope for this ticket and tracked
// by SKY-253.
//
// Read methods (Get/List) live in the package-level helpers for now —
// the goroutine's writes are the auth surface that matters under RLS.
// One read does live here, GetRequest, because the dispatch goroutine
// reads it inside the same per-turn synthetic-claims wrap as the
// MarkRunning write and we want the read to honor RLS in Postgres.
type CuratorStore interface {
	// CreateRequest inserts a new queued curator_request row and
	// returns its id. creatorUserID is the requesting user — in
	// local mode the handler passes runmode.LocalDefaultUserID
	// (D9 retrofit will plumb the real user from request context).
	// In Postgres the value is bound directly; in SQLite the
	// column has a DEFAULT and the value is bound for parity.
	CreateRequest(ctx context.Context, orgID, projectID, creatorUserID, userInput string) (string, error)

	// GetRequest reads a single request row, or (nil, nil) if not
	// found. App-pool in Postgres so curator_requests_select RLS
	// gates the read on (org_id, creator_user_id).
	GetRequest(ctx context.Context, orgID, id string) (*domain.CuratorRequest, error)

	// MarkRequestRunning flips queued → running and stamps started_at.
	// Returns sql.ErrNoRows if the row is not currently queued
	// (cancel raced ahead of pickup).
	MarkRequestRunning(ctx context.Context, orgID, id string) error

	// CompleteRequest writes a terminal status + accounting, but
	// ONLY if the row is non-terminal. Returns true if the flip
	// happened. Status is one of done|cancelled|failed.
	CompleteRequest(ctx context.Context, orgID, id, status, errMsg string, costUSD float64, durationMs, numTurns int) (bool, error)

	// MarkRequestCancelledIfActive flips any non-terminal row to
	// cancelled. Returns true if the flip happened. Used by the
	// goroutine's own cancel-observation paths (markCancelled,
	// session.shutdown) — handler-side cancellation still uses
	// the package-level helper today.
	MarkRequestCancelledIfActive(ctx context.Context, orgID, id, errMsg string) (bool, error)

	// InsertMessage writes one curator_messages row and returns
	// its id. The struct's CreatedAt is set to now if zero.
	InsertMessage(ctx context.Context, orgID string, msg *domain.CuratorMessage) (int64, error)

	// DeleteMessagesBySubtype removes every curator_messages row
	// for a request with the given subtype. Used during pending-
	// context revert to drop the `context_change` audit row so the
	// chat history doesn't show a phantom "context noted" entry.
	DeleteMessagesBySubtype(ctx context.Context, orgID, requestID, subtype string) error

	// ConsumePendingContext atomically claims every unconsumed row
	// for the given (project, request) and returns them alongside
	// a fresh snapshot of the project — both reads happen inside
	// the same tx so the diff at the call site is computed against
	// project state consistent with the rows being returned. See
	// the package-level helper for the locking-order rationale.
	//
	// When this method is invoked from inside a SyntheticClaimsWithTx,
	// the outer tx is the locking boundary; the impl does not open
	// its own tx in that case. When invoked against *sql.DB (the
	// future non-curator-goroutine call path), the impl opens a
	// short-lived tx internally.
	ConsumePendingContext(ctx context.Context, orgID, projectID, requestID string) (*domain.Project, []domain.CuratorPendingContext, error)

	// FinalizePendingContext deletes every row consumed by
	// requestID. Called on terminal `done` so the agent's
	// successful absorption of the deltas retires them.
	FinalizePendingContext(ctx context.Context, orgID, requestID string) error

	// RevertPendingContext un-consumes the rows claimed by
	// requestID so the next user message picks them up again.
	// Used on terminal `cancelled` or `failed`. See the package-
	// level helper for the merge semantics.
	RevertPendingContext(ctx context.Context, orgID, requestID string) error
}
