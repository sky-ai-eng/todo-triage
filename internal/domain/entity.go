package domain

import "time"

// Entity is a long-lived source object (PR, issue, epic, message). Lives from
// first-poll until closed/merged. All events, tasks, and runs hang off it.
// Mirrors the `entities` table in internal/db/db.go.
type Entity struct {
	ID           string     `json:"id"`
	Source       string     `json:"source"`    // "github" | "jira" | "linear" | "slack"
	SourceID     string     `json:"source_id"` // "owner/repo#18", "SKY-123", etc.
	Kind         string     `json:"kind"`      // "pr" | "issue" | "epic" | "message"
	Title        string     `json:"title"`
	URL          string     `json:"url"`
	SnapshotJSON string     `json:"snapshot_json"` // opaque poller state — diff scope only, kept small
	Description  string     `json:"description"`   // flattened issue/PR body; NOT diffed
	State        string     `json:"state"`         // "active" | "closed"
	CreatedAt    time.Time  `json:"created_at"`
	LastPolledAt *time.Time `json:"last_polled_at"`
	ClosedAt     *time.Time `json:"closed_at"`
}
