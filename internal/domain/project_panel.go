package domain

import "time"

// ProjectPanelEntity is one row in the Projects panel's entity list
// — the subset of fields the panel renders, without the full
// Entity's snapshot_json blob. Trimmed projection because the panel
// shows dozens of rows and the JSON payload is large.
type ProjectPanelEntity struct {
	ID                      string
	Source                  string
	SourceID                string
	Kind                    string
	Title                   string
	URL                     string
	State                   string
	ClassificationRationale string
	CreatedAt               time.Time
	LastPolledAt            *time.Time
}
