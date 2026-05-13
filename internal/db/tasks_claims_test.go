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
	//
	// Seed the synthetic user up front so the test exercises the
	// WHERE guard rather than incidentally failing on the
	// users(id) FK. Without the seed the test still passes
	// today (the WHERE guard returns 0 rows so no FK check fires),
	// but a future WHERE-guard regression would surface as an FK
	// error instead of the proper "ok=true, claim stolen" signal.
	intruder := "00000000-0000-0000-0000-0000000000ff"
	if _, err := database.Exec(
		`INSERT INTO users (id, display_name) VALUES (?, 'Intruder')`,
		intruder,
	); err != nil {
		t.Fatalf("seed intruder user: %v", err)
	}
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

// TestClaimQueuedTaskForUser_GuardsStatusQueued pins the
// active-status half of the guard: ClaimQueuedTaskForUser lands on
// 'queued' OR 'snoozed' (both are active queue states under the
// "snoozed ↔ unclaimed" invariant), and refuses on terminal states
// (done / dismissed) where claiming makes no semantic sense and the
// claim cols are audit-sticky.
//
// Pre-invariant: snoozed was rejected too. Post-invariant: claiming
// a snoozed unclaimed task IS the wake — the helper atomically
// clears snooze_until and flips status='queued' as part of the
// stamp, so the post-state is the canonical "user-claimed, not
// snoozed."
func TestClaimQueuedTaskForUser_GuardsStatusQueued(t *testing.T) {
	database := newTestDB(t)
	seedLocalAgentForClaimTests(t, database)

	cases := []struct {
		name          string
		setStatus     string // "" to skip (leave 'queued')
		closeReason   string // for done/dismissed; uses CloseTask
		wantOK        bool
		wantClaimUser string // populated user id when claim should land; "" if shouldn't
		wantStatus    string // post-claim status; differs from setStatus for snoozed→queued wake
		wantSnoozed   bool   // whether snooze_until should still be set post-claim
	}{
		{name: "queued_unclaimed_lands", setStatus: "", wantOK: true, wantClaimUser: runmode.LocalDefaultUserID, wantStatus: "queued"},
		{name: "snoozed_unclaimed_wakes_and_lands", setStatus: "snoozed", wantOK: true, wantClaimUser: runmode.LocalDefaultUserID, wantStatus: "queued", wantSnoozed: false},
		// CloseTask always sets status='done' regardless of
		// close_reason — there is no separate 'dismissed' branch in
		// that helper. Test the dismissed case via direct
		// SetTaskStatus instead (covered as a separate setStatus row).
		{name: "done_rejected", closeReason: "run_completed", wantOK: false, wantClaimUser: "", wantStatus: "done"},
		{name: "dismissed_rejected", setStatus: "dismissed", wantOK: false, wantClaimUser: "", wantStatus: "dismissed"},
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
			case tc.setStatus == "snoozed":
				// Use a real future snooze_until so the snoozed →
				// queued wake assertion has something to verify.
				if _, err := database.Exec(
					`UPDATE tasks SET status = 'snoozed', snooze_until = '2099-01-01 00:00:00' WHERE id = ?`,
					task.ID,
				); err != nil {
					t.Fatalf("set snoozed: %v", err)
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
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q (claim should wake snoozed → queued)", got.Status, tc.wantStatus)
			}
			snoozeStillSet := got.SnoozeUntil != nil
			if snoozeStillSet != tc.wantSnoozed {
				t.Errorf("snooze_until set = %v, want %v (claim should clear snooze atomically)", snoozeStillSet, tc.wantSnoozed)
			}
		})
	}
}

