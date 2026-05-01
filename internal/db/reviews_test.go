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

// TestSetPendingReviewSubmission_WriteOnceOriginals pins the
// COALESCE-encoded write-once contract for both review body and
// review event. First call captures the agent's drafted body +
// event; second call (typically a user-edited resubmission via
// handleReviewUpdate) updates review_body / review_event but
// leaves both originals pinned at the agent's first draft.
//
// The original_review_event half is the SKY-205 addition — SKY-204
// captured original_review_body but missed the parallel for the
// verdict, leaving the human-feedback writer unable to detect
// agent-drafted-APPROVE → human-submitted-REQUEST_CHANGES (the
// highest-signal change the workstream is meant to preserve).
func TestSetPendingReviewSubmission_WriteOnceOriginals(t *testing.T) {
	db := newTestDB(t)
	seedPendingReview(t, db, "rev3")

	if err := SetPendingReviewSubmission(db, "rev3", "agent draft body", "APPROVE"); err != nil {
		t.Fatalf("first SetPendingReviewSubmission: %v", err)
	}
	if err := SetPendingReviewSubmission(db, "rev3", "user edited body", "REQUEST_CHANGES"); err != nil {
		t.Fatalf("second SetPendingReviewSubmission: %v", err)
	}

	var body, event, origBody, origEvent sql.NullString
	if err := db.QueryRow(
		`SELECT review_body, review_event, original_review_body, original_review_event
		 FROM pending_reviews WHERE id = ?`, "rev3",
	).Scan(&body, &event, &origBody, &origEvent); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if body.String != "user edited body" {
		t.Errorf("review_body = %q, want %q", body.String, "user edited body")
	}
	if event.String != "REQUEST_CHANGES" {
		t.Errorf("review_event = %q, want %q", event.String, "REQUEST_CHANGES")
	}
	if origBody.String != "agent draft body" {
		t.Errorf("original_review_body = %q, want %q (must remain agent's first draft)",
			origBody.String, "agent draft body")
	}
	if origEvent.String != "APPROVE" {
		t.Errorf("original_review_event = %q, want %q (must remain agent's first draft)",
			origEvent.String, "APPROVE")
	}
}

// TestGetPendingReview_ProjectsOriginals confirms the helper
// surfaces the new write-once columns through the domain type so
// the diff formatter has them to work with. Without this projection
// SKY-205's writer would silently degrade to legacy mode (NULL
// originals → no diff) on every submit.
func TestGetPendingReview_ProjectsOriginals(t *testing.T) {
	db := newTestDB(t)
	seedPendingReview(t, db, "rev_project")
	if err := SetPendingReviewSubmission(db, "rev_project", "draft", "APPROVE"); err != nil {
		t.Fatalf("SetPendingReviewSubmission: %v", err)
	}

	got, err := GetPendingReview(db, "rev_project")
	if err != nil {
		t.Fatalf("GetPendingReview: %v", err)
	}
	if got == nil {
		t.Fatal("GetPendingReview returned nil")
	}
	if got.OriginalReviewBody == nil || *got.OriginalReviewBody != "draft" {
		t.Errorf("OriginalReviewBody = %v, want pointer to %q", got.OriginalReviewBody, "draft")
	}
	if got.OriginalReviewEvent == nil || *got.OriginalReviewEvent != "APPROVE" {
		t.Errorf("OriginalReviewEvent = %v, want pointer to %q", got.OriginalReviewEvent, "APPROVE")
	}
}

// TestGetPendingReview_LegacyOriginalsAreNil is the regression for
// the COALESCE-collapses-snapshot-semantics bug. A row with NULL
// original_review_body / original_review_event (legacy, mid-flight
// when the columns landed) must surface as a nil pointer — not as
// the empty string, which would be indistinguishable from a real
// snapshot of an empty drafted body.
func TestGetPendingReview_LegacyOriginalsAreNil(t *testing.T) {
	db := newTestDB(t)
	seedPendingReview(t, db, "rev_legacy")
	// Direct UPDATE bypasses SetPendingReviewSubmission so the
	// originals stay NULL (simulating a row that existed before the
	// COALESCE writers were added).
	if _, err := db.Exec(
		`UPDATE pending_reviews SET review_body = ?, review_event = ? WHERE id = ?`,
		"final", "APPROVE", "rev_legacy",
	); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	got, err := GetPendingReview(db, "rev_legacy")
	if err != nil {
		t.Fatalf("GetPendingReview: %v", err)
	}
	if got.OriginalReviewBody != nil {
		t.Errorf("OriginalReviewBody = %v, want nil (legacy NULL must not collapse to empty string)", *got.OriginalReviewBody)
	}
	if got.OriginalReviewEvent != nil {
		t.Errorf("OriginalReviewEvent = %v, want nil", *got.OriginalReviewEvent)
	}
}

