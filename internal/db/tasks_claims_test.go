package db

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedLocalAgentForClaimTests inserts the LocalDefaultAgentID row into
// agents so claim-stamp FKs resolve. Mirrors what bootstrap does at
// real startup; the package-level newTestDB doesn't run bootstrap.
func seedLocalAgentForClaimTests(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT OR IGNORE INTO agents (id, org_id, display_name)
		VALUES (?, ?, 'Test Bot')
	`, runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("seed local agent: %v", err)
	}
}

// TestSetTaskClaimedByAgent_RoundTripsAndClearsUserClaim pins the
// SKY-261 D-Claims claim-mutation invariants:
//
//   - SetTaskClaimedByAgent populates claimed_by_agent_id and clears
//     claimed_by_user_id in the same UPDATE (XOR safety: the row is
//     never temporarily in a state where both are set, even mid-UPDATE,
//     which would violate the tasks_claim_xor CHECK).
//   - SetTaskClaimedByUser does the symmetric flip.
//   - Both transitions are no-throw on a fresh unclaimed row (no
//     pre-condition required).
func TestSetTaskClaimedByAgent_RoundTripsAndClearsUserClaim(t *testing.T) {
	database := newTestDB(t)
	seedLocalAgentForClaimTests(t, database)
	entity, _, err := FindOrCreateEntity(database, "github", "octo/repo#claim-1", "pr", "Claim Test 1", "https://example.com/1")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	eventID, err := RecordEvent(database, domain.Event{
		EventType: domain.EventGitHubPROpened, EntityID: &entity.ID, MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := FindOrCreateTask(database, entity.ID, domain.EventGitHubPROpened, "", eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Start: both claim cols NULL.
	got, err := GetTask(database, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.ClaimedByAgentID != "" || got.ClaimedByUserID != "" {
		t.Errorf("fresh task has stale claim: agent=%q user=%q", got.ClaimedByAgentID, got.ClaimedByUserID)
	}

	// User-claim path first: stamps user, leaves agent NULL.
	if err := SetTaskClaimedByUser(database, task.ID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("set user claim: %v", err)
	}
	got, _ = GetTask(database, task.ID)
	if got.ClaimedByUserID != runmode.LocalDefaultUserID {
		t.Errorf("user claim not stamped: got %q want %q", got.ClaimedByUserID, runmode.LocalDefaultUserID)
	}
	if got.ClaimedByAgentID != "" {
		t.Errorf("agent claim leaked through user-claim path: got %q", got.ClaimedByAgentID)
	}

	// Now stamp agent claim. SetTaskClaimedByAgent must clear the user
	// claim in the same UPDATE so the XOR CHECK never sees both set.
	if err := SetTaskClaimedByAgent(database, task.ID, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("set agent claim: %v", err)
	}
	got, _ = GetTask(database, task.ID)
	if got.ClaimedByAgentID != runmode.LocalDefaultAgentID {
		t.Errorf("agent claim not stamped: got %q want %q", got.ClaimedByAgentID, runmode.LocalDefaultAgentID)
	}
	if got.ClaimedByUserID != "" {
		t.Errorf("user claim survived agent-claim flip: got %q (XOR safety broken)", got.ClaimedByUserID)
	}

	// Flip back to user. Symmetric — agent claim must clear.
	if err := SetTaskClaimedByUser(database, task.ID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("re-set user claim: %v", err)
	}
	got, _ = GetTask(database, task.ID)
	if got.ClaimedByUserID != runmode.LocalDefaultUserID {
		t.Errorf("user claim not re-stamped: got %q", got.ClaimedByUserID)
	}
	if got.ClaimedByAgentID != "" {
		t.Errorf("agent claim survived user re-claim: got %q (XOR safety broken)", got.ClaimedByAgentID)
	}
}

// TestClaimQueuedTaskForUser_GuardsAgainstStealing pins the
// optimistic-claim guard: ClaimQueuedTaskForUser only lands a claim if
// the task is currently unclaimed by anyone. A second caller racing
// against an already-claimed task returns ok=false; the existing claim
// is preserved. This is the safety net for the "user claims a queued
// task" gesture against concurrent claims (two browser tabs, etc.).
func TestClaimQueuedTaskForUser_GuardsAgainstStealing(t *testing.T) {
	database := newTestDB(t)
	seedLocalAgentForClaimTests(t, database)
	entity, _, err := FindOrCreateEntity(database, "github", "octo/repo#claim-2", "pr", "Claim Test 2", "https://example.com/2")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	eventID, err := RecordEvent(database, domain.Event{
		EventType: domain.EventGitHubPROpened, EntityID: &entity.ID, MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := FindOrCreateTask(database, entity.ID, domain.EventGitHubPROpened, "", eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// First claim lands.
	ok, err := ClaimQueuedTaskForUser(database, task.ID, runmode.LocalDefaultUserID)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !ok {
		t.Fatal("first claim returned ok=false on unclaimed task")
	}

	// Second claim (different "user" — simulated via a synthetic id)
	// must fail because the task is already claimed. The existing
	// claim is preserved.
	intruder := "00000000-0000-0000-0000-0000000000ff"
	ok, err = ClaimQueuedTaskForUser(database, task.ID, intruder)
	if err != nil {
		t.Fatalf("intruder claim: %v", err)
	}
	if ok {
		t.Errorf("intruder claim succeeded on already-claimed task — guard broken")
	}

	got, _ := GetTask(database, task.ID)
	if got.ClaimedByUserID != runmode.LocalDefaultUserID {
		t.Errorf("first claim was overwritten: got %q want %q", got.ClaimedByUserID, runmode.LocalDefaultUserID)
	}

	// Third claim attempt against a task already claimed by an agent
	// must also fail (claim_by_agent_id IS NULL is part of the guard).
	if err := SetTaskClaimedByAgent(database, task.ID, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("flip to agent: %v", err)
	}
	ok, err = ClaimQueuedTaskForUser(database, task.ID, runmode.LocalDefaultUserID)
	if err != nil {
		t.Fatalf("post-agent claim: %v", err)
	}
	if ok {
		t.Errorf("user claim succeeded against agent-claimed task — guard broken")
	}
}

// TestClaimQueuedTaskForUser_GuardsStatusQueued pins the status='queued'
// half of the guard: even if both claim cols are NULL (an unclaimed
// task), ClaimQueuedTaskForUser must reject the claim if the task is
// in any non-queued state. Snoozed and terminal states both fall under
// this rule — the function's name promises "queued task," and
// claiming a snoozed/closed task is a surprising state that the
// caller should resolve via Requeue or a different gesture instead.
func TestClaimQueuedTaskForUser_GuardsStatusQueued(t *testing.T) {
	database := newTestDB(t)
	seedLocalAgentForClaimTests(t, database)

	cases := []struct {
		name          string
		setStatus     string // "" to skip (leave 'queued')
		closeReason   string // for done/dismissed; uses CloseTask
		wantOK        bool
		wantClaimUser string // populated user id when claim should land; "" if shouldn't
	}{
		{name: "queued_unclaimed_lands", setStatus: "", wantOK: true, wantClaimUser: runmode.LocalDefaultUserID},
		{name: "snoozed_rejected", setStatus: "snoozed", wantOK: false, wantClaimUser: ""},
		{name: "done_rejected", closeReason: "run_completed", wantOK: false, wantClaimUser: ""},
		{name: "dismissed_rejected", closeReason: "user_dismissed", wantOK: false, wantClaimUser: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Each subtest gets its own task via a unique source_id —
			// the dedup index would otherwise collapse them.
			sourceID := "octo/repo#queue-guard-" + tc.name
			entity, _, err := FindOrCreateEntity(database, "github", sourceID, "pr", "Queue Guard "+tc.name, "https://example.com/"+tc.name)
			if err != nil {
				t.Fatalf("seed entity: %v", err)
			}
			eventID, err := RecordEvent(database, domain.Event{
				EventType: domain.EventGitHubPROpened, EntityID: &entity.ID, MetadataJSON: `{}`,
			})
			if err != nil {
				t.Fatalf("record event: %v", err)
			}
			task, _, err := FindOrCreateTask(database, entity.ID, domain.EventGitHubPROpened, "", eventID, 0.5)
			if err != nil {
				t.Fatalf("create task: %v", err)
			}

			// Move into the target state.
			switch {
			case tc.closeReason != "":
				if err := CloseTask(database, task.ID, tc.closeReason, ""); err != nil {
					t.Fatalf("close task: %v", err)
				}
			case tc.setStatus != "":
				if err := SetTaskStatus(database, task.ID, tc.setStatus); err != nil {
					t.Fatalf("set status: %v", err)
				}
			}

			ok, err := ClaimQueuedTaskForUser(database, task.ID, runmode.LocalDefaultUserID)
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v (status guard mismatch)", ok, tc.wantOK)
			}

			got, _ := GetTask(database, task.ID)
			if got.ClaimedByUserID != tc.wantClaimUser {
				t.Errorf("ClaimedByUserID = %q, want %q", got.ClaimedByUserID, tc.wantClaimUser)
			}
		})
	}
}

// TestTaskClaim_StickyPastClose pins the SKY-261 audit invariant:
// claim columns are NOT cleared when a task closes. status='closed' +
// non-empty claim is the answer to "who was responsible when this
// finished." The runs.actor_agent_id audit pointer is its execution
// sibling — together they tell the full story.
func TestTaskClaim_StickyPastClose(t *testing.T) {
	database := newTestDB(t)
	seedLocalAgentForClaimTests(t, database)
	entity, _, err := FindOrCreateEntity(database, "github", "octo/repo#claim-3", "pr", "Claim Test 3", "https://example.com/3")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	eventID, err := RecordEvent(database, domain.Event{
		EventType: domain.EventGitHubPROpened, EntityID: &entity.ID, MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := FindOrCreateTask(database, entity.ID, domain.EventGitHubPROpened, "", eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Stamp agent claim, then close the task via the standard helper.
	if err := SetTaskClaimedByAgent(database, task.ID, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("stamp agent claim: %v", err)
	}
	if err := CloseTask(database, task.ID, "run_completed", domain.EventGitHubPRMerged); err != nil {
		t.Fatalf("close task: %v", err)
	}

	got, err := GetTask(database, task.ID)
	if err != nil {
		t.Fatalf("get closed task: %v", err)
	}
	if got.Status != "done" {
		t.Errorf("closed task status: got %q want done", got.Status)
	}
	if got.ClaimedByAgentID != runmode.LocalDefaultAgentID {
		t.Errorf("agent claim cleared on close — audit broken: got %q want %q",
			got.ClaimedByAgentID, runmode.LocalDefaultAgentID)
	}
}
