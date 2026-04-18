package domain

import "time"

// TaskRule is a declarative rule for creating tasks from events. Independent
// of automation — a user who wants surfacing without any auto-fire configures
// a rule and no matching prompt_trigger.
//
// Dedup: at most one active task per (entity_id, event_type, dedup_key)
// across non-terminal statuses, enforced by partial unique index on tasks.
// Subsequent matching events with the same dedup tuple bump the active task
// via task_events rather than creating new ones.
type TaskRule struct {
	ID                 string    `json:"id"`
	EventType          string    `json:"event_type"`           // FK to events_catalog(id)
	ScopePredicateJSON *string   `json:"scope_predicate_json"` // typed per event type; nil = match-all
	Enabled            bool      `json:"enabled"`
	Name               string    `json:"name"`
	DefaultPriority    float64   `json:"default_priority"` // 0.0–1.0, AI scorer prior
	SortOrder          int       `json:"sort_order"`       // UI ordering of categories
	Source             string    `json:"source"`           // system | user
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// TaskRuleSource values.
const (
	TaskRuleSourceSystem = "system"
	TaskRuleSourceUser   = "user"
)