// TestGetPendingReview_EmptyOriginalIsRealSnapshot is the inverse:
// a legitimately-empty agent-drafted body (e.g. the agent posted
// only inline comments, no top-level prose) must surface as a
// non-nil pointer to "". Folding it onto NULL via COALESCE would
// suppress the diff section when a human later adds body text.
func TestGetPendingReview_EmptyOriginalIsRealSnapshot(t *testing.T) {
	db := newTestDB(t)
	seedPendingReview(t, db, "rev_empty_orig")
	if err := SetPendingReviewSubmission(db, "rev_empty_orig", "", "COMMENT"); err != nil {
		t.Fatalf("SetPendingReviewSubmission: %v", err)
	}

	got, err := GetPendingReview(db, "rev_empty_orig")
	if err != nil {
		t.Fatalf("GetPendingReview: %v", err)
	}
	if got.OriginalReviewBody == nil {
		t.Errorf("OriginalReviewBody = nil, want non-nil pointer to \"\" (real snapshot of empty draft)")
	} else if *got.OriginalReviewBody != "" {
		t.Errorf("OriginalReviewBody = %q, want %q", *got.OriginalReviewBody, "")
	}
}

// TestListPendingReviewComments_ProjectsOriginalBody mirrors the
// review-side test for the comment list helper. Without this
// projection the formatter sees Original = "" for every comment and
// can't classify edits.
func TestListPendingReviewComments_ProjectsOriginalBody(t *testing.T) {
	db := newTestDB(t)
	seedPendingReview(t, db, "rev_comments")
	if err := AddPendingReviewComment(db, domain.PendingReviewComment{
		ID: "c_proj", ReviewID: "rev_comments", Path: "x.go", Line: 1, Body: "agent draft",
	}); err != nil {
		t.Fatalf("AddPendingReviewComment: %v", err)
	}
	if err := UpdatePendingReviewComment(db, "c_proj", "user edit"); err != nil {
		t.Fatalf("UpdatePendingReviewComment: %v", err)
	}

	got, err := ListPendingReviewComments(db, "rev_comments")
	if err != nil {
		t.Fatalf("ListPendingReviewComments: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Body != "user edit" {
		t.Errorf("Body = %q, want %q", got[0].Body, "user edit")
	}
	if got[0].OriginalBody == nil || *got[0].OriginalBody != "agent draft" {
		t.Errorf("OriginalBody = %v, want pointer to %q (helper must project original_body)",
			got[0].OriginalBody, "agent draft")
	}
}

// TestListPendingReviewComments_LegacyOriginalBodyIsNil mirrors the
// review-side legacy regression. A pre-SKY-204 comment row whose
// original_body column is NULL must surface as nil so the
// formatter folds it onto unchanged rather than emitting a
// fabricated Was: "" diff entry against the user's current body.
func TestListPendingReviewComments_LegacyOriginalBodyIsNil(t *testing.T) {
	db := newTestDB(t)
	seedPendingReview(t, db, "rev_legacy_c")
	// Bypass AddPendingReviewComment (which writes original_body)
	// so the row matches the pre-SKY-204 shape: original_body NULL.
	if _, err := db.Exec(
		`INSERT INTO pending_review_comments (id, review_id, path, line, body)
		 VALUES (?, ?, ?, ?, ?)`,
		"c_legacy", "rev_legacy_c", "x.go", 1, "legacy comment",
	); err != nil {
		t.Fatalf("seed legacy comment: %v", err)
	}

	got, err := ListPendingReviewComments(db, "rev_legacy_c")
	if err != nil {
		t.Fatalf("ListPendingReviewComments: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].OriginalBody != nil {
		t.Errorf("OriginalBody = %v, want nil (legacy NULL must not collapse to empty string)", *got[0].OriginalBody)
	}
}
