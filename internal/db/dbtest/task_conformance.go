package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TaskStoreFactory is what a per-backend test file hands to
// RunTaskStoreConformance. The factory returns:
//   - the wired TaskStore impl
//   - the orgID to pass to every method (sqlite returns
//     runmode.LocalDefaultOrg, postgres returns a fresh org UUID)
//   - the agentID + userID the backend test has seeded — claim
//     transitions need real FK-resolvable ids (auth.users / agents
//     rows already in place for the harness's chosen org)
//   - a TaskSeeder that creates the underlying entity + event +
//     task row the conformance asserts against. The seeder owns
//     backend-specific schema knowledge (sqlite has no org_id
//     column, postgres demands a creator_user_id, etc.) so the
//     harness stays schema-blind.
type TaskStoreFactory func(t *testing.T) (
	store db.TaskStore,
	orgID, agentID, userID string,
	seed TaskSeeder,
)

// TaskSeeder produces a fresh (entity, event, task) chain for one
// assertion. Returns:
//   - entityID + eventID — usable as predicate inputs and re-use
//     across FindOrCreate dedup tests
//   - taskID — the pre-existing queued task the harness will mutate
//
// Each call must produce a distinct entity (so dedup index doesn't
// collapse independent assertions).
type TaskSeeder func(t *testing.T, suffix string) (entityID, eventID, taskID string)

