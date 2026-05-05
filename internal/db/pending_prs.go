package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

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

// CreatePendingPR inserts a fresh pending-PR row at queue time. The
// agent passes title + body as drafted; we copy both into the
// write-once original_* columns so subsequent human edits via
// UpdatePendingPRTitleBody can be diffed against the agent's
// original draft for the human-feedback memory write.
//
// run_id is UNIQUE — at most one pending PR per run, parallel to how
// reviews work. Caller is responsible for not re-inserting; the
// constraint surfaces as a SQL error if violated.
func CreatePendingPR(database *sql.DB, p domain.PendingPR) error {
	_, err := database.Exec(
		`INSERT INTO pending_prs
		   (id, run_id, owner, repo, head_branch, head_sha, base_branch,
		    title, body, original_title, original_body, draft)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.RunID, p.Owner, p.Repo, p.HeadBranch, p.HeadSHA, p.BaseBranch,
		p.Title, nullIfEmpty(p.Body),
		// Snapshot the agent's draft as originals at insert time so
		// the human-feedback diff has a stable baseline even if the
		// user edits before the agent has called LockPendingPR.
		p.Title, nullIfEmpty(p.Body),
		boolToInt(p.Draft),
	)
	return err
}

// boolToInt is the SQLite-idiomatic 0/1 mapping. Defined locally to
// avoid leaking a generic helper into a wider scope.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// GetPendingPR fetches a single pending-PR row.
//
// original_title / original_body are deliberately NOT COALESCEd: the
// human-feedback diff distinguishes "no snapshot exists" (NULL —
// legacy row predating the columns) from "snapshot of a legitimately
// empty value." Folding them together via COALESCE(..., ”) would
// make a human-added body against an originally-empty draft look
// like legacy and silently suppress the diff section.
func GetPendingPR(database *sql.DB, id string) (*domain.PendingPR, error) {
	row := database.QueryRow(`
		SELECT id, run_id, owner, repo, head_branch, head_sha, base_branch,
		       title, COALESCE(body, ''),
		       original_title, original_body,
		       draft, locked, submitted_at, created_at
		  FROM pending_prs
		 WHERE id = ?`, id)
	var p domain.PendingPR
	var origTitle, origBody sql.NullString
	var draft, locked int
	var submittedAt sql.NullTime
	if err := row.Scan(
		&p.ID, &p.RunID, &p.Owner, &p.Repo, &p.HeadBranch, &p.HeadSHA, &p.BaseBranch,
		&p.Title, &p.Body,
		&origTitle, &origBody,
		&draft, &locked, &submittedAt, &p.CreatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if origTitle.Valid {
		s := origTitle.String
		p.OriginalTitle = &s
	}
	if origBody.Valid {
		s := origBody.String
		p.OriginalBody = &s
	}
	p.Draft = draft != 0
	p.Locked = locked != 0
	if submittedAt.Valid {
		t := submittedAt.Time
		p.SubmittedAt = &t
	}
	return &p, nil
}

// ErrPendingPRSubmitted is returned by UpdatePendingPRTitleBody when
// the row's submitted_at is non-NULL — the submit guard already
// claimed the row and a CreatePR call is in flight (or has already
// landed) using the values that were in the row at submit time. A
// PATCH after that point can't change what GitHub sees, so silently
// returning success would tell the user "edit saved" when GitHub is
// about to open the PR with the pre-edit values.
var ErrPendingPRSubmitted = errors.New("pending PR is already being submitted; edit dropped")

// UpdatePendingPRTitleBody is the human-edit path: the user's
// edits to title/body via the overlay PATCH endpoint. Originals stay
// frozen via COALESCE so the human-feedback diff retains the agent's
// draft as the baseline. Mirror of SetPendingReviewSubmission's
// COALESCE pattern.
//
// `submitted_at IS NULL` gates the UPDATE so a PATCH that races a
// concurrent submit can't silently land after the submit captured
// the row. The handler maps ErrPendingPRSubmitted to a 409 so the
// browser shows "PR is being opened, your edit didn't apply" rather
// than a green "saved" toast covering a dropped edit.
func UpdatePendingPRTitleBody(database *sql.DB, id, title, body string) error {
	res, err := database.Exec(
		`UPDATE pending_prs
		    SET title = ?,
		        body = ?,
		        original_title = COALESCE(original_title, ?),
		        original_body = COALESCE(original_body, ?)
		  WHERE id = ? AND submitted_at IS NULL`,
		title, nullIfEmpty(body), title, nullIfEmpty(body), id,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Disambiguate "row not found" from "row exists but already
		// submitted" so the caller can give the user the right
		// reason. A second SELECT is cheap relative to the cost of
		// a misleading toast.
		var submittedAt sql.NullTime
		err := database.QueryRow(`SELECT submitted_at FROM pending_prs WHERE id = ?`, id).Scan(&submittedAt)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("pending PR %s not found", id)
			}
			return err
		}
		if submittedAt.Valid {
			return ErrPendingPRSubmitted
		}
		// Row exists, submitted_at is NULL, but UPDATE matched zero
		// rows. Shouldn't happen — surface as a generic error rather
		// than silently lying about success.
		return fmt.Errorf("pending PR %s update matched 0 rows but row state is consistent; investigate", id)
	}
	return nil
}

// LockPendingPR is the agent-side anti-retry gate. The CLI's
// `pr create` calls it after CreatePendingPR; subsequent agent calls
// hit the `WHERE locked = 0` clause and get back
// ErrPendingPRAlreadyQueued — same SKY-212 motivating bug as reviews:
// agents would loop and call submit-review again after seeing the
// pending_approval response. The hard error makes the agent's tool
// result unambiguous on the second attempt.
//
// Title and body are passed through (agent sets them), with originals
// COALESCE'd to preserve any earlier draft if somehow the row was
// created without them populated. In the normal flow CreatePendingPR
// already snapshotted originals at insert time, so the COALESCE is
// defensive.
func LockPendingPR(database *sql.DB, id, title, body string) error {
	res, err := database.Exec(
		`UPDATE pending_prs
		    SET title = ?,
		        body = ?,
		        original_title = COALESCE(original_title, ?),
		        original_body = COALESCE(original_body, ?),
		        locked = 1
		  WHERE id = ? AND locked = 0`,
		title, nullIfEmpty(body), title, nullIfEmpty(body), id,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Distinguish "already queued" from "no such PR" — same
		// SKY-212 disambiguation pattern as LockPendingReviewSubmission.
		// A bogus id shouldn't get the lock message; the agent should
		// see a different error pointing at the argument.
		var exists int
		if err := database.QueryRow(`SELECT COUNT(*) FROM pending_prs WHERE id = ?`, id).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("pending PR %s not found", id)
		}
		return ErrPendingPRAlreadyQueued
	}
	return nil
}

// MarkPendingPRSubmitted is the concurrent-submit guard. Two browser
// tabs clicking "Open PR" simultaneously both hit POST /submit; only
// one should actually call GitHub's CreatePR. The
// `WHERE submitted_at IS NULL` clause matches once; the loser sees
// RowsAffected=0 and gets ErrPendingPRSubmitInFlight.
//
// Returns (winner, err). winner=true means this caller should proceed
// with CreatePR. winner=false + nil err means another caller already
// claimed this submission — surface 409 to the user.
//
// On submit failure the server should call ClearPendingPRSubmitted to
// release the guard so the user can retry without the lock blocking
// every retry attempt.
func MarkPendingPRSubmitted(database *sql.DB, id string) (winner bool, err error) {
	res, err := database.Exec(
		`UPDATE pending_prs
		    SET submitted_at = ?
		  WHERE id = ? AND submitted_at IS NULL`,
		time.Now().UTC(), id,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		// Distinguish "in flight" from "no such PR".
		var exists int
		if qerr := database.QueryRow(`SELECT COUNT(*) FROM pending_prs WHERE id = ?`, id).Scan(&exists); qerr != nil {
			return false, qerr
		}
		if exists == 0 {
			return false, fmt.Errorf("pending PR %s not found", id)
		}
		return false, ErrPendingPRSubmitInFlight
	}
	return true, nil
}

// ClearPendingPRSubmitted releases the submitted_at guard so a
// failed submission can be retried by the user. Called from the
// submit handler's error path after a CreatePR failure: GitHub
// rejected the PR (422 missing base, head out of sync, network
// error), but the row should remain editable and resubmittable.
func ClearPendingPRSubmitted(database *sql.DB, id string) error {
	_, err := database.Exec(
		`UPDATE pending_prs SET submitted_at = NULL WHERE id = ?`, id,
	)
	return err
}

// DeletePendingPR removes a pending-PR row by id. Used by the
// successful-submit path after CreatePR succeeds.
func DeletePendingPR(database *sql.DB, id string) error {
	res, err := database.Exec(`DELETE FROM pending_prs WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("pending PR %s not found", id)
	}
	return nil
}