// TestClaimHelpers_RefuseOnTerminalTask pins the data-layer rule
// that claim transitions are not allowed on terminal (done /
// dismissed) tasks. Sticky claims past close are an audit signal —
// they shouldn't accept new claim mutations on top, and without
// this guard the downstream RecordSwipe's vestigial status='queued'
// write would reopen a closed task as a side effect of recording
// the audit row.
//
// All four claim helpers are exercised against both terminal
// statuses to keep the surface explicit. ClaimQueuedTaskForUser
// already had this guard via its status IN ('queued', 'snoozed')
// clause; the other three are the new additions.
func TestClaimHelpers_RefuseOnTerminalTask(t *testing.T) {
	database := newTestDB(t)
	seedLocalAgentForClaimTests(t, database)

	for _, terminalStatus := range []string{"done", "dismissed"} {
		t.Run(terminalStatus, func(t *testing.T) {
			// Helper: fresh task per subtest, force into the terminal
			// status. Use a direct UPDATE rather than CloseTask so we
			// can pin the 'dismissed' branch independently — CloseTask
			// always writes status='done'.
			mkTerminalTask := func(t *testing.T, suffix string, withBotClaim, withUserClaim bool) string {
				t.Helper()
				sid := "octo/repo#terminal-" + terminalStatus + "-" + suffix
				entity, _, err := FindOrCreateEntity(database, "github", sid, "pr", "Terminal "+suffix, "https://example.com/t-"+suffix)
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
				if withBotClaim {
					if err := SetTaskClaimedByAgent(database, task.ID, runmode.LocalDefaultAgentID); err != nil {
						t.Fatalf("stage bot claim: %v", err)
					}
				}
				if withUserClaim {
					if err := SetTaskClaimedByUser(database, task.ID, runmode.LocalDefaultUserID); err != nil {
						t.Fatalf("stage user claim: %v", err)
					}
				}
				if _, err := database.Exec(
					`UPDATE tasks SET status = ? WHERE id = ?`, terminalStatus, task.ID,
				); err != nil {
					t.Fatalf("force terminal status: %v", err)
				}
				return task.ID
			}

			t.Run("StampAgentClaimIfUnclaimed refuses", func(t *testing.T) {
				taskID := mkTerminalTask(t, "stamp", false, false)
				ok, err := StampAgentClaimIfUnclaimed(database, taskID, runmode.LocalDefaultAgentID)
				if err != nil {
					t.Fatalf("StampAgentClaimIfUnclaimed: %v", err)
				}
				if ok {
					t.Error("ok=true on terminal task; data-layer guard didn't refuse")
				}
				// Audit: the row should be untouched by the failed
				// stamp — status preserved, no claim landed.
				got, _ := GetTask(database, taskID)
				if got.Status != terminalStatus {
					t.Errorf("status mutated by refused stamp: got %q want %q", got.Status, terminalStatus)
				}
				if got.ClaimedByAgentID != "" {
					t.Errorf("claim landed despite refusal: got %q", got.ClaimedByAgentID)
				}
			})

			t.Run("HandoffAgentClaim refuses", func(t *testing.T) {
				taskID := mkTerminalTask(t, "handoff", false, false)
				result, err := HandoffAgentClaim(database, taskID, runmode.LocalDefaultAgentID, runmode.LocalDefaultUserID)
				if err != nil {
					t.Fatalf("HandoffAgentClaim: %v", err)
				}
				if result == HandoffChanged {
					t.Error("HandoffChanged on terminal task; data-layer guard didn't refuse")
				}
				got, _ := GetTask(database, taskID)
				if got.Status != terminalStatus {
					t.Errorf("status mutated: got %q want %q", got.Status, terminalStatus)
				}
				if got.ClaimedByAgentID != "" {
					t.Errorf("claim landed: got %q", got.ClaimedByAgentID)
				}
			})

			t.Run("TakeoverClaimFromAgent refuses", func(t *testing.T) {
				// Seed the bot claim BEFORE forcing terminal — sticky
				// past close means a closed task could realistically
				// carry the bot's audit pointer. The guard refuses
				// the takeover anyway.
				taskID := mkTerminalTask(t, "takeover", true, false)
				ok, err := TakeoverClaimFromAgent(database, taskID, runmode.LocalDefaultUserID)
				if err != nil {
					t.Fatalf("TakeoverClaimFromAgent: %v", err)
				}
				if ok {
					t.Error("ok=true on terminal bot-claimed task; data-layer guard didn't refuse")
				}
				got, _ := GetTask(database, taskID)
				if got.Status != terminalStatus {
					t.Errorf("status mutated: got %q want %q", got.Status, terminalStatus)
				}
				if got.ClaimedByAgentID != runmode.LocalDefaultAgentID {
					t.Errorf("bot claim disturbed: got %q (sticky-past-close audit broken)", got.ClaimedByAgentID)
				}
				if got.ClaimedByUserID != "" {
					t.Errorf("user claim landed despite refusal: got %q", got.ClaimedByUserID)
				}
			})

			t.Run("ClaimQueuedTaskForUser refuses (existing guard)", func(t *testing.T) {
				taskID := mkTerminalTask(t, "userclaim", false, false)
				ok, err := ClaimQueuedTaskForUser(database, taskID, runmode.LocalDefaultUserID)
				if err != nil {
					t.Fatalf("ClaimQueuedTaskForUser: %v", err)
				}
				if ok {
					t.Error("ok=true on terminal task; status guard regressed")
				}
			})
		})
	}
}

