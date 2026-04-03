package domain

import "time"

// SwipeEvent represents a user triage action on a task.
type SwipeEvent struct {
	ID           int
	TaskID       string
	Action       string // "claim" | "dismiss" | "snooze" | "delegate" | "undo"
	HesitationMs int
	CreatedAt    time.Time
}