// DeletePendingPRByRunID tears down a pending PR keyed by the run
// that produced it. Used by cleanupPendingApprovalRun's
// drag-back-to-queue / dismiss / claim cascades — the same path that
// already calls DeletePendingReviewByRunID. No-op on a run with no
// pending PR (0 rows affected); safe to call unconditionally.
func DeletePendingPRByRunID(database *sql.DB, runID string) error {
	_, err := database.Exec(`DELETE FROM pending_prs WHERE run_id = ?`, runID)
	return err
}

// PendingPRByRunID returns the pending PR associated with a given
// agent run, or nil if none. Used by:
//   - the spawner's terminal-flip check to decide whether to flip
//     status to pending_approval
//   - the /api/agent/runs/{runID}/pending-pr endpoint that the
//     frontend's usePendingApproval hook fetches
func PendingPRByRunID(database *sql.DB, runID string) (*domain.PendingPR, error) {
	row := database.QueryRow(`
		SELECT id, run_id, owner, repo, head_branch, head_sha, base_branch,
		       title, COALESCE(body, ''),
		       original_title, original_body,
		       draft, locked, submitted_at, created_at
		  FROM pending_prs
		 WHERE run_id = ?`, runID)
	var p domain.PendingPR
	var origTitle, origBody sql.NullString
	var draft, locked int
	var submittedAt sql.NullTime
	if err := row.Scan(
		&p.ID, &p.RunID, &p.Owner, &p.Repo, &p.HeadBranch, &p.HeadSHA, &p.BaseBranch,
		&p.Title, &p.Body,
		&origTitle, &origBody,
		&draft, &locked, &submittedAt, &p.CreatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if origTitle.Valid {
		s := origTitle.String
		p.OriginalTitle = &s
	}
	if origBody.Valid {
		s := origBody.String
		p.OriginalBody = &s
	}
	p.Draft = draft != 0
	p.Locked = locked != 0
	if submittedAt.Valid {
		t := submittedAt.Time
		p.SubmittedAt = &t
	}
	return &p, nil
}