// TestStampAgentClaimIfUnclaimed_WakesSnoozed pins the "snoozed ↔
// unclaimed" invariant from the auto-trigger angle: when a snoozed
// task receives a new matching event and the trigger fires, the
// stamp atomically wakes the task (snoozed → queued, snooze_until
// cleared) AND lands the bot claim. Pre-invariant, the stamp left
// status='snoozed' alongside a bot claim — an incoherent state
// (drain skips snoozed, but the immediate stamp+run spawn path
// ignored status). The invariant collapses that gap.
func TestStampAgentClaimIfUnclaimed_WakesSnoozed(t *testing.T) {
	database := newTestDB(t)
	seedLocalAgentForClaimTests(t, database)
	entity, _, err := FindOrCreateEntity(database, "github", "octo/repo#wake-stamp", "pr", "Wake Stamp", "https://example.com/ws")
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
	// Pre-stage: snoozed unclaimed (valid state under invariant).
	if _, err := database.Exec(
		`UPDATE tasks SET status = 'snoozed', snooze_until = '2099-01-01 00:00:00' WHERE id = ?`,
		task.ID,
	); err != nil {
		t.Fatalf("stage snooze: %v", err)
	}

	ok, err := StampAgentClaimIfUnclaimed(database, task.ID, runmode.LocalDefaultAgentID)
	if err != nil {
		t.Fatalf("StampAgentClaimIfUnclaimed: %v", err)
	}
	if !ok {
		t.Fatal("ok=false; stamp should have landed on the snoozed unclaimed task")
	}

	got, _ := GetTask(database, task.ID)
	if got.Status != "queued" {
		t.Errorf("Status = %q, want 'queued' (stamp must wake the snoozed task)", got.Status)
	}
	if got.SnoozeUntil != nil {
		t.Errorf("snooze_until still set = %v, want nil (stamp must clear snooze atomically)", got.SnoozeUntil)
	}
	if got.ClaimedByAgentID != runmode.LocalDefaultAgentID {
		t.Errorf("ClaimedByAgentID = %q, want %q", got.ClaimedByAgentID, runmode.LocalDefaultAgentID)
	}
}

// TestHandoffAgentClaim_WakesSnoozed pins the same invariant from
// the user→bot handoff angle: a user dragging their snoozed task
// to the Agent lane wakes the task as part of the transfer.
func TestHandoffAgentClaim_WakesSnoozed(t *testing.T) {
	database := newTestDB(t)
	seedLocalAgentForClaimTests(t, database)
	entity, _, err := FindOrCreateEntity(database, "github", "octo/repo#wake-handoff", "pr", "Wake Handoff", "https://example.com/wh")
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
	// Stage snoozed unclaimed.
	if _, err := database.Exec(
		`UPDATE tasks SET status = 'snoozed', snooze_until = '2099-01-01 00:00:00' WHERE id = ?`,
		task.ID,
	); err != nil {
		t.Fatalf("stage snooze: %v", err)
	}

	result, err := HandoffAgentClaim(database, task.ID, runmode.LocalDefaultAgentID, runmode.LocalDefaultUserID)
	if err != nil {
		t.Fatalf("HandoffAgentClaim: %v", err)
	}
	if result != HandoffChanged {
		t.Fatalf("result = %v, want HandoffChanged", result)
	}

	got, _ := GetTask(database, task.ID)
	if got.Status != "queued" {
		t.Errorf("Status = %q, want 'queued' (handoff must wake snoozed)", got.Status)
	}
	if got.SnoozeUntil != nil {
		t.Errorf("snooze_until still set; handoff must clear it atomically")
	}
	if got.ClaimedByAgentID != runmode.LocalDefaultAgentID {
		t.Errorf("ClaimedByAgentID = %q, want bot", got.ClaimedByAgentID)
	}
}

