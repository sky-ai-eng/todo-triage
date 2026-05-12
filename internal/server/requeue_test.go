package server

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// pendingApprovalFixture installs the full FK chain for a task whose
// delegated run is sitting in pending_approval with a saved review +
// comments + agent-side memory row. Returns (taskID, runID,
// reviewID). Centralized here so each requeue test exercises the
// exact shape SKY-206 is meant to clean up: agent finished, wrote
// memory, prepared a review, awaiting human submit.
func pendingApprovalFixture(t *testing.T, database *sql.DB) (taskID, runID, reviewID string) {
	t.Helper()

	const eventType = "github:pr:ci_check_passed"
	if _, err := database.Exec(`
		INSERT INTO entities (id, source, source_id, kind, state)
		VALUES ('e_pa', 'github', 'owner/repo#pa', 'pr', 'active');
		INSERT INTO events (id, entity_id, event_type, dedup_key)
		VALUES ('ev_pa', 'e_pa', ?, '');
		INSERT INTO prompts (id, name, body) VALUES ('p_pa', 'Review', 'body');
		INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		VALUES ('t_pa', 'e_pa', ?, 'ev_pa', 'delegated');
		INSERT INTO runs (id, task_id, prompt_id, status, trigger_type)
		VALUES ('r_pa', 't_pa', 'p_pa', 'pending_approval', 'manual');
	`, eventType, eventType); err != nil {
		t.Fatalf("seed FK chain: %v", err)
	}

	// run_memory: agent finished and wrote its self-report (the
	// SKY-204 termination upsert). We assert below that
	// human_content lands without trampling agent_content.
	if err := db.UpsertAgentMemory(database, "r_pa", "e_pa", "agent self-report"); err != nil {
		t.Fatalf("UpsertAgentMemory: %v", err)
	}

	// Pending review with one comment, populated via the same
	// helpers production uses so the original_* columns get the
	// real write-once snapshots.
	if err := db.CreatePendingReview(database, domain.PendingReview{
		ID: "rev_pa", PRNumber: 7, Owner: "owner", Repo: "repo", CommitSHA: "sha", DiffLines: "", RunID: "r_pa",
	}); err != nil {
		t.Fatalf("CreatePendingReview: %v", err)
	}
	if err := db.AddPendingReviewComment(database, domain.PendingReviewComment{
		ID: "c_pa", ReviewID: "rev_pa", Path: "x.go", Line: 1, Body: "agent comment",
	}); err != nil {
		t.Fatalf("AddPendingReviewComment: %v", err)
	}
	if err := db.SetPendingReviewSubmission(database, "rev_pa", "agent draft body", "APPROVE"); err != nil {
		t.Fatalf("SetPendingReviewSubmission: %v", err)
	}
	return "t_pa", "r_pa", "rev_pa"
}

