package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrPendingReviewAlreadySubmitted is returned by
// LockPendingReviewSubmission when the agent has already locked a
// pending review (its original_* columns are populated, meaning a
// prior submit-review call captured the agent's draft). It exists
// to give cmd/exec/gh/pr.go a clear sentinel for SKY-212's
// "block second submit" gate without leaking SQL specifics through
// the call stack.
var ErrPendingReviewAlreadySubmitted = errors.New("pending review already submitted to local approval queue")

func CreatePendingReview(database *sql.DB, r domain.PendingReview) error {
	_, err := database.Exec(
		`INSERT INTO pending_reviews (id, pr_number, owner, repo, commit_sha, diff_lines, diff_hunks, run_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.PRNumber, r.Owner, r.Repo, r.CommitSHA, r.DiffLines, r.DiffHunks, r.RunID,
	)
	return err
}

func GetPendingReview(database *sql.DB, reviewID string) (*domain.PendingReview, error) {
	// original_review_body / original_review_event are deliberately
	// NOT COALESCEd: SKY-205's diff formatter distinguishes "no
	// snapshot exists" (NULL — legacy row predating the columns)
	// from "snapshot of a legitimately empty value." Folding them
	// together via COALESCE(..., '') would make a human-added body
	// against an originally-empty draft look like legacy and
	// silently suppress the diff section.
	row := database.QueryRow(`
		SELECT id, pr_number, owner, repo, commit_sha,
		       COALESCE(diff_lines, ''), COALESCE(diff_hunks, ''), COALESCE(run_id, ''),
		       COALESCE(review_body, ''), COALESCE(review_event, ''),
		       original_review_body, original_review_event
		FROM pending_reviews WHERE id = ?`, reviewID)
	var r domain.PendingReview
	var origBody, origEvent sql.NullString
	err := row.Scan(
		&r.ID, &r.PRNumber, &r.Owner, &r.Repo, &r.CommitSHA,
		&r.DiffLines, &r.DiffHunks, &r.RunID, &r.ReviewBody, &r.ReviewEvent,
		&origBody, &origEvent,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if origBody.Valid {
		s := origBody.String
		r.OriginalReviewBody = &s
	}
	if origEvent.Valid {
		s := origEvent.String
		r.OriginalReviewEvent = &s
	}
	return &r, nil
}

// AddPendingReviewComment inserts a pending review comment, snapshotting
// `body` into the write-once `original_body` column at the same time.
// Subsequent edits via UpdatePendingReviewComment mutate `body` only;
// `original_body` is the durable record of the agent's draft so the
// follow-up workstream (SKY-205 / SKY-206) can compute a diff for the
// human-feedback memory write.
func AddPendingReviewComment(database *sql.DB, c domain.PendingReviewComment) error {
	_, err := database.Exec(
		`INSERT INTO pending_review_comments (id, review_id, path, line, start_line, body, original_body) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.ReviewID, c.Path, c.Line, c.StartLine, c.Body, c.Body,
	)
	return err
}

func UpdatePendingReviewComment(database *sql.DB, commentID, body string) error {
	result, err := database.Exec(`UPDATE pending_review_comments SET body = ? WHERE id = ?`, body, commentID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("pending comment %s not found", commentID)
	}
	return nil
}

func DeletePendingReviewComment(database *sql.DB, commentID string) error {
	result, err := database.Exec(`DELETE FROM pending_review_comments WHERE id = ?`, commentID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("pending comment %s not found", commentID)
	}
	return nil
}

func ListPendingReviewComments(database *sql.DB, reviewID string) ([]domain.PendingReviewComment, error) {
	// original_body NOT COALESCEd — see GetPendingReview's note.
	// nil OriginalBody === "legacy row, no snapshot," distinct from
	// a non-nil pointer to "" which means "agent drafted an empty
	// comment body" (rare, but the formatter must classify edits
	// against the real value rather than dismissing the row).
	rows, err := database.Query(
		`SELECT id, review_id, path, line, start_line, body, original_body
		 FROM pending_review_comments WHERE review_id = ? ORDER BY rowid`,
		reviewID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var comments []domain.PendingReviewComment
	for rows.Next() {
		var c domain.PendingReviewComment
		var startLine sql.NullInt64
		var origBody sql.NullString
		if err := rows.Scan(&c.ID, &c.ReviewID, &c.Path, &c.Line, &startLine, &c.Body, &origBody); err != nil {
			return nil, err
		}
		if startLine.Valid {
			v := int(startLine.Int64)
			c.StartLine = &v
		}
		if origBody.Valid {
			s := origBody.String
			c.OriginalBody = &s
		}
		comments = append(comments, c)
	}
	return comments, rows.Err()
}

// DeletePendingReviewByRunID tears down a pending review (and its
// cascaded comments) keyed by the run that produced it. Used by
// SKY-206's discard cleanup so a transient failure in a separate
// lookup-by-id query doesn't strand the review row in the DB —
// which is precisely the stale-state bug that workstream is fixing.
//
// Single transaction across the comments + review delete: if
// either statement fails, the entire teardown rolls back and the
// caller's error propagates rather than leaving a half-cleaned
// state where comments are gone but the review row remains
// (or vice versa). No-op on a run with no pending review (0 rows
// affected on both DELETEs); safe to call unconditionally.
func DeletePendingReviewByRunID(database *sql.DB, runID string) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`DELETE FROM pending_review_comments
		 WHERE review_id IN (SELECT id FROM pending_reviews WHERE run_id = ?)`,
		runID,
	); err != nil {
		return fmt.Errorf("delete review comments by run: %w", err)
	}
	if _, err := tx.Exec(
		`DELETE FROM pending_reviews WHERE run_id = ?`,
		runID,
	); err != nil {
		return fmt.Errorf("delete review by run: %w", err)
	}
	return tx.Commit()
}