// TestStampAgentClaimIfUnclaimed pins the race-safe variant's three
// outcomes:
//
//   - unclaimed → claim lands, ok=true
//   - bot-already-owns (same agent) → no-op, ok=false (skip broadcast)
//   - user-already-owns → refuse to steal, ok=false; user claim survives
//
// The middle case is the load-bearing fix: stampAgentClaim no longer
// churns task_claimed broadcasts on every duplicate stamp call, and
// the user-claim case closes the auto-trigger-steals-user-claim race.
func TestStampAgentClaimIfUnclaimed(t *testing.T) {
	database := newTestDB(t)
	seedLocalAgentForClaimTests(t, database)
	entity, _, err := FindOrCreateEntity(database, "github", "octo/repo#stamp", "pr", "Stamp Test", "https://example.com/s")
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

	// First stamp on unclaimed row → lands.
	ok, err := StampAgentClaimIfUnclaimed(database, task.ID, runmode.LocalDefaultAgentID)
	if err != nil {
		t.Fatalf("first stamp: %v", err)
	}
	if !ok {
		t.Fatal("ok=false on first stamp; expected the claim to land")
	}

	// Re-stamp same agent → no-op.
	ok, err = StampAgentClaimIfUnclaimed(database, task.ID, runmode.LocalDefaultAgentID)
	if err != nil {
		t.Fatalf("re-stamp: %v", err)
	}
	if ok {
		t.Error("ok=true on idempotent re-stamp; expected no-op so the caller skips the broadcast")
	}

	// Switch to user claim (simulates a takeover that landed
	// before the next auto-trigger stamp). Then try to stamp again
	// — must refuse to overwrite the user's claim.
	if err := SetTaskClaimedByUser(database, task.ID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("set user claim: %v", err)
	}
	ok, err = StampAgentClaimIfUnclaimed(database, task.ID, runmode.LocalDefaultAgentID)
	if err != nil {
		t.Fatalf("stamp post-user-claim: %v", err)
	}
	if ok {
		t.Fatal("ok=true; agent claim stole a user claim — race guard broken")
	}
	got, err := GetTask(database, task.ID)
	if err != nil {
		t.Fatalf("re-read task: %v", err)
	}
	if got.ClaimedByUserID != runmode.LocalDefaultUserID {
		t.Errorf("user claim was overwritten: got %q want %q", got.ClaimedByUserID, runmode.LocalDefaultUserID)
	}
	if got.ClaimedByAgentID != "" {
		t.Errorf("agent claim landed on top of user claim: got %q want empty", got.ClaimedByAgentID)
	}
}

// TestHandoffAgentClaim covers all four cases of the user→bot
// handoff helper: unclaimed lands, same-user-claim transfers, same-
// agent is a no-op, different-user refuses. The "user-claimed →
// bot" case is the load-bearing one — without it, the Board's
// You → Agent drag (SKY-133) wouldn't work because the user's own
// claim would block the bot stamp.
func TestHandoffAgentClaim(t *testing.T) {
	database := newTestDB(t)
	seedLocalAgentForClaimTests(t, database)

	// Helper: fresh task per subtest (the partial-unique index on
	// (entity_id, event_type, dedup_key) would otherwise collide
	// across cases).
	mkTask := func(t *testing.T, suffix string) string {
		t.Helper()
		entity, _, err := FindOrCreateEntity(database, "github", "octo/repo#handoff-"+suffix, "pr", "Handoff "+suffix, "https://example.com/h-"+suffix)
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
		return task.ID
	}

	t.Run("unclaimed lands as Changed", func(t *testing.T) {
		taskID := mkTask(t, "unclaimed")
		result, err := HandoffAgentClaim(database, taskID, runmode.LocalDefaultAgentID, runmode.LocalDefaultUserID)
		if err != nil {
			t.Fatalf("HandoffAgentClaim: %v", err)
		}
		if result != HandoffChanged {
			t.Fatalf("result = %v, want HandoffChanged", result)
		}
		got, _ := GetTask(database, taskID)
		if got.ClaimedByAgentID != runmode.LocalDefaultAgentID {
			t.Errorf("agent claim not stamped: got %q", got.ClaimedByAgentID)
		}
	})

	t.Run("same-user-claim transfers as Changed", func(t *testing.T) {
		taskID := mkTask(t, "transfer")
		if ok, err := ClaimQueuedTaskForUser(database, taskID, runmode.LocalDefaultUserID); err != nil || !ok {
			t.Fatalf("pre-stage user claim: ok=%v err=%v", ok, err)
		}
		result, err := HandoffAgentClaim(database, taskID, runmode.LocalDefaultAgentID, runmode.LocalDefaultUserID)
		if err != nil {
			t.Fatalf("HandoffAgentClaim: %v", err)
		}
		if result != HandoffChanged {
			t.Fatalf("result = %v, want HandoffChanged (same-user → bot transfer)", result)
		}
		got, _ := GetTask(database, taskID)
		if got.ClaimedByAgentID != runmode.LocalDefaultAgentID {
			t.Errorf("agent claim not stamped after transfer: got %q", got.ClaimedByAgentID)
		}
		if got.ClaimedByUserID != "" {
			t.Errorf("user claim not cleared: got %q", got.ClaimedByUserID)
		}
	})

	t.Run("same-agent is NoOp", func(t *testing.T) {
		taskID := mkTask(t, "samebot")
		if err := SetTaskClaimedByAgent(database, taskID, runmode.LocalDefaultAgentID); err != nil {
			t.Fatalf("pre-stage bot claim: %v", err)
		}
		result, err := HandoffAgentClaim(database, taskID, runmode.LocalDefaultAgentID, runmode.LocalDefaultUserID)
		if err != nil {
			t.Fatalf("HandoffAgentClaim: %v", err)
		}
		if result != HandoffNoOp {
			t.Errorf("result = %v, want HandoffNoOp (idempotent same-agent)", result)
		}
	})

	t.Run("different-user refuses", func(t *testing.T) {
		taskID := mkTask(t, "intruder")
		// Seed a real users row for the FK — keeps the test FK-clean so
		// the failure mode (if Handoff's WHERE guard regresses) is
		// "ok=true claim stolen" instead of an incidental FK error.
		const otherUserID = "00000000-0000-0000-0000-0000000009aa"
		if _, err := database.Exec(
			`INSERT INTO users (id, display_name) VALUES (?, 'Other User')`,
			otherUserID,
		); err != nil {
			t.Fatalf("seed other user: %v", err)
		}
		if err := SetTaskClaimedByUser(database, taskID, otherUserID); err != nil {
			t.Fatalf("pre-stage other user claim: %v", err)
		}
		result, err := HandoffAgentClaim(database, taskID, runmode.LocalDefaultAgentID, runmode.LocalDefaultUserID)
		if err != nil {
			t.Fatalf("HandoffAgentClaim: %v", err)
		}
		if result != HandoffRefused {
			t.Errorf("result = %v, want HandoffRefused (different user owns it)", result)
		}
		got, _ := GetTask(database, taskID)
		if got.ClaimedByUserID != otherUserID {
			t.Errorf("other user's claim was overwritten: got %q want %q", got.ClaimedByUserID, otherUserID)
		}
	})

	t.Run("missing task refuses", func(t *testing.T) {
		result, err := HandoffAgentClaim(database, "no-such-task", runmode.LocalDefaultAgentID, runmode.LocalDefaultUserID)
		if err != nil {
			t.Fatalf("HandoffAgentClaim: %v", err)
		}
		if result != HandoffRefused {
			t.Errorf("result = %v, want HandoffRefused for missing task", result)
		}
	})
}

