package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// swipeStore is the Postgres impl of db.SwipeStore. SQL is fresh
// against D3's schema:
//
//   - swipe_events has org_id + creator_user_id columns NOT NULL,
//     populated via tf.current_user_id() (request-path) or the
//     COALESCE-to-org-owner fallback (system/test-path), same
//     pattern PromptStore.Create uses.
//   - tasks updates include org_id in WHERE as defense in depth
//     alongside RLS — if RLS were ever bypassed the org filter
//     still applies.
//
// Atomicity matches SQLite: each mutating method wraps the
// swipe_events INSERT + tasks UPDATE in a single tx so a partial
// state can't leak.
type swipeStore struct{ q queryer }

func newSwipeStore(q queryer) db.SwipeStore { return &swipeStore{q: q} }

var _ db.SwipeStore = (*swipeStore)(nil)

func (s *swipeStore) RecordSwipe(ctx context.Context, orgID string, taskID, action string, hesitationMs int) (string, error) {
	// SKY-261 B+ split the responsibility axis off the lifecycle axis.
	// claim + delegate are responsibility-only — the handler stamps
	// claim columns; this UPDATE leaves status at 'queued'. Only
	// dismiss/snooze/complete are genuine lifecycle moves.
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
	if err := s.runInTx(ctx, func(tx *sql.Tx) error {
		if err := insertSwipeEvent(ctx, tx, orgID, taskID, action, &hesitationMs); err != nil {
			return err
		}
		// clearSnooze=true: none of the target statuses (queued for
		// claim/delegate/unknown, dismissed, done) are semantically
		// compatible with a leftover future snooze_until — and the
		// queue listing filter hides any 'queued' row with a future
		// snooze_until. Mirrors the SQLite impl; SnoozeTask is the
		// only method that sets snooze_until, every other path clears
		// it.
		return updateTaskStatus(ctx, tx, orgID, taskID, newStatus, nil, true)
	}); err != nil {
		return "", err
	}
	return newStatus, nil
}

func (s *swipeStore) SnoozeTask(ctx context.Context, orgID string, taskID string, until time.Time, hesitationMs int) error {
	return s.runInTx(ctx, func(tx *sql.Tx) error {
		if err := insertSwipeEvent(ctx, tx, orgID, taskID, "snooze", &hesitationMs); err != nil {
			return err
		}
		return updateTaskStatus(ctx, tx, orgID, taskID, "snoozed", &until, false)
	})
}

func (s *swipeStore) RequeueTask(ctx context.Context, orgID string, taskID string) (bool, error) {
	var ok bool
	err := s.runInTx(ctx, func(tx *sql.Tx) error {
		// SKY-261 B+: Requeue puts a task back in the team's triage
		// queue, which means it's no longer claimed by anyone. Clear
		// both claim cols in the same UPDATE so the derived queue
		// filter (claim cols all NULL + status 'queued') picks the
		// row up immediately. Status reset to 'queued' covers the
		// snoozed-back-to-queue path too.
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks
			    SET status = 'queued',
			        snooze_until = NULL,
			        claimed_by_agent_id = NULL,
			        claimed_by_user_id  = NULL
			  WHERE org_id = $1 AND id = $2`,
			orgID, taskID,
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
	return s.runInTx(ctx, func(tx *sql.Tx) error {
		if err := insertSwipeEvent(ctx, tx, orgID, taskID, "undo", nil); err != nil {
			return err
		}
		// SKY-261 B+: undo mirrors requeue's full reset — claim cols
		// also clear. A claim/delegate swipe stamps the relevant
		// claim col; leaving it on the row would keep the task in
		// the owner's lane even after status returns to 'queued'.
		// Clear both cols so the task lands back in the team's
		// unclaimed triage queue, matching RequeueTask's shape. The
		// inline UPDATE bypasses updateTaskStatus because that
		// helper only handles the (status, snooze_until) axis pair —
		// growing it to take claim cols too would proliferate
		// boolean flags across every callsite for one path's needs.
		_, err := tx.ExecContext(ctx,
			`UPDATE tasks
			    SET status = $1,
			        snooze_until = NULL,
			        claimed_by_agent_id = NULL,
			        claimed_by_user_id  = NULL
			  WHERE org_id = $2 AND id = $3`,
			"queued", orgID, taskID,
		)
		return err
	})
}

// runInTx is the Postgres-side counterpart of sqlite's inTx — opens
// a tx on s.q if it's a *sql.DB, or runs the closure against the
// caller's *sql.Tx if we're already inside WithTx. Lets mutating
// store methods share atomicity-boundary code regardless of
// composition context.
func (s *swipeStore) runInTx(ctx context.Context, fn func(*sql.Tx) error) error {
	switch v := s.q.(type) {
	case *sql.Tx:
		return fn(v)
	case *sql.DB:
		tx, err := v.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	default:
		return errors.New("postgres swipes: unexpected queryer type")
	}
}

func insertSwipeEvent(ctx context.Context, tx *sql.Tx, orgID, taskID, action string, hesitationMs *int) error {
	// creator_user_id NOT NULL — use tf.current_user_id() (request
	// path) with COALESCE-to-org-owner for the system / test path,
	// same fallback PromptStore.Create uses so all writes share
	// one creator-resolution rule.
	var hesitation any
	if hesitationMs != nil {
		hesitation = *hesitationMs
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO swipe_events (org_id, creator_user_id, task_id, action, hesitation_ms)
		VALUES ($1,
			COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $1)),
			$2, $3, $4)
	`, orgID, taskID, action, hesitation)
	if err != nil {
		return fmt.Errorf("insert swipe_events: %w", err)
	}
	return nil
}

// updateTaskStatus is the shared tasks-row update for every swipe
// method. clearSnooze=true clears snooze_until; clearSnooze=false
// leaves it alone (RecordSwipe's path) or replaces it with the
// passed value (SnoozeTask's path). The two-flag shape lets one
// helper serve all three callsites without a separate UPDATE per
// permutation.
func updateTaskStatus(ctx context.Context, tx *sql.Tx, orgID, taskID, status string, snoozeUntil *time.Time, clearSnooze bool) error {
	switch {
	case clearSnooze:
		_, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = $1, snooze_until = NULL WHERE org_id = $2 AND id = $3`,
			status, orgID, taskID,
		)
		return err
	case snoozeUntil != nil:
		_, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = $1, snooze_until = $2 WHERE org_id = $3 AND id = $4`,
			status, *snoozeUntil, orgID, taskID,
		)
		return err
	default:
		_, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = $1 WHERE org_id = $2 AND id = $3`,
			status, orgID, taskID,
		)
		return err
	}
}
