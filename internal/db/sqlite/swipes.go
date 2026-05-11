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
	// Action → status mapping. Unknown action defaults to "queued" so
	// a typo or unknown future value doesn't strand the task in an
	// invalid status (existing pre-D2 behavior; preserved verbatim).
	var newStatus string
	switch action {
	case "claim":
		newStatus = "claimed"
	case "dismiss":
		newStatus = "dismissed"
	case "snooze":
		newStatus = "snoozed"
	case "delegate":
		newStatus = "delegated"
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
		_, err := q.ExecContext(ctx, `UPDATE tasks SET status = ? WHERE id = ?`, newStatus, taskID)
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
		res, err := q.ExecContext(ctx,
			`UPDATE tasks SET status = 'queued', snooze_until = NULL WHERE id = ?`,
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
		_, err := q.ExecContext(ctx,
			`UPDATE tasks SET status = 'queued', snooze_until = NULL WHERE id = ?`,
			taskID,
		)
		return err
	})
}
