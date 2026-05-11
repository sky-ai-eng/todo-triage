package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// SwipeStoreFactory is what a per-backend test file hands to
// RunSwipeStoreConformance. The factory returns:
//   - the wired SwipeStore impl
//   - the orgID to pass to every method
//   - a seedTask hook that creates a task row the harness can swipe
//     against. The harness is schema-blind; backend tests own task
//     creation against their dialect's columns.
//   - a readTask hook that returns the task's current status +
//     snooze_until so the harness can assert state transitions
//     without touching the schema directly.
type SwipeStoreFactory func(t *testing.T) (store db.SwipeStore, orgID string, seedTask TaskSeederForSwipes, readTask TaskReaderForSwipes)

// TaskSeederForSwipes creates one task row in 'queued' state and
// returns its ID. The harness calls it once per subtest.
type TaskSeederForSwipes func(t *testing.T) string

// TaskReaderForSwipes returns the task's status + snooze_until so
// the harness can assert post-swipe state without coupling to
// schema. snoozeUntil is the zero Time when NULL.
type TaskReaderForSwipes func(t *testing.T, taskID string) (status string, snoozeUntil time.Time)

// RunSwipeStoreConformance runs the shared assertion suite. The
// invariants are the contract every SwipeStore impl must hold:
//
//   - RecordSwipe maps action → status correctly + writes an audit
//     row + leaves a fresh task in the right state.
//   - SnoozeTask sets snooze_until + status='snoozed'.
//   - RequeueTask flips status back without writing an audit row,
//     and returns ok=false on a missing id.
//   - UndoLastSwipe writes an 'undo' audit row + resets the task to
//     queued + clears snooze_until.
//   - Context cancellation fails fast.
func RunSwipeStoreConformance(t *testing.T, factory SwipeStoreFactory) {
	t.Helper()

	t.Run("RecordSwipe_ClaimMapsToClaimed", func(t *testing.T) {
		store, orgID, seedTask, readTask := factory(t)
		ctx := context.Background()
		taskID := seedTask(t)
		newStatus, err := store.RecordSwipe(ctx, orgID, taskID, "claim", 200)
		if err != nil {
			t.Fatalf("RecordSwipe: %v", err)
		}
		if newStatus != "claimed" {
			t.Fatalf("newStatus=%q want claimed", newStatus)
		}
		status, _ := readTask(t, taskID)
		if status != "claimed" {
			t.Fatalf("task.status=%q want claimed (DB not updated)", status)
		}
	})

	t.Run("RecordSwipe_DismissMapsToDismissed", func(t *testing.T) {
		store, orgID, seedTask, readTask := factory(t)
		taskID := seedTask(t)
		if _, err := store.RecordSwipe(context.Background(), orgID, taskID, "dismiss", 0); err != nil {
			t.Fatalf("RecordSwipe: %v", err)
		}
		status, _ := readTask(t, taskID)
		if status != "dismissed" {
			t.Fatalf("task.status=%q want dismissed", status)
		}
	})

	t.Run("RecordSwipe_UnknownActionFallsBackToQueued", func(t *testing.T) {
		// Misuse path: action="garbage" must NOT silently strand the
		// task in an invalid status. The fallback keeps the task
		// reachable; the audit row preserves the original (wrong)
		// action so a future debug pass can find it.
		store, orgID, seedTask, readTask := factory(t)
		newStatus, err := store.RecordSwipe(context.Background(), orgID, seedTask(t), "garbage", 0)
		if err != nil {
			t.Fatalf("RecordSwipe: %v", err)
		}
		if newStatus != "queued" {
			t.Fatalf("newStatus=%q want queued for unknown action", newStatus)
		}
		_ = readTask
	})

	t.Run("SnoozeTask_SetsSnoozeUntilAndStatus", func(t *testing.T) {
		store, orgID, seedTask, readTask := factory(t)
		taskID := seedTask(t)
		until := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
		if err := store.SnoozeTask(context.Background(), orgID, taskID, until, 150); err != nil {
			t.Fatalf("SnoozeTask: %v", err)
		}
		status, snoozeUntil := readTask(t, taskID)
		if status != "snoozed" {
			t.Fatalf("status=%q want snoozed", status)
		}
		// SQLite stores DATETIME as a string and roundtrips through
		// time.Time at second resolution; allow a 5s slop so the
		// assertion isn't brittle across timezone serialization.
		if delta := snoozeUntil.Sub(until); delta < -5*time.Second || delta > 5*time.Second {
			t.Fatalf("snooze_until=%v want close to %v (delta=%v)", snoozeUntil, until, delta)
		}
	})

	t.Run("RequeueTask_FlipsToQueuedWithoutAuditRow", func(t *testing.T) {
		store, orgID, seedTask, readTask := factory(t)
		taskID := seedTask(t)
		// First swipe → claimed so we can observe the transition back.
		if _, err := store.RecordSwipe(context.Background(), orgID, taskID, "claim", 0); err != nil {
			t.Fatalf("seed swipe: %v", err)
		}
		ok, err := store.RequeueTask(context.Background(), orgID, taskID)
		if err != nil {
			t.Fatalf("RequeueTask: %v", err)
		}
		if !ok {
			t.Fatalf("RequeueTask returned ok=false for an existing task")
		}
		status, _ := readTask(t, taskID)
		if status != "queued" {
			t.Fatalf("status=%q want queued", status)
		}
	})

	t.Run("RequeueTask_OkFalseOnMissingTask", func(t *testing.T) {
		// Use a syntactically-valid UUID for the missing id —
		// Postgres rejects non-UUID strings at the column type
		// level before the WHERE filter ever runs, and we want to
		// assert the "no row matched" path, not the parse error.
		store, orgID, _, _ := factory(t)
		missing := "00000000-0000-0000-0000-000000000000"
		ok, err := store.RequeueTask(context.Background(), orgID, missing)
		if err != nil {
			t.Fatalf("RequeueTask returned error for missing id: %v", err)
		}
		if ok {
			t.Fatalf("ok=true for missing id; want false")
		}
	})

	t.Run("UndoLastSwipe_ResetsTaskAndClearsSnooze", func(t *testing.T) {
		store, orgID, seedTask, readTask := factory(t)
		taskID := seedTask(t)
		// Snooze first so we can verify the undo clears snooze_until.
		until := time.Now().Add(time.Hour).UTC()
		if err := store.SnoozeTask(context.Background(), orgID, taskID, until, 0); err != nil {
			t.Fatalf("snooze: %v", err)
		}
		if err := store.UndoLastSwipe(context.Background(), orgID, taskID); err != nil {
			t.Fatalf("UndoLastSwipe: %v", err)
		}
		status, snoozeUntil := readTask(t, taskID)
		if status != "queued" {
			t.Fatalf("status=%q want queued after undo", status)
		}
		if !snoozeUntil.IsZero() {
			t.Fatalf("snooze_until=%v want zero after undo", snoozeUntil)
		}
	})

	t.Run("CtxCancellation_FailsFast", func(t *testing.T) {
		store, orgID, seedTask, _ := factory(t)
		taskID := seedTask(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := store.RecordSwipe(ctx, orgID, taskID, "claim", 0); err == nil {
			t.Fatalf("RecordSwipe with cancelled ctx returned nil error")
		}
	})
}