// assertPendingApprovalCleanedUp checks every post-condition the
// SKY-206 cleanup is meant to deliver: task at the expected
// post-state, run cancelled with the discriminator stop_reason,
// pending_reviews + comments gone, human_content recording the
// discard with a marker phrase that distinguishes the requeue
// from the dismiss flavor, agent_content preserved (the whole
// point of SKY-204 was keeping both halves). wantTaskStatus and
// wantHumanContentMarker let callers vary the assertion across
// the requeue (`queued` + "returned to the triage queue") and
// dismiss (`dismissed` + "dismissed the task entirely") paths.
func assertPendingApprovalCleanedUp(
	t *testing.T,
	database *sql.DB,
	taskID, runID, reviewID string,
	wantTaskStatus, wantHumanContentMarker string,
) {
	t.Helper()

	var taskStatus string
	if err := database.QueryRow(`SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&taskStatus); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if taskStatus != wantTaskStatus {
		t.Errorf("task.status = %q, want %q", taskStatus, wantTaskStatus)
	}

	var runStatus, stopReason string
	var completedAt sql.NullTime
	if err := database.QueryRow(
		`SELECT status, COALESCE(stop_reason, ''), completed_at FROM runs WHERE id = ?`, runID,
	).Scan(&runStatus, &stopReason, &completedAt); err != nil {
		t.Fatalf("scan run: %v", err)
	}
	if runStatus != "cancelled" {
		t.Errorf("run.status = %q, want %q", runStatus, "cancelled")
	}
	if stopReason != "review_discarded_by_user" {
		t.Errorf("run.stop_reason = %q, want %q", stopReason, "review_discarded_by_user")
	}
	if !completedAt.Valid {
		t.Errorf("run.completed_at not populated")
	}

	var revCount, commentCount int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM pending_reviews WHERE id = ?`, reviewID,
	).Scan(&revCount); err != nil {
		t.Fatalf("scan pending_reviews count: %v", err)
	}
	if revCount != 0 {
		t.Errorf("pending_reviews count = %d, want 0", revCount)
	}
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM pending_review_comments WHERE review_id = ?`, reviewID,
	).Scan(&commentCount); err != nil {
		t.Fatalf("scan pending_review_comments count: %v", err)
	}
	if commentCount != 0 {
		t.Errorf("pending_review_comments count = %d, want 0", commentCount)
	}

	var agentContent, humanContent sql.NullString
	if err := database.QueryRow(
		`SELECT agent_content, human_content FROM run_memory WHERE run_id = ?`, runID,
	).Scan(&agentContent, &humanContent); err != nil {
		t.Fatalf("scan run_memory: %v", err)
	}
	if !agentContent.Valid || agentContent.String != "agent self-report" {
		t.Errorf("agent_content = %v, want preserved %q", agentContent, "agent self-report")
	}
	if !humanContent.Valid || !strings.Contains(humanContent.String, wantHumanContentMarker) {
		t.Errorf("human_content missing %q marker; got %q", wantHumanContentMarker, humanContent.String)
	}
	// Stored value MUST NOT carry the "## Human feedback (post-run)"
	// heading — materialization owns that, and a writer that bakes it
	// into the stored body double-heads the agent-readable file. The
	// canonical heading should appear only after materializeMemory
	// joins agent_content + human_content via humanFeedbackSeparator.
	if humanContent.Valid && strings.Contains(humanContent.String, "## Human feedback (post-run)") {
		t.Errorf("stored human_content includes the canonical heading; materialization layer should own it: %q", humanContent.String)
	}

	// Read-side check: GetRunMemory's materialization must produce
	// the heading exactly once, anchoring the boundary the next
	// agent's prompt parser scans for.
	mem, err := db.GetRunMemory(database, runID)
	if err != nil {
		t.Fatalf("GetRunMemory: %v", err)
	}
	if mem == nil {
		t.Fatalf("GetRunMemory returned nil after cleanup")
	}
	headingCount := strings.Count(mem.Content, "## Human feedback (post-run)")
	if headingCount != 1 {
		t.Errorf("materialized content has %d occurrences of canonical heading; want exactly 1\n--- materialized ---\n%s",
			headingCount, mem.Content)
	}
}

// TestHandleUndo_CleansUpPendingApprovalRun is the SKY-206 regression
// for the swipe-toast UX path: Cards user dismissed/claimed the
// task, agent ran and reached pending_approval, user hits Cmd-Z (or
// the toast's Undo button). The full cleanup must run AND a swipe
// audit row should be recorded since this is a swipe undo.
func TestHandleUndo_CleansUpPendingApprovalRun(t *testing.T) {
	s := newTestServer(t)
	taskID, runID, reviewID := pendingApprovalFixture(t, s.db)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/undo", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	assertPendingApprovalCleanedUp(t, s.db, taskID, runID, reviewID,
		"queued", "returned to the triage queue")

	// /undo must record an 'undo' swipe_events row — that's the
	// audit signal for swipe-card analytics that distinguishes it
	// from /requeue.
	var undoCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM swipe_events WHERE task_id = ? AND action = 'undo'`, taskID,
	).Scan(&undoCount); err != nil {
		t.Fatalf("scan swipe_events: %v", err)
	}
	if undoCount != 1 {
		t.Errorf("undo swipe_events count = %d, want 1", undoCount)
	}
}