// TestTakeoverClaimFromAgent pins the swipe-claim race-safe takeover
// helper's three branches: bot→user flip lands; race-lost cases
// (already user-claimed by another user, or no bot claim to take
// over) return ok=false without changing state.
func TestTakeoverClaimFromAgent(t *testing.T) {
	database := newTestDB(t)
	seedLocalAgentForClaimTests(t, database)
	entity, _, err := FindOrCreateEntity(database, "github", "octo/repo#takeover", "pr", "Takeover Test", "https://example.com/t")
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

	// (1) No claim at all → takeover refuses (not a takeover; use
	// ClaimQueuedTaskForUser for the unclaimed case).
	ok, err := TakeoverClaimFromAgent(database, task.ID, runmode.LocalDefaultUserID)
	if err != nil {
		t.Fatalf("takeover-from-naked: %v", err)
	}
	if ok {
		t.Error("ok=true taking over an unclaimed task; helper should refuse")
	}

	// (2) Stamp bot claim, then take it over → lands.
	if err := SetTaskClaimedByAgent(database, task.ID, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("stamp agent claim: %v", err)
	}
	ok, err = TakeoverClaimFromAgent(database, task.ID, runmode.LocalDefaultUserID)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if !ok {
		t.Fatal("ok=false on legitimate bot→user takeover")
	}
	got, err := GetTask(database, task.ID)
	if err != nil {
		t.Fatalf("re-read post-takeover: %v", err)
	}
	if got.ClaimedByAgentID != "" {
		t.Errorf("agent claim survived takeover: got %q", got.ClaimedByAgentID)
	}
	if got.ClaimedByUserID != runmode.LocalDefaultUserID {
		t.Errorf("user claim didn't land: got %q want %q", got.ClaimedByUserID, runmode.LocalDefaultUserID)
	}

	// (3) Already user-claimed (by this user, no bot claim to take
	// over from) → refuse. This guards the swipe-claim handler's
	// idempotent same-user branch from accidentally going down the
	// takeover path.
	ok, err = TakeoverClaimFromAgent(database, task.ID, runmode.LocalDefaultUserID)
	if err != nil {
		t.Fatalf("takeover-from-user-claim: %v", err)
	}
	if ok {
		t.Error("ok=true taking over a user-claimed task; helper should refuse")
	}
}

// TestTaskClaim_StickyPastClose pins the SKY-261 audit invariant:
// claim columns are NOT cleared when a task closes. status='done' +
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
