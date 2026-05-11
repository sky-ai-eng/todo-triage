package domain

import "time"

// TakenOverRun is one row in the "runs ready to resume" list — a run
// whose state was handed off to the user (Takeover flow), joined
// with its task title + source ID for display. Used by the CLI's
// resume command and the future "held takeovers" board panel.
type TakenOverRun struct {
	RunID        string
	SessionID    string
	WorktreePath string
	TaskTitle    string
	SourceID     string
	CompletedAt  time.Time
}