// TestHandleRequeue_CleansUpPendingApprovalRun is the parallel for
// the state-driven path: Board's drag-to-Queue, SKY-207's "Return
// to queue" button. Same cleanup, but NO swipe row — drag/click
// gestures aren't swipes and shouldn't muddy the swipe analytics.
func TestHandleRequeue_CleansUpPendingApprovalRun(t *testing.T) {
	s := newTestServer(t)
	taskID, runID, reviewID := pendingApprovalFixture(t, s.db)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/requeue", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	assertPendingApprovalCleanedUp(t, s.db, taskID, runID, reviewID,
		"queued", "returned to the triage queue")

	// /requeue must NOT record a swipe_events row — this is a
	// deliberate state change, not a swipe undo. Recording it
	// would inflate the swipe-undo rate analytics every time the
	// user drags a card to the Queue column.
	var swipeCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM swipe_events WHERE task_id = ?`, taskID,
	).Scan(&swipeCount); err != nil {
		t.Fatalf("scan swipe_events: %v", err)
	}
	if swipeCount != 0 {
		t.Errorf("/requeue should not record swipe_events; got %d rows", swipeCount)
	}
}

// TestHandleSwipe_DismissCleansUpPendingApprovalRun is the third
// entry point: user swipes left to dismiss a delegated card whose
// agent already produced a pending_approval review. Today this
// orphans the review and leaves the run as a phantom
// pending_approval against a dismissed task — SKY-206's other half.
//
// The dismiss-flavored human_content note carries a different
// implication marker ("dismissed the task entirely") than the
// requeue paths so a future agent reading prior memory can
// distinguish "the human shelved this verdict but kept the entity
// on the docket" from "the human walked away from this entity".
func TestHandleSwipe_DismissCleansUpPendingApprovalRun(t *testing.T) {
	s := newTestServer(t)
	taskID, runID, reviewID := pendingApprovalFixture(t, s.db)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/swipe",
		map[string]any{"action": "dismiss", "hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	assertPendingApprovalCleanedUp(t, s.db, taskID, runID, reviewID,
		"dismissed", "dismissed the task entirely")
}

// TestHandleSwipe_CompleteCleansUpPendingApprovalRun is the fourth
// entry point: the Board's drag-AgentCard-to-Done gesture for a
// pending_approval run. The complete swipe action flips the task to
// 'done' (so the card lands in the Done column rather than
// disappearing from the board, the way dismiss makes it) but reuses
// the same SKY-206 cleanup — pending_reviews row gone, run flipped
// to cancelled, agent_content preserved, human_content recording
// the user's verdict with a complete-flavored marker that's distinct
// from both the requeue and dismiss shapes. Future agents reading
// memory should be able to tell "the human resolved this themselves
// without applying my prepared review" from "the human walked away
// from the entity entirely."
func TestHandleSwipe_CompleteCleansUpPendingApprovalRun(t *testing.T) {
	s := newTestServer(t)
	taskID, runID, reviewID := pendingApprovalFixture(t, s.db)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/swipe",
		map[string]any{"action": "complete", "hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	assertPendingApprovalCleanedUp(t, s.db, taskID, runID, reviewID,
		"done", "marked the task complete without submitting")
}

// TestHandleSwipe_ClaimCleansUpPendingApprovalRun guards the SKY-206
// race the PR #77 review flagged: Board's drag-Agent-to-You issues
// /swipe claim, but the frontend's agentRuns map can be transiently
// empty during a fetchTasks refresh — so any frontend gating on
// agentRuns[taskId]?.Status === 'pending_approval' would silently
// skip the cleanup, stranding the prepared review row and leaving a
// phantom pending_approval run.
//
// Backend-authoritative cleanup closes that hole: the swipe handler
// runs cleanupPendingApprovalRun for every claim, and the
// pending_approval-row lookup makes it a no-op for tasks without a
// review. The claim-flavored marker carries its own
// recalibration signal — "human took over manually" — distinct from
// requeue/dismiss/complete.
func TestHandleSwipe_ClaimCleansUpPendingApprovalRun(t *testing.T) {
	s := newTestServer(t)
	taskID, runID, reviewID := pendingApprovalFixture(t, s.db)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/swipe",
		map[string]any{"action": "claim", "hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// SKY-261 B+: claim no longer transitions status; the task stays
	// 'queued' and claimed_by_user_id is set instead. The
	// pending-approval cleanup invariants (run cancelled, review row
	// removed, human_content marker) are unchanged.
	assertPendingApprovalCleanedUp(t, s.db, taskID, runID, reviewID,
		"queued", "claimed the task to handle it themselves")
	// Pin the claim col too — it's the actual responsibility signal
	// post-B+.
	var claimedByUserID sql.NullString
	if err := s.db.QueryRow(
		`SELECT claimed_by_user_id FROM tasks WHERE id = ?`, taskID,
	).Scan(&claimedByUserID); err != nil {
		t.Fatalf("scan claim: %v", err)
	}
	if !claimedByUserID.Valid || claimedByUserID.String == "" {
		t.Errorf("task.claimed_by_user_id empty after claim swipe; want stamped")
	}
}

// TestHandleSwipe_ClaimWithoutPendingApprovalIsNoOp pins the
// idempotency contract: cleanupPendingApprovalRun must be a no-op
// when the task has no pending_approval run, so adding the cleanup
// to the claim path doesn't disturb the queue → claim flow used by
// Cards.tsx and the existing Board queue → you drag.
func TestHandleSwipe_ClaimWithoutPendingApprovalIsNoOp(t *testing.T) {
	s := newTestServer(t)

	// Plain queued task with no agent run. Mirrors what claim from
	// the queue looks like — the event/task FK chain mirrors
	// pendingApprovalFixture but stops short of any runs or reviews.
	const eventType = "github:pr:opened"
	if _, err := s.db.Exec(`
		INSERT INTO entities (id, source, source_id, kind, state)
		VALUES ('e1', 'github', 'sky/repo#1', 'pr', 'active');
		INSERT INTO events (id, entity_id, event_type, dedup_key)
		VALUES ('ev1', 'e1', ?, '');
		INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		VALUES ('task-noruns', 'e1', ?, 'ev1', 'queued');
	`, eventType, eventType); err != nil {
		t.Fatalf("seed FK chain: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/task-noruns/swipe",
		map[string]any{"action": "claim", "hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// SKY-261 B+: claim is a responsibility-axis action; status stays
	// 'queued', claim col gets stamped.
	var status string
	var claimedByUserID sql.NullString
	if err := s.db.QueryRow(
		`SELECT status, claimed_by_user_id FROM tasks WHERE id = 'task-noruns'`,
	).Scan(&status, &claimedByUserID); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if status != "queued" {
		t.Errorf("task.status = %q, want %q (claim no longer changes status)", status, "queued")
	}
	if !claimedByUserID.Valid || claimedByUserID.String == "" {
		t.Errorf("task.claimed_by_user_id empty after claim swipe; want stamped")
	}
}

// TestHandleUndo_404OnMissingTask pins the missing-id behavior:
// /undo against a bogus task ID must return 404 with a clean error
// body, not the SQLite FK violation surfaced as a 500. The
// GetTask-first check in the handler fails fast before
// UndoLastSwipe's INSERT into swipe_events trips the FK constraint
// — so legitimate 404 callers don't have to parse SQLite error
// strings to tell "doesn't exist" from "real server error."
func TestHandleUndo_404OnMissingTask(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/no-such-task/undo", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleRequeue_404OnMissingTask is the regression for the
// silent-success bug: /requeue used to return 200 against a bogus
// id because the underlying UPDATE just affected 0 rows. Both the
// handler-level GetTask check and RequeueTask's ok-bool now catch
// it — the test goes through the handler so both layers are
// exercised together.
func TestHandleRequeue_404OnMissingTask(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/no-such-task/requeue", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestRequeueTask_OkFalseOnMissingID directly exercises the DB
// helper's ok-bool. The handler check above catches the common
// case, but the bool is the defense against a race between
// GetTask and the UPDATE (task deleted in the gap). Without this
// signal, that race would silently 200 even with the handler
// check in place.
func TestRequeueTask_OkFalseOnMissingID(t *testing.T) {
	s := newTestServer(t)

	ok, err := s.swipes.RequeueTask(t.Context(), runmode.LocalDefaultOrg, "no-such-task")
	if err != nil {
		t.Fatalf("RequeueTask: %v", err)
	}
	if ok {
		t.Errorf("RequeueTask returned ok=true for missing id; want false")
	}
}

// TestHandleUndo_NoPendingApprovalIsNoOp guards the common case:
// the task has no delegated run (or its delegated run is still
// active, not pending_approval). The cleanup should silently
// no-op rather than touching unrelated runs/reviews.
func TestHandleUndo_NoPendingApprovalIsNoOp(t *testing.T) {
	s := newTestServer(t)

	// Seed a plain claimed task with no run at all — the simplest
	// shape that exercises handleUndo's other half (status flip +
	// Jira reversal skipped because EntitySource isn't 'jira').
	if _, err := s.db.Exec(`
		INSERT INTO entities (id, source, source_id, kind, state)
		VALUES ('e_plain', 'github', 'owner/repo#plain', 'pr', 'active');
		INSERT INTO events (id, entity_id, event_type, dedup_key)
		VALUES ('ev_plain', 'e_plain', 'github:pr:opened', '');
		INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		VALUES ('t_plain', 'e_plain', 'github:pr:opened', 'ev_plain', 'claimed');
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/t_plain/undo", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var taskStatus string
	if err := s.db.QueryRow(`SELECT status FROM tasks WHERE id = ?`, "t_plain").Scan(&taskStatus); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if taskStatus != "queued" {
		t.Errorf("task.status = %q, want %q", taskStatus, "queued")
	}
}

