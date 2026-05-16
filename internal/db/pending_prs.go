package db

import (
	"context"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=PendingPRStore --output=./mocks --case=underscore --with-expecter

// ErrPendingPRAlreadyQueued is returned by LockPendingPR when the
// agent has already queued a pending PR for human approval (locked=1).
// Mirrors ErrPendingReviewAlreadySubmitted's purpose: gives the CLI a
// clear sentinel for the SKY-212 anti-retry gate so the agent's tool
// result is unambiguous on the second attempt.
var ErrPendingPRAlreadyQueued = errors.New("pending PR already queued for human approval")

// ErrPendingPRSubmitInFlight is returned by MarkPendingPRSubmitted
// when a concurrent submit attempt has already claimed the row. Two
// browser tabs clicking "Open PR" can't both call CreatePR on
// GitHub; the loser sees this sentinel.
var ErrPendingPRSubmitInFlight = errors.New("pending PR submission already in flight or completed")

// ErrPendingPRSubmitted is returned by UpdateTitleBody when the row's
// submitted_at is non-NULL — the submit guard already claimed the row
// and a CreatePR call is in flight (or has already landed) using the
// values that were in the row at submit time. A PATCH after that
// point can't change what GitHub sees, so silently returning success
// would tell the user "edit saved" when GitHub is about to open the
// PR with the pre-edit values.
var ErrPendingPRSubmitted = errors.New("pending PR is already being submitted; edit dropped")

// PendingPRStore owns the pending_prs table — the agent-drafted PR
// that sits in `pending_approval` until the user accepts / edits /
// discards. Mirror of ReviewStore for the PR-opening path: queue at
// draft time → lock at agent's commit point → user edits via PATCH →
// submit either wins (turn-key open via GitHub API) or fails and is
// released for retry.
//
// All methods take orgID; local mode passes runmode.LocalDefaultOrgID.
// Postgres impl filters on org_id alongside the run_id-gated RLS
// policy as defense in depth; SQLite impl asserts orgID equals the
// local sentinel and otherwise ignores it (single-tenant by design).
type PendingPRStore interface {
	// Create inserts a fresh pending-PR row at queue time. The agent
	// passes title + body as drafted; the impl copies both into the
	// write-once original_* columns so subsequent human edits via
	// UpdateTitleBody can be diffed against the agent's original draft
	// for the human-feedback memory write.
	//
	// run_id is UNIQUE — at most one pending PR per run, parallel to
	// how reviews work. Caller is responsible for not re-inserting;
	// the constraint surfaces as a SQL error if violated.
	Create(ctx context.Context, orgID string, p domain.PendingPR) error

	// Get fetches a single pending-PR row by id, or nil if absent.
	// OriginalTitle / OriginalBody are deliberately NOT COALESCEd:
	// the human-feedback diff distinguishes "no snapshot exists"
	// (nil — legacy row) from "snapshot of a legitimately empty
	// value." See domain.PendingPR's doc-comment.
	Get(ctx context.Context, orgID, id string) (*domain.PendingPR, error)

	// ByRunID returns the pending PR associated with a given agent
	// run, or nil if none. Used by:
	//   - the spawner's terminal-flip check to decide whether to flip
	//     status to pending_approval
	//   - the /api/agent/runs/{runID}/pending-pr endpoint that the
	//     frontend's usePendingApproval hook fetches
	ByRunID(ctx context.Context, orgID, runID string) (*domain.PendingPR, error)

	// UpdateTitleBody is the human-edit path: the user's edits to
	// title/body via the overlay PATCH endpoint. Originals stay
	// frozen via COALESCE so the human-feedback diff retains the
	// agent's draft as the baseline.
	//
	// `submitted_at IS NULL` gates the UPDATE so a PATCH that races
	// a concurrent submit can't silently land after the submit
	// captured the row. Returns ErrPendingPRSubmitted in that case;
	// the handler maps that to a 409 so the browser shows "PR is
	// being opened, your edit didn't apply" rather than a green
	// "saved" toast covering a dropped edit.
	UpdateTitleBody(ctx context.Context, orgID, id, title, body string) error

	// Lock is the agent-side anti-retry gate. The CLI's `pr create`
	// calls it after Create; subsequent agent calls hit the
	// `WHERE locked = 0` clause and get back ErrPendingPRAlreadyQueued
	// — same SKY-212 motivating bug as reviews: agents would loop
	// and call submit-review again after seeing the pending_approval
	// response. The hard error makes the agent's tool result
	// unambiguous on the second attempt.
	//
	// Title and body are passed through (agent sets them), with
	// originals COALESCE'd to preserve any earlier draft if somehow
	// the row was created without them populated. In the normal flow
	// Create already snapshotted originals at insert time, so the
	// COALESCE is defensive.
	Lock(ctx context.Context, orgID, id, title, body string) error

	// MarkSubmitted is the concurrent-submit guard. Two browser tabs
	// clicking "Open PR" simultaneously both hit POST /submit; only
	// one should actually call GitHub's CreatePR. The
	// `WHERE submitted_at IS NULL` clause matches once; the loser
	// sees RowsAffected=0 and gets ErrPendingPRSubmitInFlight (and
	// the existence probe disambiguates that case from a bogus id,
	// which surfaces as a wrapped "not found" error).
	//
	// Returns (winner, err). winner=true means this caller should
	// proceed with CreatePR. winner=false + nil err means another
	// caller already claimed this submission — surface 409 to the
	// user.
	//
	// On submit failure the server should call ClearSubmitted to
	// release the guard so the user can retry without the lock
	// blocking every retry attempt.
	MarkSubmitted(ctx context.Context, orgID, id string) error

	// ClearSubmitted releases the submitted_at guard so a failed
	// submission can be retried by the user. Called from the submit
	// handler's error path after a CreatePR failure: GitHub rejected
	// the PR (422 missing base, head out of sync, network error),
	// but the row should remain editable and resubmittable.
	ClearSubmitted(ctx context.Context, orgID, id string) error

	// Delete removes a pending-PR row by id. Used by the
	// successful-submit path after CreatePR succeeds.
	Delete(ctx context.Context, orgID, id string) error

	// DeleteByRunID tears down a pending PR keyed by the run that
	// produced it. Used by cleanupPendingApprovalRun's
	// drag-back-to-queue / dismiss / claim cascades — the same path
	// that already calls ReviewStore.DeleteByRunID. No-op on a run
	// with no pending PR; safe to call unconditionally.
	DeleteByRunID(ctx context.Context, orgID, runID string) error

	// ByRunIDSystem mirrors ByRunID but routes through the admin
	// pool in Postgres. The delegate spawner's processCompletion
	// reads the pending PR attached to a run from a goroutine that
	// has detached from any request context, so it has no
	// JWT-claims in scope. Behavior matches the non-System variant;
	// the only difference is which Postgres pool the statement runs
	// on; SQLite has one connection and the two variants collapse.
	ByRunIDSystem(ctx context.Context, orgID, runID string) (*domain.PendingPR, error)

	// --- Admin-pool variants for the cmd/exec event-triggered branch (SKY-302) ---
	//
	// `triagefactory exec gh pr-create` invoked by an event-triggered
	// agent run has no user identity to wrap synthetic claims around,
	// so its inserts route through the admin pool here. Manual runs
	// go through SyntheticClaimsWithTx + the non-System methods.
	CreateSystem(ctx context.Context, orgID string, p domain.PendingPR) error
	LockSystem(ctx context.Context, orgID, id, title, body string) error
}
