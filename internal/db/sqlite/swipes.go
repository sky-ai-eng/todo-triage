package sqlite

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// swipeStore is the SQLite impl of db.SwipeStore. SQL bodies are
// ported verbatim from the pre-D2 internal/db/swipes.go; behavioral
// changes are:
//
//   - assertLocalOrg at every method entry,
//   - context propagation on every Exec/Begin,
//   - inTx wraps the multi-statement methods so a partial
//     swipe_events INSERT + tasks UPDATE can't strand the row.
type swipeStore struct{ q queryer }

func newSwipeStore(q queryer) db.SwipeStore { return &swipeStore{q: q} }

var _ db.SwipeStore = (*swipeStore)(nil)

func (s *swipeStore) RecordSwipe(ctx context.Context, orgID string, taskID, action string, hesitationMs int) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	// Action → status mapping. SKY-261 B+ split the responsibility axis
	// (who owns this) off the lifecycle axis (where in its life the
	// task is). claim + delegate are responsibility-only now — the
	// handler stamps claim columns; the status stays 'queued' here.
	// Only dismiss/snooze/complete (genuine lifecycle moves) transition
	// status. Unknown action defaults to 'queued' so a typo doesn't
	// strand the task; same fallback as pre-SKY-261.
	var newStatus string
	switch action {
	case "claim", "delegate":
		newStatus = "queued"
	case "dismiss":
		newStatus = "dismissed"
	case "snooze":
		newStatus = "snoozed"
	case "complete":
		newStatus = "done"
	default:
		newStatus = "queued"
	}
	err := inTx(ctx, s.q, func(q queryer) error {
		if _, err := q.ExecContext(ctx,
			`INSERT INTO swipe_events (task_id, action, hesitation_ms) VALUES (?, ?, ?)`,
			taskID, action, hesitationMs,
		); err != nil {
			return err
		}
		// snooze_until is cleared on EVERY transition out of the
		// pre-swipe state. None of the swipe target statuses (queued
		// for claim/delegate/unknown, dismissed, snoozed-by-SnoozeTask,
		// done) are semantically compatible with a leftover future
		// snooze_until — and the queue listing filter hides any
		// 'queued' row whose snooze_until is in the future, so a
		// snoozed→claimed→requeued path would otherwise leave the
		// task invisible. SnoozeTask is the only method that should
		// set snooze_until; everything else clears it.
		_, err := q.ExecContext(ctx,
			`UPDATE tasks SET status = ?, snooze_until = NULL WHERE id = ?`,
			newStatus, taskID,
		)
		return err
	})
	if err != nil {
		return "", err
	}
	return newStatus, nil
}

func (s *swipeStore) SnoozeTask(ctx context.Context, orgID string, taskID string, until time.Time, hesitationMs int) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	return inTx(ctx, s.q, func(q queryer) error {
		if _, err := q.ExecContext(ctx,
			`INSERT INTO swipe_events (task_id, action, hesitation_ms) VALUES (?, 'snooze', ?)`,
			taskID, hesitationMs,
		); err != nil {
			return err
		}
		_, err := q.ExecContext(ctx,
			`UPDATE tasks SET status = 'snoozed', snooze_until = ? WHERE id = ?`,
			until, taskID,
		)
		return err
	})
}

func (s *swipeStore) RequeueTask(ctx context.Context, orgID string, taskID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	var ok bool
	err := inTx(ctx, s.q, func(q queryer) error {
		// SKY-261 B+: Requeue clears both claim cols too — putting a
		// task back in the team's triage queue means it's no longer
		// claimed by anyone (the derived queue filter requires both
		// claim cols NULL).
		res, err := q.ExecContext(ctx,
			`UPDATE tasks
			    SET status = 'queued',
			        snooze_until = NULL,
			        claimed_by_agent_id = NULL,
			        claimed_by_user_id  = NULL
			  WHERE id = ?`,
			taskID,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		ok = n > 0
		return nil
	})
	return ok, err
}

func (s *swipeStore) UndoLastSwipe(ctx context.Context, orgID string, taskID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	return inTx(ctx, s.q, func(q queryer) error {
		if _, err := q.ExecContext(ctx,
			`INSERT INTO swipe_events (task_id, action) VALUES (?, 'undo')`,
			taskID,
		); err != nil {
			return err
		}
		// SKY-261 B+: undo mirrors requeue's full reset — claim cols
		// also clear. A claim/delegate swipe stamps the relevant
		// claim col; the post-swipe-handler teardown
		// (cleanupPendingApprovalRun + spawner.Cancel for the
		// dismiss/complete/claim paths) is the side-effect, but the
		// claim col left on the row would keep the task in the
		// owner's lane even after status returns to 'queued'. Clear
		// both cols so the task lands back in the team's unclaimed
		// triage queue, the same shape /requeue produces.
		_, err := q.ExecContext(ctx,
			`UPDATE tasks
			    SET status = 'queued',
			        snooze_until = NULL,
			        claimed_by_agent_id = NULL,
			        claimed_by_user_id  = NULL
			  WHERE id = ?`,
			taskID,
		)
		return err
	})
}
