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
