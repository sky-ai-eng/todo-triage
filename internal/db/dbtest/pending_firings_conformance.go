package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// PendingFiringsStoreFactory is what a per-backend test file hands to
// RunPendingFiringsStoreConformance. Returns:
//   - the wired PendingFiringsStore impl,
//   - the orgID to pass to every call,
//   - a PendingFiringsSeeder for fixtures the store can't create itself
//     (entity → task → event_handler → event chains, plus optional
//     non-terminal runs for the HasActiveAutoRunForEntity gate).
type PendingFiringsStoreFactory func(t *testing.T) (
	store db.PendingFiringsStore,
	orgID string,
	seed PendingFiringsSeeder,
)

// PendingFiringsTuple is the minimum identifier set Enqueue needs.
// Returned by the per-backend Tuple seeder so each subtest can stand
// up an independent (entity, task, trigger, event) chain.
type PendingFiringsTuple struct {
	EntityID  string
	TaskID    string
	TriggerID string
	EventID   string
	// UserID is the value Enqueue should bind to creator_user_id when
	// the Postgres schema requires it. SQLite ignores it.
	UserID string
}

// PendingFiringsSeeder bags raw-SQL helpers backend tests provide.
type PendingFiringsSeeder struct {
	// Tuple inserts a fresh entity/task/trigger/event chain and
	// returns the IDs Enqueue needs.
	Tuple func(t *testing.T) PendingFiringsTuple

	// ActiveAutoRun inserts a non-terminal trigger_type='event' run
	// against an existing taskID. Used to exercise the
	// HasActiveAutoRunForEntity gate without going through the
	// router's spawner.
	ActiveAutoRun func(t *testing.T, taskID string) string

	// TerminalAutoRun inserts a terminal trigger_type='event' run
	// against the taskID — paired with ActiveAutoRun to assert the
	// gate flips false once the run terminates.
	TerminalAutoRun func(t *testing.T, taskID string) string

	// ManualRun inserts a non-terminal trigger_type='manual' run
	// against the taskID. Used to assert the gate ignores manual
	// delegations (per SKY-189 design — manual decoupled from queue).
	ManualRun func(t *testing.T, taskID string) string
}