// TestHandleUndo_ClearsClaimColumns pins the SKY-261 B+ semantic:
// /undo returns the task to the team's unclaimed queue, which means
// both claim_by_* cols are cleared — not just status reset. Without
// this, a claim/delegate swipe followed by Undo would leave the task
// status='queued' but still in the owner's lane (queue-view filter
// requires both claim cols NULL), so the user would think they
// undid the action while the Board kept rendering the task as
// claimed.
func TestHandleUndo_ClearsClaimColumns(t *testing.T) {
	s := newTestServer(t)

	// Seed a task carrying both a status that /undo will revert and
	// a user claim (simulates the post-swipe state for action='claim').
	// snooze_until is also set so the regression covers the "claim
	// during a snoozed window" path — both cols + snooze_until must
	// reset.
	if _, err := s.db.Exec(`
		INSERT INTO entities (id, source, source_id, kind, state)
		VALUES ('e_undo_claim', 'github', 'owner/repo#u1', 'pr', 'active');
		INSERT INTO events (id, entity_id, event_type, dedup_key)
		VALUES ('ev_undo_claim', 'e_undo_claim', 'github:pr:opened', '');
		INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_user_id, snooze_until)
		VALUES ('t_undo_claim', 'e_undo_claim', 'github:pr:opened', 'ev_undo_claim', 'snoozed', ?, '2099-01-01 00:00:00');
	`, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/t_undo_claim/undo", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var status, claimedAgent, claimedUser sql.NullString
	var snoozeUntil sql.NullTime
	if err := s.db.QueryRow(
		`SELECT status, claimed_by_agent_id, claimed_by_user_id, snooze_until
		 FROM tasks WHERE id = ?`, "t_undo_claim",
	).Scan(&status, &claimedAgent, &claimedUser, &snoozeUntil); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if status.String != "queued" {
		t.Errorf("status = %q, want %q", status.String, "queued")
	}
	if claimedAgent.Valid {
		t.Errorf("claimed_by_agent_id = %q; want NULL (undo must clear claim)", claimedAgent.String)
	}
	if claimedUser.Valid {
		t.Errorf("claimed_by_user_id = %q; want NULL (undo must clear claim)", claimedUser.String)
	}
	if snoozeUntil.Valid {
		t.Errorf("snooze_until = %v; want NULL", snoozeUntil.Time)
	}
}

// TestCleanupPendingApprovalRun_Idempotent calls the cleanup twice
// against the same task with a different outcome the second time.
// The second call must find the run already cancelled (the
// PendingApprovalRunIDForTask filter returns "" once status flips
// off pending_approval) and exit silently — otherwise:
//
//   - human_content would be overwritten with the second outcome's
//     text, erasing the first verdict from memory
//   - the websocket would double-fire agent_run_update
//   - the audit row's stop_reason / completed_at would shift
//
// We pick discardOutcomeDismissed for the second call so that if
// the early-out broke, the human_content marker would visibly
// flip from "returned to the triage queue" to "dismissed the task
// entirely" — making the test failure mode loud rather than silent.
func TestCleanupPendingApprovalRun_Idempotent(t *testing.T) {
	s := newTestServer(t)
	taskID, runID, _ := pendingApprovalFixture(t, s.db)

	s.cleanupPendingApprovalRun(taskID, discardOutcomeRequeued)

	var humanContentBefore sql.NullString
	var completedAtBefore sql.NullTime
	if err := s.db.QueryRow(
		`SELECT rm.human_content, r.completed_at
		 FROM run_memory rm JOIN runs r ON r.id = rm.run_id
		 WHERE rm.run_id = ?`, runID,
	).Scan(&humanContentBefore, &completedAtBefore); err != nil {
		t.Fatalf("scan after first call: %v", err)
	}
	if !strings.Contains(humanContentBefore.String, "returned to the triage queue") {
		t.Fatalf("first call didn't write requeue marker; got %q", humanContentBefore.String)
	}

	// Second call: different outcome, must not take effect.
	s.cleanupPendingApprovalRun(taskID, discardOutcomeDismissed)

	var humanContentAfter sql.NullString
	var completedAtAfter sql.NullTime
	var runStatusAfter string
	if err := s.db.QueryRow(
		`SELECT rm.human_content, r.completed_at, r.status
		 FROM run_memory rm JOIN runs r ON r.id = rm.run_id
		 WHERE rm.run_id = ?`, runID,
	).Scan(&humanContentAfter, &completedAtAfter, &runStatusAfter); err != nil {
		t.Fatalf("scan after second call: %v", err)
	}
	if runStatusAfter != "cancelled" {
		t.Errorf("run.status drifted after second call: %q", runStatusAfter)
	}
	if humanContentAfter.String != humanContentBefore.String {
		t.Errorf("human_content overwritten by second call:\n  before: %q\n  after:  %q",
			humanContentBefore.String, humanContentAfter.String)
	}
	if !completedAtBefore.Valid || !completedAtAfter.Valid ||
		!completedAtAfter.Time.Equal(completedAtBefore.Time) {
		t.Errorf("completed_at shifted on idempotent re-call: before=%v after=%v",
			completedAtBefore, completedAtAfter)
	}
}

// TestCleanupPendingApprovalRun_DeleteFailureHoldsRunForRetry is
// the regression for the ordering bug: if DeletePendingReviewByRunID
// fails transiently, the cleanup must NOT flip the run off
// status='pending_approval'. PendingApprovalRunIDForTask filters on
// that status, so cancelling the run too eagerly would strand the
// review with no path back to retry.
//
// We force a delete failure by temporarily renaming the
// pending_review_comments table — the DELETE inside the
// transactional helper sees a missing dependency and rolls back. After
// restoring the table, a second cleanup call must find the run
// still in pending_approval, succeed, and leave the expected
// terminal state.
func TestCleanupPendingApprovalRun_DeleteFailureHoldsRunForRetry(t *testing.T) {
	s := newTestServer(t)
	taskID, runID, _ := pendingApprovalFixture(t, s.db)

	// Sabotage: rename the comments table so the DELETE inside
	// DeletePendingReviewByRunID fails. SQLite's foreign-key
	// validation isn't what trips here — the transactional helper's
	// first DELETE references pending_review_comments by name and
	// fails with "no such table".
	if _, err := s.db.Exec(`ALTER TABLE pending_review_comments RENAME TO pending_review_comments_temp`); err != nil {
		t.Fatalf("rename comments table: %v", err)
	}

	s.cleanupPendingApprovalRun(taskID, discardOutcomeRequeued)

	// Run must still be pending_approval — the delete failed and
	// MarkAgentRunDiscarded must have been skipped.
	var runStatus string
	if err := s.db.QueryRow(`SELECT status FROM runs WHERE id = ?`, runID).Scan(&runStatus); err != nil {
		t.Fatalf("scan run after sabotaged cleanup: %v", err)
	}
	if runStatus != "pending_approval" {
		t.Fatalf("run.status = %q after delete failure; want %q (cleanup must hold for retry)",
			runStatus, "pending_approval")
	}

	// Heal the table; the next cleanup call must rediscover the
	// run via PendingApprovalRunIDForTask and complete the work.
	if _, err := s.db.Exec(`ALTER TABLE pending_review_comments_temp RENAME TO pending_review_comments`); err != nil {
		t.Fatalf("restore comments table: %v", err)
	}

	s.cleanupPendingApprovalRun(taskID, discardOutcomeRequeued)

	if err := s.db.QueryRow(`SELECT status FROM runs WHERE id = ?`, runID).Scan(&runStatus); err != nil {
		t.Fatalf("scan run after retry: %v", err)
	}
	if runStatus != "cancelled" {
		t.Errorf("run.status = %q after successful retry; want %q", runStatus, "cancelled")
	}
	var revCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pending_reviews WHERE run_id = ?`, runID).Scan(&revCount); err != nil {
		t.Fatalf("scan pending_reviews after retry: %v", err)
	}
	if revCount != 0 {
		t.Errorf("pending_reviews count = %d after retry; want 0 (review should be torn down)", revCount)
	}
}

// TestCleanupPendingApprovalRun_AgentContentNullSurvives is the
// SKY-204 synthetic-row case: agent skipped the memory file, so
// run_memory exists with agent_content NULL. The discard cleanup
// still lands human_content cleanly on the existing row (the spec's
// guarantee that the unconditional termination-time upsert means
// no INSERT-or-UPDATE branching is needed downstream).
func TestCleanupPendingApprovalRun_AgentContentNullSurvives(t *testing.T) {
	s := newTestServer(t)
	taskID, runID, _ := pendingApprovalFixture(t, s.db)

	// Force agent_content NULL to simulate a noncompliant gate
	// (SKY-204's UpsertAgentMemory("") would have done this in
	// production; we set it directly to skip the dependency).
	if _, err := s.db.Exec(
		`UPDATE run_memory SET agent_content = NULL WHERE run_id = ?`, runID,
	); err != nil {
		t.Fatalf("force null agent_content: %v", err)
	}

	s.cleanupPendingApprovalRun(taskID, discardOutcomeRequeued)

	var agentContent, humanContent sql.NullString
	if err := s.db.QueryRow(
		`SELECT agent_content, human_content FROM run_memory WHERE run_id = ?`, runID,
	).Scan(&agentContent, &humanContent); err != nil {
		t.Fatalf("scan run_memory: %v", err)
	}
	if agentContent.Valid {
		t.Errorf("agent_content was NULL pre-cleanup; should still be NULL post-cleanup, got %q", agentContent.String)
	}
	if !humanContent.Valid || !strings.Contains(humanContent.String, "Human discarded") {
		t.Errorf("human_content not landed against NULL agent_content row: %v", humanContent)
	}
}
