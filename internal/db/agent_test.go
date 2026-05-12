package db

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestActiveRunIDsForTask verifies the terminal-state filter matches the one
// used by HasActiveRunForTask — the close cascade depends on this query
// returning exactly the runs that should be cancelled when a task closes.
func TestActiveRunIDsForTask(t *testing.T) {
	database := newTestDB(t)

	entity, _, err := FindOrCreateEntity(database, "github", "owner/repo#1", "pr", "Test", "https://example.com/1")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	eventID, err := RecordEvent(database, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := FindOrCreateTask(database, entity.ID, domain.EventGitHubPRCICheckFailed, "build", eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	createPromptForTest(t, database, domain.Prompt{ID: "test-prompt", Name: "T", Body: "x", Source: "user"})

	// Seed runs in a mix of states. Non-terminal ones should appear in the
	// returned list; terminal ones (including pending_approval, which is
	// "terminal for the purposes of this gate", and taken_over which was
	// added with the takeover feature) must not.
	runs := []struct {
		id     string
		status string
		active bool
	}{
		{"run-init", "initializing", true},
		{"run-cloning", "cloning", true},
		{"run-running", "running", true},
		{"run-completed", "completed", false},
		{"run-failed", "failed", false},
		{"run-cancelled", "cancelled", false},
		{"run-unsolvable", "task_unsolvable", false},
		{"run-pending", "pending_approval", false},
		{"run-taken-over", "taken_over", false},
	}
	for _, r := range runs {
		if err := CreateAgentRun(database, domain.AgentRun{
			ID:       r.id,
			TaskID:   task.ID,
			PromptID: "test-prompt",
			Status:   r.status,
			Model:    "claude-sonnet-4-6",
		}); err != nil {
			t.Fatalf("create run %s: %v", r.id, err)
		}
		if r.status != "initializing" {
			if _, err := database.Exec(`UPDATE runs SET status = ? WHERE id = ?`, r.status, r.id); err != nil {
				t.Fatalf("set run %s status: %v", r.id, err)
			}
		}
	}

	ids, err := ActiveRunIDsForTask(database, task.ID)
	if err != nil {
		t.Fatalf("ActiveRunIDsForTask: %v", err)
	}

	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	for _, r := range runs {
		if r.active && !got[r.id] {
			t.Errorf("expected active run %s (status=%s) in result, missing", r.id, r.status)
		}
		if !r.active && got[r.id] {
			t.Errorf("unexpected terminal run %s (status=%s) in result", r.id, r.status)
		}
	}
}

// TestActiveRunIDsForTask_Empty returns nil (not error) when the task has
// no runs at all.
func TestActiveRunIDsForTask_Empty(t *testing.T) {
	database := newTestDB(t)
	ids, err := ActiveRunIDsForTask(database, "no-such-task")
	if err != nil {
		t.Fatalf("ActiveRunIDsForTask on missing task: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 ids, got %d", len(ids))
	}
}

// takeoverFixture spins up an entity, event, task, prompt, and run all
// pointing at one another so tests can exercise the takeover DB helpers
// without re-doing the FK boilerplate. The run is created with the
// requested initial status (force-set after CreateAgentRun's "cloning"
// default) and worktree_path so race-loss + worktree_path-update assertions
// have something to compare against.
//
// Each call uses a freshly suffixed entity/task ID so the same test file
// can call this multiple times against the same DB without colliding on
// the entity dedup key.
func takeoverFixture(t *testing.T, database *sql.DB, runID, status, worktreePath string) (taskID string) {
	t.Helper()

	entitySource := "owner/repo#" + runID
	entity, _, err := FindOrCreateEntity(database, "github", entitySource, "pr", "Test "+runID, "https://example.com/"+runID)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	eventID, err := RecordEvent(database, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := FindOrCreateTask(database, entity.ID, domain.EventGitHubPRCICheckFailed, runID, eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	// Use FindOrCreate semantics for the prompt — multiple fixtures in
	// one test would otherwise collide on the unique ID.
	if existing := getPromptForTest(t, database, "test-prompt"); existing == nil {
		createPromptForTest(t, database, domain.Prompt{ID: "test-prompt", Name: "T", Body: "x", Source: "user"})
	}
	if err := CreateAgentRun(database, domain.AgentRun{
		ID:           runID,
		TaskID:       task.ID,
		PromptID:     "test-prompt",
		Status:       "initializing",
		Model:        "claude-sonnet-4-6",
		WorktreePath: worktreePath,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if status != "initializing" {
		if _, err := database.Exec(`UPDATE runs SET status = ? WHERE id = ?`, status, runID); err != nil {
			t.Fatalf("set status: %v", err)
		}
	}
	if worktreePath != "" {
		if _, err := database.Exec(`UPDATE runs SET worktree_path = ? WHERE id = ?`, worktreePath, runID); err != nil {
			t.Fatalf("set worktree_path: %v", err)
		}
	}
	return task.ID
}

// TestMarkAgentRunTakenOver_AtomicWithClaim pins the SKY-261 B+
// atomicity invariant: when claimUserID is supplied, the run flip and
// the task claim flip both happen in one transaction. The contract:
//
//   - Successful call: run.status='taken_over' AND task is now
//     user-claimed (agent claim cleared). Single observable moment.
//   - Race-lost (run already terminal): tx rolls back; BOTH the run
//     AND the task are unchanged. The bot's claim survives the
//     attempted takeover.
//
// Pre-B+ implementations did two separate UPDATEs with the task
// UPDATE allowed to fail — leaving the system in an incoherent state
// where the run reads "taken over" but the task still shows bot
// claim. This test catches that regression by exercising both arms.
func TestMarkAgentRunTakenOver_AtomicWithClaim(t *testing.T) {
	database := newTestDB(t)
	taskID := takeoverFixture(t, database, "run-atomic-takeover", "running", "/tmp/triagefactory-runs/run-atomic-takeover")

	// Pre-condition: bot claims the task (via the local agent
	// sentinel). seedLocalAgentForClaimTests is in tasks_claims_test.go,
	// but inlining the INSERT here keeps this test self-contained.
	if _, err := database.Exec(
		`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if err := SetTaskClaimedByAgent(database, taskID, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("seed agent claim: %v", err)
	}

	const userID = runmode.LocalDefaultUserID
	dest := "/home/user/.triagefactory/takeovers/run-atomic"
	ok, err := MarkAgentRunTakenOver(database, "run-atomic-takeover", dest, userID)
	if err != nil {
		t.Fatalf("MarkAgentRunTakenOver: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true for active run")
	}

	// Both writes must have landed: run.status='taken_over' AND
	// task.claimed_by_user_id=userID with the agent claim cleared.
	run, err := GetAgentRun(database, "run-atomic-takeover")
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	if run.Status != "taken_over" {
		t.Errorf("run.Status=%q want taken_over", run.Status)
	}
	gotTask, err := GetTask(database, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if gotTask.ClaimedByAgentID != "" {
		t.Errorf("task.ClaimedByAgentID=%q want empty (atomic flip should have cleared it)", gotTask.ClaimedByAgentID)
	}
	if gotTask.ClaimedByUserID != userID {
		t.Errorf("task.ClaimedByUserID=%q want %q (atomic flip should have set it)", gotTask.ClaimedByUserID, userID)
	}
}

// TestMarkAgentRunTakenOver_AtomicRollsBackOnRaceLoss pins the
// rollback side of the atomicity contract: when the run is already
// terminal (race-lost), the transaction must roll back. The task's
// claim must NOT have been flipped — a fast-completing run shouldn't
// trigger a claim transfer.
func TestMarkAgentRunTakenOver_AtomicRollsBackOnRaceLoss(t *testing.T) {
	database := newTestDB(t)
	taskID := takeoverFixture(t, database, "run-race-loss", "completed", "/tmp/triagefactory-runs/run-race-loss")

	if _, err := database.Exec(
		`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if err := SetTaskClaimedByAgent(database, taskID, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("seed agent claim: %v", err)
	}

	ok, err := MarkAgentRunTakenOver(database, "run-race-loss", "/dest", runmode.LocalDefaultUserID)
	if err != nil {
		t.Fatalf("MarkAgentRunTakenOver: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false on race-loss (run already completed)")
	}

	gotTask, err := GetTask(database, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if gotTask.ClaimedByAgentID != runmode.LocalDefaultAgentID {
		t.Errorf("task.ClaimedByAgentID=%q want %q (race-loss must roll back the claim flip too)",
			gotTask.ClaimedByAgentID, runmode.LocalDefaultAgentID)
	}
	if gotTask.ClaimedByUserID != "" {
		t.Errorf("task.ClaimedByUserID=%q want empty (race-loss rolled back, no user claim)", gotTask.ClaimedByUserID)
	}
}

// TestMarkAgentRunTakenOver_Active is the happy-path: an active run gets
// marked taken_over with the right metadata and worktree_path is updated
// to the takeover destination (so the row no longer points at the soon-
// to-be-deleted /tmp worktree).
func TestMarkAgentRunTakenOver_Active(t *testing.T) {
	database := newTestDB(t)
	takeoverFixture(t, database, "run-takeover-active", "running", "/tmp/triagefactory-runs/run-takeover-active")

	dest := "/home/user/.triagefactory/takeovers/run-run-takeover-active"
	ok, err := MarkAgentRunTakenOver(database, "run-takeover-active", dest, "")
	if err != nil {
		t.Fatalf("MarkAgentRunTakenOver: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true for active run")
	}

	got, err := GetAgentRun(database, "run-takeover-active")
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	if got.Status != "taken_over" {
		t.Errorf("Status = %q, want taken_over", got.Status)
	}
	if got.WorktreePath != dest {
		t.Errorf("WorktreePath = %q, want %q", got.WorktreePath, dest)
	}
	if got.StopReason != "user_takeover" {
		t.Errorf("StopReason = %q, want user_takeover", got.StopReason)
	}
	if got.ResultSummary == "" || !contains(got.ResultSummary, dest) {
		t.Errorf("ResultSummary = %q, want it to mention %q", got.ResultSummary, dest)
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt was not set")
	} else if time.Since(*got.CompletedAt) > time.Minute {
		t.Errorf("CompletedAt = %v, expected ~now", got.CompletedAt)
	}
}

// TestMarkAgentRunTakenOver_RaceLoss covers every terminal status: if the
// goroutine wrote a real terminal status before our flag could land, the
// guarded UPDATE no-ops and we get ok=false. The original status (and
// worktree_path) must be preserved so the agent's actual outcome isn't
// clobbered with taken_over.
func TestMarkAgentRunTakenOver_RaceLoss(t *testing.T) {
	cases := []string{
		"completed",
		"failed",
		"cancelled",
		"task_unsolvable",
		"pending_approval",
		"taken_over",
	}
	for _, status := range cases {
		t.Run(status, func(t *testing.T) {
			database := newTestDB(t)
			origPath := "/tmp/triagefactory-runs/run-" + status
			takeoverFixture(t, database, "run-"+status, status, origPath)

			ok, err := MarkAgentRunTakenOver(database, "run-"+status, "/somewhere/new", "")
			if err != nil {
				t.Fatalf("MarkAgentRunTakenOver: %v", err)
			}
			if ok {
				t.Errorf("expected ok=false for terminal status %s", status)
			}

			got, err := GetAgentRun(database, "run-"+status)
			if err != nil {
				t.Fatalf("GetAgentRun: %v", err)
			}
			if got.Status != status {
				t.Errorf("Status changed from %q to %q — race-loss path must preserve original outcome", status, got.Status)
			}
			if got.WorktreePath != origPath {
				t.Errorf("WorktreePath changed from %q to %q — race-loss path must not overwrite", origPath, got.WorktreePath)
			}
		})
	}
}

// TestMarkAgentRunTakenOver_NonexistentRun returns ok=false (no rows) without
// erroring. The takeover handler treats this the same as race-loss.
func TestMarkAgentRunTakenOver_NonexistentRun(t *testing.T) {
	database := newTestDB(t)
	ok, err := MarkAgentRunTakenOver(database, "no-such-run", "/dest", "")
	if err != nil {
		t.Fatalf("MarkAgentRunTakenOver: %v", err)
	}
	if ok {
		t.Error("expected ok=false for nonexistent run")
	}
}

// TestMarkAgentRunCancelledIfActive_Active flips an active run to
// cancelled with the supplied stop_reason — used by abortTakeover to
// recover from copy/DB failures so the row doesn't sit in 'running'.
func TestMarkAgentRunCancelledIfActive_Active(t *testing.T) {
	database := newTestDB(t)
	takeoverFixture(t, database, "run-cancellable", "running", "")

	ok, err := MarkAgentRunCancelledIfActive(database, "run-cancellable", "test_reason", "test summary")
	if err != nil {
		t.Fatalf("MarkAgentRunCancelledIfActive: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true for active run")
	}

	got, err := GetAgentRun(database, "run-cancellable")
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	if got.Status != "cancelled" {
		t.Errorf("Status = %q, want cancelled", got.Status)
	}
	if got.StopReason != "test_reason" {
		t.Errorf("StopReason = %q, want test_reason", got.StopReason)
	}
	if got.ResultSummary != "test summary" {
		t.Errorf("ResultSummary = %q, want test summary", got.ResultSummary)
	}
}

// TestMarkAgentRunCancelledIfActive_AlreadyTerminal must no-op for every
// terminal status — the race-loss leg of abortTakeover relies on this to
// preserve the agent's actual outcome instead of overwriting with
// 'cancelled'.
func TestMarkAgentRunCancelledIfActive_AlreadyTerminal(t *testing.T) {
	cases := []string{
		"completed",
		"failed",
		"cancelled",
		"task_unsolvable",
		"pending_approval",
		"taken_over",
	}
	for _, status := range cases {
		t.Run(status, func(t *testing.T) {
			database := newTestDB(t)
			takeoverFixture(t, database, "run-term-"+status, status, "")

			ok, err := MarkAgentRunCancelledIfActive(database, "run-term-"+status, "should_not_apply", "should not apply")
			if err != nil {
				t.Fatalf("MarkAgentRunCancelledIfActive: %v", err)
			}
			if ok {
				t.Errorf("expected ok=false for terminal status %s", status)
			}

			got, err := GetAgentRun(database, "run-term-"+status)
			if err != nil {
				t.Fatalf("GetAgentRun: %v", err)
			}
			if got.Status != status {
				t.Errorf("Status changed from %q to %q — must preserve terminal status", status, got.Status)
			}
		})
	}
}

// TestListTakenOverRunIDs returns only runs whose status is taken_over
// AND whose worktree_path is still populated. Released takeovers (status
// stays 'taken_over' as audit trail, but worktree_path is cleared) are
// excluded — their dirs are gone, so the startup-cleanup sweep has
// nothing to preserve for them.
func TestListTakenOverRunIDs(t *testing.T) {
	database := newTestDB(t)
	takeoverFixture(t, database, "run-A", "running", "")
	takeoverFixture(t, database, "run-B", "taken_over", "/tmp/wt-B")
	takeoverFixture(t, database, "run-C", "completed", "")
	takeoverFixture(t, database, "run-D", "taken_over", "/tmp/wt-D")
	takeoverFixture(t, database, "run-E-released", "taken_over", "")

	got, err := ListTakenOverRunIDs(database)
	if err != nil {
		t.Fatalf("ListTakenOverRunIDs: %v", err)
	}
	gotSet := map[string]bool{}
	for _, id := range got {
		gotSet[id] = true
	}
	if !gotSet["run-B"] || !gotSet["run-D"] {
		t.Errorf("missing held taken_over runs; got %v", got)
	}
	if gotSet["run-A"] || gotSet["run-C"] {
		t.Errorf("included non-taken_over runs; got %v", got)
	}
	if gotSet["run-E-released"] {
		t.Errorf("released takeover (empty worktree_path) must not appear in preserve set; got %v", got)
	}
}

// TestMarkAgentRunReleased verifies the release flip: status stays
// 'taken_over' for audit, worktree_path is cleared, result_summary
// gets the released marker. Idempotent against double-call.
func TestMarkAgentRunReleased(t *testing.T) {
	database := newTestDB(t)
	takeoverFixture(t, database, "run-rel", "taken_over", "/tmp/wt-rel")

	ok, err := MarkAgentRunReleased(database, "run-rel")
	if err != nil {
		t.Fatalf("MarkAgentRunReleased: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true on first release")
	}

	got, err := GetAgentRun(database, "run-rel")
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	if got.Status != "taken_over" {
		t.Errorf("status: got %q, want taken_over (audit trail)", got.Status)
	}
	if got.WorktreePath != "" {
		t.Errorf("worktree_path: got %q, want empty after release", got.WorktreePath)
	}
	if !strings.Contains(got.ResultSummary, "released by user") {
		t.Errorf("result_summary missing release marker: %q", got.ResultSummary)
	}

	// Idempotent: a second call returns ok=false (no rows match the
	// non-empty-worktree_path guard since we just cleared it).
	ok2, err := MarkAgentRunReleased(database, "run-rel")
	if err != nil {
		t.Fatalf("MarkAgentRunReleased (second): %v", err)
	}
	if ok2 {
		t.Errorf("second release returned ok=true; want false (already released)")
	}
}

// TestMarkAgentRunReleased_RejectsNonHeld confirms the guard: release
// against a run that isn't in 'taken_over' (or is taken_over with empty
// worktree_path) returns ok=false without mutating anything.
func TestMarkAgentRunReleased_RejectsNonHeld(t *testing.T) {
	database := newTestDB(t)
	takeoverFixture(t, database, "run-running", "running", "/tmp/wt-running")
	takeoverFixture(t, database, "run-completed", "completed", "/tmp/wt-completed")
	takeoverFixture(t, database, "run-already-released", "taken_over", "")

	for _, runID := range []string{"run-running", "run-completed", "run-already-released"} {
		ok, err := MarkAgentRunReleased(database, runID)
		if err != nil {
			t.Fatalf("MarkAgentRunReleased(%s): %v", runID, err)
		}
		if ok {
			t.Errorf("release of %s returned ok=true; want false", runID)
		}
	}
}

// TestListTakenOverRunIDs_Empty returns nil (no runs match the filter)
// without erroring. Startup must tolerate this — it's the common case
// after a clean shutdown.
func TestListTakenOverRunIDs_Empty(t *testing.T) {
	database := newTestDB(t)
	got, err := ListTakenOverRunIDs(database)
	if err != nil {
		t.Fatalf("ListTakenOverRunIDs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty result, got %v", got)
	}
}

// TestListTakenOverRunsForResume_NewestFirstAndFilters verifies the
// query the CLI's `triagefactory resume` command relies on: only
// taken_over runs, ordered newest-first by completion time, joined
// to entities for display title + source_id. Skips rows missing
// session_id or worktree_path so a defective historical row can't
// surface a runaway entry the user can't actually resume.
func TestListTakenOverRunsForResume_NewestFirstAndFilters(t *testing.T) {
	database := newTestDB(t)

	// Three taken-over runs with descending completion times, plus
	// one running run that should be excluded entirely.
	takeoverFixture(t, database, "run-old", "taken_over", "/tmp/wt-old")
	takeoverFixture(t, database, "run-mid", "taken_over", "/tmp/wt-mid")
	takeoverFixture(t, database, "run-new", "taken_over", "/tmp/wt-new")
	takeoverFixture(t, database, "run-running", "running", "/tmp/wt-running")

	// Force completed_at so the ORDER BY result is deterministic.
	for i, id := range []string{"run-old", "run-mid", "run-new"} {
		// Spread completions across an hour: old at 60m ago, mid at
		// 30m ago, new at 1m ago.
		offset := time.Duration([]int{60, 30, 1}[i]) * -time.Minute
		if _, err := database.Exec(`UPDATE runs SET completed_at = ? WHERE id = ?`, time.Now().Add(offset), id); err != nil {
			t.Fatalf("set completed_at: %v", err)
		}
		// Set session_id too so the filter doesn't drop them.
		if _, err := database.Exec(`UPDATE runs SET session_id = ? WHERE id = ?`, "sess-"+id, id); err != nil {
			t.Fatalf("set session_id: %v", err)
		}
	}

	got, err := ListTakenOverRunsForResume(database)
	if err != nil {
		t.Fatalf("ListTakenOverRunsForResume: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 results, got %d: %+v", len(got), got)
	}
	// Newest first.
	wantOrder := []string{"run-new", "run-mid", "run-old"}
	for i, want := range wantOrder {
		if got[i].RunID != want {
			t.Errorf("position %d: got %q, want %q", i, got[i].RunID, want)
		}
	}
	// Joined fields populated from entity (we set source_id via the
	// fixture as "owner/repo#run-old" etc., title as "Test run-old").
	for _, r := range got {
		if r.TaskTitle == "" {
			t.Errorf("run %s missing TaskTitle", r.RunID)
		}
		if r.SourceID == "" {
			t.Errorf("run %s missing SourceID", r.RunID)
		}
		if r.SessionID == "" {
			t.Errorf("run %s missing SessionID", r.RunID)
		}
		if r.WorktreePath == "" {
			t.Errorf("run %s missing WorktreePath", r.RunID)
		}
	}
}

// TestListTakenOverRunsForResume_SkipsMissingSessionOrWorktree —
// defensive against historical rows: a takeover marked taken_over
// without a session_id or worktree_path is unresumable, so it
// shouldn't appear in the picker even though its status matches.
func TestListTakenOverRunsForResume_SkipsMissingSessionOrWorktree(t *testing.T) {
	database := newTestDB(t)
	takeoverFixture(t, database, "run-noSession", "taken_over", "/tmp/wt") // session_id stays ""
	takeoverFixture(t, database, "run-noPath", "taken_over", "")           // worktree_path stays ""
	takeoverFixture(t, database, "run-good", "taken_over", "/tmp/wt-good")
	if _, err := database.Exec(`UPDATE runs SET session_id = ? WHERE id = ?`, "sess-good", "run-good"); err != nil {
		t.Fatalf("set session_id: %v", err)
	}

	got, err := ListTakenOverRunsForResume(database)
	if err != nil {
		t.Fatalf("ListTakenOverRunsForResume: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 result (only run-good), got %d: %+v", len(got), got)
	}
	if got[0].RunID != "run-good" {
		t.Errorf("got %q, want run-good", got[0].RunID)
	}
}

// TestInsertAgentMessage_StampsZeroCreatedAt verifies the SKY-213 fix: when
// msg.CreatedAt is zero, InsertAgentMessage stamps it with time.Now().UTC()
// and writes that same value to the DB row, so the WS broadcast and the
// persisted row share an authoritative timestamp without a re-read.
func TestInsertAgentMessage_StampsZeroCreatedAt(t *testing.T) {
	database := newTestDB(t)
	runID := "run-stamp-zero-ts"
	takeoverFixture(t, database, runID, "running", "")

	before := time.Now().UTC()
	msg := &domain.AgentMessage{
		RunID:   runID,
		Role:    "assistant",
		Content: "hello world",
		Subtype: "text",
		// CreatedAt intentionally left zero — the function must stamp it.
	}

	id, err := InsertAgentMessage(database, msg)
	if err != nil {
		t.Fatalf("InsertAgentMessage: %v", err)
	}
	after := time.Now().UTC()

	if msg.CreatedAt.IsZero() {
		t.Fatal("msg.CreatedAt was not stamped — WS broadcast would carry zero time")
	}
	if msg.CreatedAt.Before(before) || msg.CreatedAt.After(after) {
		t.Errorf("msg.CreatedAt = %v, want between %v and %v", msg.CreatedAt, before, after)
	}

	// DB row must carry the same instant as the stamped struct so the WS
	// broadcast and the REST fetch agree without a reconciliation round-trip.
	rows, err := MessagesForRun(database, runID)
	if err != nil {
		t.Fatalf("MessagesForRun: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 message, got %d", len(rows))
	}
	if rows[0].ID != int(id) {
		t.Errorf("ID = %d, want %d", rows[0].ID, id)
	}
	if !rows[0].CreatedAt.Equal(msg.CreatedAt) {
		t.Errorf("DB CreatedAt = %v, struct CreatedAt = %v — WS and DB are out of sync", rows[0].CreatedAt, msg.CreatedAt)
	}
}

// TestInsertAgentMessage_PreservesExplicitCreatedAt confirms that an explicit
// non-zero CreatedAt is written through unchanged so callers that timestamp
// messages themselves (e.g., from a streaming event with its own clock) are
// not silently overwritten.
func TestInsertAgentMessage_PreservesExplicitCreatedAt(t *testing.T) {
	database := newTestDB(t)
	runID := "run-explicit-ts"
	takeoverFixture(t, database, runID, "running", "")

	explicit := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	msg := &domain.AgentMessage{
		RunID:     runID,
		Role:      "assistant",
		Content:   "explicit timestamp",
		Subtype:   "text",
		CreatedAt: explicit,
	}

	if _, err := InsertAgentMessage(database, msg); err != nil {
		t.Fatalf("InsertAgentMessage: %v", err)
	}
	if !msg.CreatedAt.Equal(explicit) {
		t.Errorf("non-zero CreatedAt mutated to %v, want %v", msg.CreatedAt, explicit)
	}

	rows, err := MessagesForRun(database, runID)
	if err != nil {
		t.Fatalf("MessagesForRun: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 message, got %d", len(rows))
	}
	if !rows[0].CreatedAt.Equal(explicit) {
		t.Errorf("DB CreatedAt = %v, want %v", rows[0].CreatedAt, explicit)
	}
}

// contains is a small string-contains helper used by these tests so we
// don't pull strings into the imports for one assertion. Faster to
// inline than to round-trip through strings.Contains.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