// RunPendingFiringsStoreConformance covers the pending-firings
// contract every backend impl must hold:
//
//   - Enqueue inserts a row in 'pending' status and returns inserted=true.
//   - Enqueue with the same (task_id, trigger_id) while one is pending
//     collapses via ON CONFLICT DO NOTHING and returns inserted=false.
//   - PopForEntity returns the oldest pending row and leaves it
//     'pending' (no implicit reservation).
//   - PopForEntity returns nil on empty queue and ignores non-pending.
//   - MarkFired flips 'pending' → 'fired' with run_id; idempotent
//     against already-terminal rows (guarded by status='pending').
//   - MarkSkipped flips 'pending' → 'skipped_stale' with reason;
//     same idempotency guard.
//   - HasPendingForEntity tracks presence of 'pending' rows.
//   - HasActiveAutoRunForEntity is true for non-terminal event-trigger
//     runs, false for terminal runs, false for manual runs.
//   - EntityCanFireImmediately is false when either gate is active.
//   - ListEntitiesWithPending returns distinct entity ids that have
//     at least one 'pending' row, scoped to the org.
//   - ListForEntity orders by queued_at ASC then id ASC.
func RunPendingFiringsStoreConformance(t *testing.T, mk PendingFiringsStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Enqueue_inserts_pending_row", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		inserted, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if !inserted {
			t.Errorf("first Enqueue should report inserted=true")
		}
		rows, err := s.ListForEntity(ctx, orgID, tup.EntityID)
		if err != nil {
			t.Fatalf("ListForEntity: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("expected 1 row, got %d", len(rows))
		}
		if rows[0].Status != domain.PendingFiringStatusPending {
			t.Errorf("status = %q, want pending", rows[0].Status)
		}
		if rows[0].EntityID != tup.EntityID || rows[0].TaskID != tup.TaskID || rows[0].TriggerID != tup.TriggerID {
			t.Errorf("row identity mismatch: %+v", rows[0])
		}
		if rows[0].TriggeringEventID != tup.EventID {
			t.Errorf("triggering_event_id = %q, want %q", rows[0].TriggeringEventID, tup.EventID)
		}
	})

	t.Run("Enqueue_collapses_duplicate", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		if inserted, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID); err != nil || !inserted {
			t.Fatalf("first Enqueue: inserted=%v err=%v", inserted, err)
		}
		inserted, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID)
		if err != nil {
			t.Fatalf("duplicate Enqueue: %v", err)
		}
		if inserted {
			t.Errorf("duplicate (task_id, trigger_id) while pending should report inserted=false")
		}
		rows, _ := s.ListForEntity(ctx, orgID, tup.EntityID)
		if len(rows) != 1 {
			t.Errorf("dedup should keep one row, got %d", len(rows))
		}
	})

	t.Run("PopForEntity_returns_oldest_pending", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup1 := seed.Tuple(t) // same entity, distinct task+trigger
		tup2 := seed.Tuple(t)
		// Re-point tup2 at tup1's entity by inserting against tup1's
		// entity with tup2's task/trigger. Backend seeders can't share
		// an entity across two Tuple calls, so we enqueue against
		// tup1.EntityID using tup1.TaskID/tup1.TriggerID first then
		// tup2.TaskID/tup2.TriggerID — both reference rows the seeder
		// already FK-validated under their own entities. The dedup
		// index is on (task_id, trigger_id) so collisions across
		// entities aren't a concern.
		if _, err := s.Enqueue(ctx, orgID, tup1.UserID, tup1.EntityID, tup1.TaskID, tup1.TriggerID, tup1.EventID); err != nil {
			t.Fatalf("first Enqueue: %v", err)
		}
		if _, err := s.Enqueue(ctx, orgID, tup2.UserID, tup1.EntityID, tup2.TaskID, tup2.TriggerID, tup2.EventID); err != nil {
			t.Fatalf("second Enqueue: %v", err)
		}
		got, err := s.PopForEntity(ctx, orgID, tup1.EntityID)
		if err != nil || got == nil {
			t.Fatalf("Pop: got=%v err=%v", got, err)
		}
		if got.TaskID != tup1.TaskID {
			t.Errorf("Pop returned task %q, want oldest %q", got.TaskID, tup1.TaskID)
		}
		// Non-mutating: the row should still be pending.
		if got.Status != domain.PendingFiringStatusPending {
			t.Errorf("popped row status = %q, want pending (Pop is non-mutating)", got.Status)
		}
		rows, _ := s.ListForEntity(ctx, orgID, tup1.EntityID)
		pendingCount := 0
		for _, r := range rows {
			if r.Status == domain.PendingFiringStatusPending {
				pendingCount++
			}
		}
		if pendingCount != 2 {
			t.Errorf("queue should still have 2 pending after Pop, got %d", pendingCount)
		}
	})

	t.Run("PopForEntity_nil_on_empty_queue", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		got, err := s.PopForEntity(ctx, orgID, tup.EntityID)
		if err != nil {
			t.Fatalf("Pop: %v", err)
		}
		if got != nil {
			t.Errorf("Pop on empty queue should be nil, got %+v", got)
		}
	})

	t.Run("PopForEntity_ignores_non_pending", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		if _, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		row, err := s.PopForEntity(ctx, orgID, tup.EntityID)
		if err != nil || row == nil {
			t.Fatalf("Pop: row=%v err=%v", row, err)
		}
		if err := s.MarkSkipped(ctx, orgID, row.ID, domain.PendingFiringSkipTaskClosed); err != nil {
			t.Fatalf("MarkSkipped: %v", err)
		}
		got, err := s.PopForEntity(ctx, orgID, tup.EntityID)
		if err != nil {
			t.Fatalf("Pop after skip: %v", err)
		}
		if got != nil {
			t.Errorf("Pop should ignore skipped_stale rows, got %+v", got)
		}
	})

	t.Run("MarkFired_transitions_with_run_id", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		if _, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		row, _ := s.PopForEntity(ctx, orgID, tup.EntityID)

		// MarkFired references a real run row in Postgres
		// (fired_run_id has FK with ON DELETE on (fired_run_id, org_id)
		// referencing runs(id, org_id)). The seeder's run-insert
		// helpers produce valid run ids.
		runID := seed.ActiveAutoRun(t, tup.TaskID)
		if err := s.MarkFired(ctx, orgID, row.ID, runID); err != nil {
			t.Fatalf("MarkFired: %v", err)
		}
		rows, _ := s.ListForEntity(ctx, orgID, tup.EntityID)
		if len(rows) != 1 || rows[0].Status != domain.PendingFiringStatusFired {
			t.Errorf("expected one fired row, got %+v", rows)
		}
		if rows[0].FiredRunID == nil || *rows[0].FiredRunID != runID {
			t.Errorf("fired_run_id = %v, want pointer to %q", rows[0].FiredRunID, runID)
		}
		if rows[0].DrainedAt == nil {
			t.Errorf("drained_at should be set after MarkFired")
		}
	})

	t.Run("MarkFired_no_op_on_terminal", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		if _, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		row, _ := s.PopForEntity(ctx, orgID, tup.EntityID)
		if err := s.MarkSkipped(ctx, orgID, row.ID, domain.PendingFiringSkipTaskClosed); err != nil {
			t.Fatalf("MarkSkipped: %v", err)
		}
		runID := seed.ActiveAutoRun(t, tup.TaskID)
		// Should silently no-op — guarded by WHERE status='pending'.
		if err := s.MarkFired(ctx, orgID, row.ID, runID); err != nil {
			t.Fatalf("MarkFired on terminal: %v", err)
		}
		rows, _ := s.ListForEntity(ctx, orgID, tup.EntityID)
		if len(rows) != 1 || rows[0].Status != domain.PendingFiringStatusSkippedStale {
			t.Errorf("terminal row should stay skipped_stale, got %+v", rows)
		}
	})

	t.Run("MarkSkipped_records_reason", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		if _, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		row, _ := s.PopForEntity(ctx, orgID, tup.EntityID)
		if err := s.MarkSkipped(ctx, orgID, row.ID, domain.PendingFiringSkipBreakerTripped); err != nil {
			t.Fatalf("MarkSkipped: %v", err)
		}
		rows, _ := s.ListForEntity(ctx, orgID, tup.EntityID)
		if rows[0].SkipReason != domain.PendingFiringSkipBreakerTripped {
			t.Errorf("skip_reason = %q, want %q", rows[0].SkipReason, domain.PendingFiringSkipBreakerTripped)
		}
	})

	t.Run("HasPendingForEntity_tracks_pending_rows", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		has, err := s.HasPendingForEntity(ctx, orgID, tup.EntityID)
		if err != nil {
			t.Fatalf("HasPending: %v", err)
		}
		if has {
			t.Errorf("empty queue should not report pending")
		}
		if _, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		has, _ = s.HasPendingForEntity(ctx, orgID, tup.EntityID)
		if !has {
			t.Errorf("queue with pending row should report true")
		}
		row, _ := s.PopForEntity(ctx, orgID, tup.EntityID)
		_ = s.MarkSkipped(ctx, orgID, row.ID, domain.PendingFiringSkipTriggerDisabled)
		has, _ = s.HasPendingForEntity(ctx, orgID, tup.EntityID)
		if has {
			t.Errorf("after only terminal rows remain, HasPending should be false")
		}
	})

	t.Run("HasActiveAutoRunForEntity_gates_only_event_runs", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		has, _ := s.HasActiveAutoRunForEntity(ctx, orgID, tup.EntityID)
		if has {
			t.Errorf("no runs → should report false")
		}

		// Manual run on the task — must NOT trip the gate per SKY-189.
		seed.ManualRun(t, tup.TaskID)
		has, _ = s.HasActiveAutoRunForEntity(ctx, orgID, tup.EntityID)
		if has {
			t.Errorf("manual run should not trip the auto-run gate")
		}

		// Active event run — should trip.
		seed.ActiveAutoRun(t, tup.TaskID)
		has, _ = s.HasActiveAutoRunForEntity(ctx, orgID, tup.EntityID)
		if !has {
			t.Errorf("active event run should trip the gate")
		}
	})

	t.Run("HasActiveAutoRunForEntity_false_when_only_terminal", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		seed.TerminalAutoRun(t, tup.TaskID)
		has, _ := s.HasActiveAutoRunForEntity(ctx, orgID, tup.EntityID)
		if has {
			t.Errorf("terminal-only runs should not trip the gate, got true")
		}
	})

	t.Run("EntityCanFireImmediately_composes_both_gates", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)

		// Clean slate: no runs, no pending → can fire.
		can, err := s.EntityCanFireImmediately(ctx, orgID, tup.EntityID)
		if err != nil {
			t.Fatalf("EntityCanFireImmediately: %v", err)
		}
		if !can {
			t.Errorf("clean entity should be allowed to fire immediately")
		}

		// Pending row in queue → cannot fire.
		if _, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		can, _ = s.EntityCanFireImmediately(ctx, orgID, tup.EntityID)
		if can {
			t.Errorf("queue with pending row should block immediate fire")
		}

		// Skip the row → queue empty, can fire again.
		row, _ := s.PopForEntity(ctx, orgID, tup.EntityID)
		_ = s.MarkSkipped(ctx, orgID, row.ID, domain.PendingFiringSkipTaskClosed)
		can, _ = s.EntityCanFireImmediately(ctx, orgID, tup.EntityID)
		if !can {
			t.Errorf("after skip drains queue, should be allowed to fire (no active run either)")
		}

		// Active auto run → blocks.
		seed.ActiveAutoRun(t, tup.TaskID)
		can, _ = s.EntityCanFireImmediately(ctx, orgID, tup.EntityID)
		if can {
			t.Errorf("active auto run should block immediate fire")
		}
	})

	t.Run("ListEntitiesWithPending_distinct_per_entity", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tupA := seed.Tuple(t)
		tupB := seed.Tuple(t)

		// Two pending rows on tupA's entity; one on tupB's.
		if _, err := s.Enqueue(ctx, orgID, tupA.UserID, tupA.EntityID, tupA.TaskID, tupA.TriggerID, tupA.EventID); err != nil {
			t.Fatalf("Enqueue tupA: %v", err)
		}
		// Same entity, distinct task — Tuple gives us a fresh tuple
		// rooted on a different entity, so use tupB's task/trigger
		// against tupA's entity for the dedup-non-collision check.
		if _, err := s.Enqueue(ctx, orgID, tupB.UserID, tupA.EntityID, tupB.TaskID, tupB.TriggerID, tupB.EventID); err != nil {
			t.Fatalf("Enqueue second on tupA entity: %v", err)
		}

		ids, err := s.ListEntitiesWithPending(ctx, orgID)
		if err != nil {
			t.Fatalf("ListEntitiesWithPending: %v", err)
		}
		if len(ids) != 1 || ids[0] != tupA.EntityID {
			t.Errorf("expected distinct ids = [%q], got %v", tupA.EntityID, ids)
		}
	})

	t.Run("ListForEntity_fifo_order", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup1 := seed.Tuple(t)
		tup2 := seed.Tuple(t)
		// Two rows on the same entity in known order.
		if _, err := s.Enqueue(ctx, orgID, tup1.UserID, tup1.EntityID, tup1.TaskID, tup1.TriggerID, tup1.EventID); err != nil {
			t.Fatalf("Enqueue first: %v", err)
		}
		if _, err := s.Enqueue(ctx, orgID, tup2.UserID, tup1.EntityID, tup2.TaskID, tup2.TriggerID, tup2.EventID); err != nil {
			t.Fatalf("Enqueue second: %v", err)
		}
		rows, err := s.ListForEntity(ctx, orgID, tup1.EntityID)
		if err != nil {
			t.Fatalf("ListForEntity: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("expected 2 rows, got %d", len(rows))
		}
		// queued_at ASC then id ASC — first enqueue must be index 0.
		if rows[0].TaskID != tup1.TaskID {
			t.Errorf("FIFO order broken: rows[0].TaskID=%q, want %q", rows[0].TaskID, tup1.TaskID)
		}
		if rows[1].TaskID != tup2.TaskID {
			t.Errorf("FIFO order broken: rows[1].TaskID=%q, want %q", rows[1].TaskID, tup2.TaskID)
		}
	})
}

// LocalUserID is a convenience for SQLite seeders — the local user
// sentinel is what the router passes today, and the conformance suite
// uses it through PendingFiringsTuple.UserID.
var LocalUserID = runmode.LocalDefaultUserID