// DeletePendingReview removes a review and all its comments (on submit or cancel).
func DeletePendingReview(database *sql.DB, reviewID string) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM pending_review_comments WHERE review_id = ?`, reviewID); err != nil {
		return fmt.Errorf("delete review comments: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM pending_reviews WHERE id = ?`, reviewID); err != nil {
		return fmt.Errorf("delete review: %w", err)
	}
	return tx.Commit()
}

// SetPendingReviewSubmission stores the deferred review body and event,
// marking the review as ready for user approval rather than immediate
// GitHub submission. The `original_review_body` and
// `original_review_event` columns are populated once via COALESCE: the
// first call captures the agent's drafted body + event, later calls
// (which may carry user-edited values from handleReviewUpdate) leave
// the snapshots untouched. Encoding the write-once contract in SQL —
// rather than a SELECT-then-UPDATE in Go — keeps it race-free without
// a transaction.
func SetPendingReviewSubmission(database *sql.DB, reviewID, body, event string) error {
	_, err := database.Exec(
		`UPDATE pending_reviews
		 SET review_body = ?,
		     review_event = ?,
		     original_review_body = COALESCE(original_review_body, ?),
		     original_review_event = COALESCE(original_review_event, ?)
		 WHERE id = ?`,
		body, event, body, event, reviewID,
	)
	return err
}

// LockPendingReviewSubmission is the agent-side variant of
// SetPendingReviewSubmission used by cmd/exec/gh's prSubmitReview.
// It captures the agent's drafted body + event AND seals the
// review against further agent submissions in a single atomic
// UPDATE.
//
// The gate is `review_event IS NULL OR review_event = ”`: the
// baseline schema leaves review_event empty on fresh
// CreatePendingReview rows and any code path that finalizes a
// submission populates it. That makes review_event the
// era-independent "has been submitted" signal — which matters
// because original_review_event was added later (SKY-205) and
// pre-SKY-205 rows can sit in the DB with original_review_event
// NULL even though they were already submitted (review_event +
// original_review_body populated via the SKY-204 COALESCE writer).
// Gating on original_review_event would falsely open the lock for
// those rows.
//
// Originals use COALESCE so legacy rows with a SKY-204-era
// original_review_body keep their snapshot if the gate ever lets a
// write through (e.g. a fresh row from before either column
// existed) — preserving the write-once contract regardless of
// migration era.
//
// On second-and-later agent calls, RowsAffected is 0 and the
// helper returns ErrPendingReviewAlreadySubmitted. SKY-212's
// motivating bug: agents would call submit-review, see the
// pending_approval response, then loop and call it again. The
// gate forces a hard error on the second attempt so the agent's
// tool result is unambiguous ("you already queued this review").
//
// Human edits via handleReviewUpdate stay on
// SetPendingReviewSubmission — that path needs to update body /
// event after the agent has already locked the review, and the
// COALESCE in the existing function preserves originals through
// those edits. Splitting into two functions keeps the gate
// behavior off the human path.
func LockPendingReviewSubmission(database *sql.DB, reviewID, body, event string) error {
	res, err := database.Exec(
		`UPDATE pending_reviews
		 SET review_body = ?,
		     review_event = ?,
		     original_review_body = COALESCE(original_review_body, ?),
		     original_review_event = COALESCE(original_review_event, ?)
		 WHERE id = ? AND (review_event IS NULL OR review_event = '')`,
		body, event, body, event, reviewID,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Distinguish "already submitted" from "no such review".
		// A bogus reviewID shouldn't get the SKY-212 lock message;
		// the agent should see a different error pointing at the
		// argument. The lookup is cheap and the row is already in
		// the page cache from the prSubmitReview load.
		var exists int
		if err := database.QueryRow(`SELECT COUNT(*) FROM pending_reviews WHERE id = ?`, reviewID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("pending review %s not found", reviewID)
		}
		return ErrPendingReviewAlreadySubmitted
	}
	return nil
}

// PendingReviewByRunID returns the pending review associated with a given agent
// run that has a deferred submission (review_event is set). Returns nil if none.
func PendingReviewByRunID(database *sql.DB, runID string) (*domain.PendingReview, error) {
	row := database.QueryRow(
		`SELECT id, pr_number, owner, repo, commit_sha, COALESCE(diff_lines, ''), COALESCE(run_id, ''), COALESCE(review_body, ''), COALESCE(review_event, '')
		 FROM pending_reviews WHERE run_id = ? AND review_event IS NOT NULL AND review_event != ''`, runID)
	var r domain.PendingReview
	err := row.Scan(&r.ID, &r.PRNumber, &r.Owner, &r.Repo, &r.CommitSHA, &r.DiffLines, &r.RunID, &r.ReviewBody, &r.ReviewEvent)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &r, err
}

// IsPendingCommentID checks if a comment ID belongs to a local pending review.
func IsPendingCommentID(database *sql.DB, commentID string) bool {
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM pending_review_comments WHERE id = ?`, commentID).Scan(&count); err != nil {
		return false
	}
	return count > 0
}
