package db

import (
	"database/sql"
	"time"
)

// RecordSwipe inserts a swipe event and updates the task status accordingly.
// Returns the new task status.
func RecordSwipe(database *sql.DB, taskID, action string, hesitationMs int) (string, error) {
	tx, err := database.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	// Insert swipe event
	_, err = tx.Exec(
		`INSERT INTO swipe_events (task_id, action, hesitation_ms) VALUES (?, ?, ?)`,
		taskID, action, hesitationMs,
	)
	if err != nil {
		return "", err
	}

	// Map action to task status
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
	default:
		newStatus = "queued"
	}

	_, err = tx.Exec(`UPDATE tasks SET status = ? WHERE id = ?`, newStatus, taskID)
	if err != nil {
		return "", err
	}

	return newStatus, tx.Commit()
}

// SnoozeTask sets the snooze_until timestamp and updates status to snoozed.
func SnoozeTask(database *sql.DB, taskID string, until time.Time, hesitationMs int) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`INSERT INTO swipe_events (task_id, action, hesitation_ms) VALUES (?, 'snooze', ?)`,
		taskID, hesitationMs,
	)
	if err != nil {
		return err
	}

	_, err = tx.Exec(
		`UPDATE tasks SET status = 'snoozed', snooze_until = ? WHERE id = ?`,
		until, taskID,
	)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// RequeueTask flips a task back to the queue without recording a
// swipe_events row. Used by state-driven requeue paths (Board's
// drag-to-Queue, SKY-207's "Return to queue" button, anything else
// that isn't a swipe-card UX undo). Mirrors UndoLastSwipe's status
// reset half but skips the audit row — those events belong to the
// swipe subsystem and would muddy the analytics if every drag-to-
// queue gesture got logged as if the user had swiped.
//
// Returns ok=false when no row matched the taskID. Without this
// signal, the public /requeue endpoint would silently 200 on a
// bogus id (the underlying UPDATE just affects 0 rows) — masking
// frontend bugs and making races between GetTask and the UPDATE
// indistinguishable from real successes. Mirrors the
// MarkAgentRunCancelledIfActive / MarkAgentRunDiscarded ok-bool
// pattern.
//
// Wrapped in a tx for symmetry with UndoLastSwipe + future-
// proofing if more state needs clearing on requeue (snooze_until
// is the only one today; if a future state needs reset, both
// helpers should evolve together).
func RequeueTask(database *sql.DB, taskID string) (bool, error) {
	tx, err := database.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`UPDATE tasks SET status = 'queued', snooze_until = NULL WHERE id = ?`,
		taskID,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n > 0, nil
}

// UndoLastSwipe reverts the most recent swipe on a task, setting it back to queued.
func UndoLastSwipe(database *sql.DB, taskID string) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Record the undo event
	_, err = tx.Exec(
		`INSERT INTO swipe_events (task_id, action) VALUES (?, 'undo')`,
		taskID,
	)
	if err != nil {
		return err
	}

	// Reset task to queued
	_, err = tx.Exec(
		`UPDATE tasks SET status = 'queued', snooze_until = NULL WHERE id = ?`,
		taskID,
	)
	if err != nil {
		return err
	}

	return tx.Commit()
}