// RunTaskStoreConformance is the shared assertion suite for any
// db.TaskStore implementation. Backend tests invoke it with their
// factory; both backends run the same subtests.
func RunTaskStoreConformance(t *testing.T, mk TaskStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Get_returns_nil_for_missing_id", func(t *testing.T) {
		s, orgID, _, _, _ := mk(t)
		task, err := s.Get(ctx, orgID, "00000000-0000-0000-0000-000000000bad")
		if err != nil {
			t.Fatalf("Get on missing id: %v", err)
		}
		if task != nil {
			t.Errorf("expected nil task for missing id, got %+v", task)
		}
	})

	t.Run("Get_returns_seeded_task_with_entity_join", func(t *testing.T) {
		s, orgID, _, _, seed := mk(t)
		_, _, taskID := seed(t, "get-happy")
		task, err := s.Get(ctx, orgID, taskID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if task == nil {
			t.Fatal("Get returned nil for seeded task")
		}
		if task.Title == "" {
			t.Error("entity JOIN didn't populate Title")
		}
		if task.EntitySource == "" {
			t.Error("entity JOIN didn't populate EntitySource")
		}
	})

	t.Run("Queued_returns_unclaimed_queued_tasks", func(t *testing.T) {
		s, orgID, _, _, seed := mk(t)
		seed(t, "q1")
		seed(t, "q2")
		out, err := s.Queued(ctx, orgID)
		if err != nil {
			t.Fatalf("Queued: %v", err)
		}
		if len(out) < 2 {
			t.Errorf("Queued returned %d, want >= 2", len(out))
		}
		for _, task := range out {
			if task.Status != "queued" {
				t.Errorf("task %s status=%q, want queued", task.ID, task.Status)
			}
			if task.ClaimedByAgentID != "" || task.ClaimedByUserID != "" {
				t.Errorf("task %s shouldn't appear in queued (has claim)", task.ID)
			}
		}
	})

	t.Run("ByStatus_with_done_excludes_active", func(t *testing.T) {
		s, orgID, _, _, seed := mk(t)
		_, _, taskID := seed(t, "bs-done")
		// Active task should not appear in done list.
		done, err := s.ByStatus(ctx, orgID, "done")
		if err != nil {
			t.Fatalf("ByStatus done: %v", err)
		}
		for _, task := range done {
			if task.ID == taskID {
				t.Errorf("active task %s appeared under ByStatus(done)", taskID)
			}
		}
		// Now close it; should appear under done.
		if err := s.Close(ctx, orgID, taskID, "test", ""); err != nil {
			t.Fatalf("Close: %v", err)
		}
		done, err = s.ByStatus(ctx, orgID, "done")
		if err != nil {
			t.Fatalf("ByStatus done (post-close): %v", err)
		}
		found := false
		for _, task := range done {
			if task.ID == taskID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("closed task %s missing from ByStatus(done)", taskID)
		}
	})

	t.Run("FindOrCreate_idempotent_on_dedup_key", func(t *testing.T) {
		s, orgID, _, _, seed := mk(t)
		entityID, eventID, _ := seed(t, "foc-dedup")
		// Re-call with the seed's eventType+dedupKey would collide on
		// the existing seeded task. Use a different eventType so the
		// first FindOrCreate creates a new row, and a second call with
		// the same args returns it idempotently.
		task1, created1, err := s.FindOrCreate(ctx, orgID, entityID, domain.EventGitHubPRCICheckPassed, "dedup-x", eventID, 0.5)
		if err != nil {
			t.Fatalf("FindOrCreate first call: %v", err)
		}
		if !created1 {
			t.Error("first FindOrCreate should return created=true")
		}
		task2, created2, err := s.FindOrCreate(ctx, orgID, entityID, domain.EventGitHubPRCICheckPassed, "dedup-x", eventID, 0.5)
		if err != nil {
			t.Fatalf("FindOrCreate second call: %v", err)
		}
		if created2 {
			t.Error("second FindOrCreate should return created=false (dedup)")
		}
		if task1.ID != task2.ID {
			t.Errorf("second call should return same task id; got %q vs %q", task1.ID, task2.ID)
		}
	})

	t.Run("Bump_wakes_snoozed_task", func(t *testing.T) {
		s, orgID, _, _, seed := mk(t)
		_, eventID, taskID := seed(t, "bump-wake")
		// Force task into snoozed via SetStatus (sufficient for this
		// invariant — claim cols stay empty, satisfying the
		// "snoozed ↔ unclaimed" invariant trivially).
		if err := s.SetStatus(ctx, orgID, taskID, "snoozed"); err != nil {
			t.Fatalf("SetStatus snoozed: %v", err)
		}
		if err := s.Bump(ctx, orgID, taskID, eventID); err != nil {
			t.Fatalf("Bump: %v", err)
		}
		got, err := s.Get(ctx, orgID, taskID)
		if err != nil || got == nil {
			t.Fatalf("Get post-bump: task=%v err=%v", got, err)
		}
		if got.Status != "queued" {
			t.Errorf("status=%q post-bump, want queued (snooze should clear)", got.Status)
		}
	})

	t.Run("Close_terminates_task", func(t *testing.T) {
		s, orgID, _, _, seed := mk(t)
		_, _, taskID := seed(t, "close")
		if err := s.Close(ctx, orgID, taskID, "test_close", ""); err != nil {
			t.Fatalf("Close: %v", err)
		}
		got, _ := s.Get(ctx, orgID, taskID)
		if got.Status != "done" {
			t.Errorf("status=%q post-close, want done", got.Status)
		}
		if got.ClosedAt == nil {
			t.Error("closed_at not set")
		}
	})

	t.Run("CloseAllForEntity_counts_and_closes", func(t *testing.T) {
		s, orgID, _, _, seed := mk(t)
		entityID, _, taskID := seed(t, "closeall")
		closed, err := s.CloseAllForEntity(ctx, orgID, entityID, "entity_closed")
		if err != nil {
			t.Fatalf("CloseAllForEntity: %v", err)
		}
		if closed < 1 {
			t.Errorf("closed count=%d, want >=1", closed)
		}
		got, _ := s.Get(ctx, orgID, taskID)
		if got.Status != "done" {
			t.Errorf("status=%q post-CloseAllForEntity, want done", got.Status)
		}
	})

	t.Run("FindActiveByEntity_excludes_terminal", func(t *testing.T) {
		s, orgID, _, _, seed := mk(t)
		entityID, _, taskID := seed(t, "fab")
		active, err := s.FindActiveByEntity(ctx, orgID, entityID)
		if err != nil {
			t.Fatalf("FindActiveByEntity: %v", err)
		}
		if len(active) != 1 || active[0].ID != taskID {
			t.Fatalf("active list = %+v, want [%s]", active, taskID)
		}
		_ = s.Close(ctx, orgID, taskID, "test", "")
		active, _ = s.FindActiveByEntity(ctx, orgID, entityID)
		if len(active) != 0 {
			t.Errorf("active list post-close = %d rows, want 0", len(active))
		}
	})

	t.Run("ListActiveRefsForEntities_empty_input", func(t *testing.T) {
		s, orgID, _, _, _ := mk(t)
		refs, err := s.ListActiveRefsForEntities(ctx, orgID, nil)
		if err != nil {
			t.Fatalf("ListActiveRefsForEntities(nil): %v", err)
		}
		if len(refs) != 0 {
			t.Errorf("got %d refs, want 0 on empty input", len(refs))
		}
	})

	t.Run("ListActiveRefsForEntities_filters_terminal", func(t *testing.T) {
		s, orgID, _, _, seed := mk(t)
		entityA, _, taskA := seed(t, "ref-a")
		entityB, _, taskB := seed(t, "ref-b")
		// Close B; only A should remain.
		if err := s.Close(ctx, orgID, taskB, "test", ""); err != nil {
			t.Fatalf("Close: %v", err)
		}
		refs, err := s.ListActiveRefsForEntities(ctx, orgID, []string{entityA, entityB})
		if err != nil {
			t.Fatalf("ListActiveRefsForEntities: %v", err)
		}
		if len(refs) != 1 {
			t.Fatalf("got %d refs, want 1 (only A active)", len(refs))
		}
		if refs[0].ID != taskA {
			t.Errorf("ref ID = %s, want %s", refs[0].ID, taskA)
		}
	})

	t.Run("EntityIDsWithActiveTasks_filters_by_source", func(t *testing.T) {
		s, orgID, _, _, seed := mk(t)
		entityID, _, _ := seed(t, "eida")
		ids, err := s.EntityIDsWithActiveTasks(ctx, orgID, "github")
		if err != nil {
			t.Fatalf("EntityIDsWithActiveTasks: %v", err)
		}
		if _, ok := ids[entityID]; !ok {
			t.Errorf("seeded github entity %s missing from result", entityID)
		}
		// Wrong-source query: should not include the github entity.
		ids, err = s.EntityIDsWithActiveTasks(ctx, orgID, "jira")
		if err != nil {
			t.Fatalf("EntityIDsWithActiveTasks(jira): %v", err)
		}
		if _, ok := ids[entityID]; ok {
			t.Errorf("github entity %s leaked into jira-source query", entityID)
		}
	})

	// --- Claim invariants ---

	t.Run("ClaimQueuedForUser_lands_then_refuses_steal", func(t *testing.T) {
		s, orgID, _, userID, seed := mk(t)
		_, _, taskID := seed(t, "cqfu")
		ok, err := s.ClaimQueuedForUser(ctx, orgID, taskID, userID)
		if err != nil {
			t.Fatalf("first claim: %v", err)
		}
		if !ok {
			t.Fatal("first claim returned ok=false on unclaimed task")
		}
		// Second claim attempt — even by the same user — should now be
		// refused because the task is no longer unclaimed.
		ok, err = s.ClaimQueuedForUser(ctx, orgID, taskID, userID)
		if err != nil {
			t.Fatalf("second claim: %v", err)
		}
		if ok {
			t.Error("second claim returned ok=true on already-claimed task; guard broken")
		}
		// Verify the original claim survived.
		got, _ := s.Get(ctx, orgID, taskID)
		if got.ClaimedByUserID != userID {
			t.Errorf("user claim was overwritten: got %q want %q", got.ClaimedByUserID, userID)
		}
	})

	t.Run("ClaimQueuedForUser_rejects_terminal_task", func(t *testing.T) {
		s, orgID, _, userID, seed := mk(t)
		_, _, taskID := seed(t, "cqfu-term")
		if err := s.Close(ctx, orgID, taskID, "test", ""); err != nil {
			t.Fatalf("Close: %v", err)
		}
		ok, err := s.ClaimQueuedForUser(ctx, orgID, taskID, userID)
		if err != nil {
			t.Fatalf("claim on terminal: %v", err)
		}
		if ok {
			t.Error("ok=true claiming a closed task; status guard broken")
		}
	})

	t.Run("StampAgentClaimIfUnclaimed_lands_then_skips_same_agent", func(t *testing.T) {
		s, orgID, agentID, _, seed := mk(t)
		_, _, taskID := seed(t, "stamp")
		ok, err := s.StampAgentClaimIfUnclaimed(ctx, orgID, taskID, agentID)
		if err != nil {
			t.Fatalf("first stamp: %v", err)
		}
		if !ok {
			t.Fatal("first stamp returned ok=false")
		}
		// Same agent again — should no-op (ok=false).
		ok, err = s.StampAgentClaimIfUnclaimed(ctx, orgID, taskID, agentID)
		if err != nil {
			t.Fatalf("second stamp: %v", err)
		}
		if ok {
			t.Error("second stamp returned ok=true; same-agent no-op guard broken")
		}
	})

	t.Run("StampAgentClaimIfUnclaimed_refuses_terminal", func(t *testing.T) {
		s, orgID, agentID, _, seed := mk(t)
		_, _, taskID := seed(t, "stamp-term")
		if err := s.Close(ctx, orgID, taskID, "test", ""); err != nil {
			t.Fatalf("Close: %v", err)
		}
		ok, err := s.StampAgentClaimIfUnclaimed(ctx, orgID, taskID, agentID)
		if err != nil {
			t.Fatalf("StampAgentClaimIfUnclaimed on terminal: %v", err)
		}
		if ok {
			t.Error("ok=true stamping a terminal task; status guard broken")
		}
	})

	t.Run("HandoffAgentClaim_three_outcomes", func(t *testing.T) {
		s, orgID, agentID, userID, seed := mk(t)
		_, _, taskID := seed(t, "handoff")
		// Unclaimed → bot: HandoffChanged.
		result, err := s.HandoffAgentClaim(ctx, orgID, taskID, agentID, userID)
		if err != nil {
			t.Fatalf("first handoff: %v", err)
		}
		if result != db.HandoffChanged {
			t.Errorf("first handoff result=%v, want HandoffChanged", result)
		}
		// Same-agent already-owns → HandoffNoOp.
		result, err = s.HandoffAgentClaim(ctx, orgID, taskID, agentID, userID)
		if err != nil {
			t.Fatalf("second handoff: %v", err)
		}
		if result != db.HandoffNoOp {
			t.Errorf("second handoff result=%v, want HandoffNoOp", result)
		}
		// Terminal task — HandoffRefused regardless of sticky claim.
		if err := s.Close(ctx, orgID, taskID, "test", ""); err != nil {
			t.Fatalf("Close: %v", err)
		}
		result, err = s.HandoffAgentClaim(ctx, orgID, taskID, agentID, userID)
		if err != nil {
			t.Fatalf("post-terminal handoff: %v", err)
		}
		if result != db.HandoffRefused {
			t.Errorf("post-terminal handoff result=%v, want HandoffRefused (terminal-status precedence)", result)
		}
	})

	t.Run("TakeoverClaimFromAgent_succeeds_on_bot_claim", func(t *testing.T) {
		s, orgID, agentID, userID, seed := mk(t)
		_, _, taskID := seed(t, "takeover")
		// Set up bot claim first.
		if _, err := s.StampAgentClaimIfUnclaimed(ctx, orgID, taskID, agentID); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		ok, err := s.TakeoverClaimFromAgent(ctx, orgID, taskID, userID)
		if err != nil {
			t.Fatalf("Takeover: %v", err)
		}
		if !ok {
			t.Fatal("Takeover returned ok=false on bot-claimed task")
		}
		got, _ := s.Get(ctx, orgID, taskID)
		if got.ClaimedByAgentID != "" {
			t.Errorf("ClaimedByAgentID=%q, want empty after takeover", got.ClaimedByAgentID)
		}
		if got.ClaimedByUserID != userID {
			t.Errorf("ClaimedByUserID=%q, want %q", got.ClaimedByUserID, userID)
		}
	})

	// --- Empty-arg / ctx-cancel quick guards ---

	t.Run("CtxCancellation_fails_fast", func(t *testing.T) {
		s, orgID, _, _, _ := mk(t)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.Queued(cancelled, orgID); err == nil {
			t.Errorf("Queued with cancelled ctx: want error, got nil")
		}
	})

	t.Run("ListActiveRefs_empty_orgID_check_does_not_panic", func(t *testing.T) {
		s, orgID, _, _, _ := mk(t)
		refs, err := s.ListActiveRefsForEntities(ctx, orgID, []string{})
		if err != nil {
			t.Fatalf("empty slice: %v", err)
		}
		if len(refs) != 0 {
			t.Errorf("got %d refs, want 0", len(refs))
		}
	})
}
