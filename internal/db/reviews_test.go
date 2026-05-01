package db

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// seedPendingReview installs a pending_reviews row so the tests below
// can attach comments + exercise SetPendingReviewSubmission. Lean
// fixture — the column list mirrors what GetPendingReview actually
// scans for, nothing more.
func seedPendingReview(t *testing.T, db *sql.DB, reviewID string) {
	t.Helper()
	if err := CreatePendingReview(db, domain.PendingReview{
		ID: reviewID, PRNumber: 42, Owner: "owner", Repo: "repo", CommitSHA: "sha", DiffLines: "", RunID: "",
	}); err != nil {
		t.Fatalf("CreatePendingReview: %v", err)
	}
}

// TestAddPendingReviewComment_SnapshotsOriginalBody pins the SKY-204
// write-once contract for comments: the agent's drafted body is
// captured into `original_body` at INSERT time. Without this the
// follow-up workstream (SKY-205 / SKY-206) couldn't compute a diff
// between the agent's draft and the user's edits.
func TestAddPendingReviewComment_SnapshotsOriginalBody(t *testing.T) {
	db := newTestDB(t)
	seedPendingReview(t, db, "rev1")

	if err := AddPendingReviewComment(db, domain.PendingReviewComment{
		ID: "c1", ReviewID: "rev1", Path: "foo.go", Line: 10, Body: "agent draft",
	}); err != nil {
		t.Fatalf("AddPendingReviewComment: %v", err)
	}

	var body, original sql.NullString
	if err := db.QueryRow(
		`SELECT body, original_body FROM pending_review_comments WHERE id = ?`, "c1",
	).Scan(&body, &original); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if body.String != "agent draft" {
		t.Errorf("body = %q, want %q", body.String, "agent draft")
	}
	if original.String != "agent draft" {
		t.Errorf("original_body = %q, want %q (must mirror body at insert)", original.String, "agent draft")
	}
}

// TestUpdatePendingReviewComment_PreservesOriginalBody is the
// regression for the "user edits trample provenance" failure mode the
// workstream is fixing. After a user edit, body changes but
// original_body stays anchored to the agent's first draft.
func TestUpdatePendingReviewComment_PreservesOriginalBody(t *testing.T) {
	db := newTestDB(t)
	seedPendingReview(t, db, "rev2")
	if err := AddPendingReviewComment(db, domain.PendingReviewComment{
		ID: "c2", ReviewID: "rev2", Path: "foo.go", Line: 20, Body: "agent draft",
	}); err != nil {
		t.Fatalf("AddPendingReviewComment: %v", err)
	}

	if err := UpdatePendingReviewComment(db, "c2", "user edit"); err != nil {
		t.Fatalf("UpdatePendingReviewComment: %v", err)
	}

	var body, original sql.NullString
	if err := db.QueryRow(
		`SELECT body, original_body FROM pending_review_comments WHERE id = ?`, "c2",
	).Scan(&body, &original); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if body.String != "user edit" {
		t.Errorf("body = %q, want %q (update should mutate body)", body.String, "user edit")
	}
	if original.String != "agent draft" {
		t.Errorf("original_body = %q, want %q (update must NOT touch original_body)", original.String, "agent draft")
	}
}

// TestSetPendingReviewSubmission_WriteOnceOriginalBody pins the
// COALESCE-encoded write-once contract for review bodies. First call
// captures the agent draft; second call (typically a user-edited
// resubmission) updates review_body but leaves original_review_body
// pinned at the agent's original.
func TestSetPendingReviewSubmission_WriteOnceOriginalBody(t *testing.T) {
	db := newTestDB(t)
	seedPendingReview(t, db, "rev3")

	if err := SetPendingReviewSubmission(db, "rev3", "agent draft body", "COMMENT"); err != nil {
		t.Fatalf("first SetPendingReviewSubmission: %v", err)
	}
	if err := SetPendingReviewSubmission(db, "rev3", "user edited body", "REQUEST_CHANGES"); err != nil {
		t.Fatalf("second SetPendingReviewSubmission: %v", err)
	}

	var body, event, original sql.NullString
	if err := db.QueryRow(
		`SELECT review_body, review_event, original_review_body FROM pending_reviews WHERE id = ?`, "rev3",
	).Scan(&body, &event, &original); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if body.String != "user edited body" {
		t.Errorf("review_body = %q, want %q", body.String, "user edited body")
	}
	if event.String != "REQUEST_CHANGES" {
		t.Errorf("review_event = %q, want %q", event.String, "REQUEST_CHANGES")
	}
	if original.String != "agent draft body" {
		t.Errorf("original_review_body = %q, want %q (must remain agent's first draft)", original.String, "agent draft body")
	}
}
