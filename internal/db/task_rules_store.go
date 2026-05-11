package db

import (
	"context"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ShippedTaskRule is the tabular shape of one shipped system rule.
// Predicate "" maps to NULL (match-all) at INSERT time; the backend
// impls translate this into their dialect's null-literal.
//
// ID is a human-readable slug ("system-rule-ci-check-failed"). SQLite
// stores it verbatim (the column is TEXT). Postgres stores its UUID()
// form because the column is UUID-typed; UUID() is deterministic, so
// re-seeds across boots produce the same row identity.
type ShippedTaskRule struct {
	ID              string
	EventType       string
	Predicate       string // "" → NULL (match-all)
	Name            string
	DefaultPriority float64
	SortOrder       int
}

// shippedTaskRuleNamespace is a fixed UUID v4 used as the seed
// namespace for UUID5 derivation. Hardcoded once so re-seeds across
// every install land on the same UUID for a given slug. Generated
// out-of-band; never regenerate without a migration plan.
var shippedTaskRuleNamespace = uuid.MustParse("a9b6f3c1-7e58-4d1f-8c02-1e6f8c4a9b3e")

// UUID returns the deterministic UUID for this shipped rule's slug,
// suitable for use as the row id in a UUID-typed column. Same slug
// → same UUID across installs and across re-seeds.
func (r ShippedTaskRule) UUID() string {
	return uuid.NewSHA1(shippedTaskRuleNamespace, []byte(r.ID)).String()
}

// ShippedTaskRules is the v1 default rule set documented in
// docs/data-model-target.md → "Seeded defaults". Both backend impls
// of TaskRuleStore.Seed iterate this list; keep it in sync with the
// doc — new entries here are visible to every install on the next
// boot.
var ShippedTaskRules = []ShippedTaskRule{
	{
		ID:              "system-rule-ci-check-failed",
		EventType:       domain.EventGitHubPRCICheckFailed,
		Predicate:       `{"author_is_self":true}`,
		Name:            "CI check failed on my PR",
		DefaultPriority: 0.75,
		SortOrder:       0,
	},
	{
		ID:              "system-rule-review-changes-requested",
		EventType:       domain.EventGitHubPRReviewChangesRequested,
		Predicate:       `{"author_is_self":true}`,
		Name:            "Changes requested on my PR",
		DefaultPriority: 0.85,
		SortOrder:       1,
	},
	{
		ID:              "system-rule-review-commented",
		EventType:       domain.EventGitHubPRReviewCommented,
		Predicate:       `{"author_is_self":true}`,
		Name:            "Reviewer commented on my PR",
		DefaultPriority: 0.65,
		SortOrder:       2,
	},
	{
		ID:              "system-rule-review-requested",
		EventType:       domain.EventGitHubPRReviewRequested,
		Predicate:       "",
		Name:            "Someone requested my review",
		DefaultPriority: 0.80,
		SortOrder:       3,
	},
	{
		ID:              "system-rule-jira-assigned",
		EventType:       domain.EventJiraIssueAssigned,
		Predicate:       `{"assignee_is_self":true}`,
		Name:            "Jira issue assigned to me",
		DefaultPriority: 0.60,
		SortOrder:       4,
	},
	{
		// Companion to jira-assigned. A ticket that had subtasks
		// suppresses the initial assigned event (SKY-173 — parents
		// are containers, not work), then emits became_atomic when
		// the last subtask closes. This rule gives that belated
		// discovery path the same task-creation behavior the
		// assignment rule would have given it.
		ID:              "system-rule-jira-became-atomic",
		EventType:       domain.EventJiraIssueBecameAtomic,
		Predicate:       `{"assignee_is_self":true}`,
		Name:            "Jira issue decomposition resolved (now actionable)",
		DefaultPriority: 0.60,
		SortOrder:       5,
	},
}

//go:generate go run github.com/vektra/mockery/v2 --name=TaskRuleStore --output=./mocks --case=underscore --with-expecter

// TaskRuleStore owns task_rules — declarative rules that say "spawn a
// task when an event of type X matches predicate P". Three audiences:
//
//   - HTTP handlers (server/task_rules_handler.go) — full CRUD.
//   - Router (routing/router.go) — GetEnabledForEvent on every event.
//   - Startup seeder (main.go) — Seed on every boot, idempotent.
//
// Companion to TriggerStore (the next Wave 1 ticket, prompt_triggers).
// Split deliberately: rules decide whether a card appears on the
// Board; triggers decide whether to auto-delegate. Different
// audiences, different lifecycles, narrower handler dependency.
//
// Postgres / RLS note: every method runs on the app pool. No
// admin-only sidecar (unlike PromptStore's system_prompt_versions);
// shipped system rules and user-created rules live in the same table
// distinguished by source. assertLocalOrg pins orgID in the SQLite
// impl; the Postgres impl resolves creator_user_id via
// COALESCE(tf.current_user_id(), owner_user_id) so system-context
// seeds (no JWT) still satisfy the NOT NULL constraint.
type TaskRuleStore interface {
	// Seed inserts the v1 shipped system rules if they aren't
	// already present, leaving existing rows untouched so user
	// customizations (renames, disables, predicate edits) survive
	// across restarts. Implementation uses INSERT-OR-IGNORE
	// semantics — differs from PromptStore.SeedOrUpdate, which
	// re-syncs shipped content on hash change. Task-rule predicates
	// rarely evolve and we'd rather let users own their disabled
	// state than chase shipped-content drift.
	//
	// Idempotent: every method-call counts only rows actually
	// inserted; a re-seed of unchanged content is a no-op.
	Seed(ctx context.Context, orgID string) error

	// List returns every rule, ordered by sort_order ASC then name
	// ASC. Both system-seeded and user-created rules are included;
	// callers distinguish via the Source field.
	List(ctx context.Context, orgID string) ([]domain.TaskRule, error)

	// Get returns one rule by id, or (nil, nil) if not found.
	Get(ctx context.Context, orgID string, id string) (*domain.TaskRule, error)

	// Create inserts a new user-source rule. Caller supplies ID
	// (handler generates UUIDs upstream), event_type, predicate,
	// name, default_priority, sort_order, enabled; source is forced
	// to "user" and timestamps are stamped server-side.
	Create(ctx context.Context, orgID string, r domain.TaskRule) error

	// Update changes a rule's mutable fields. ID, source, and
	// created_at are immutable; event_type is intentionally
	// immutable too (changing it would invalidate the predicate
	// schema). updated_at is refreshed server-side.
	Update(ctx context.Context, orgID string, r domain.TaskRule) error

	// SetEnabled flips just the enabled bit. Used for the
	// "disable instead of delete" path on system rules — they're
	// shipped on every boot via Seed and we don't want a deleted
	// row to resurrect as enabled.
	SetEnabled(ctx context.Context, orgID string, id string, enabled bool) error

	// Delete hard-deletes a rule. The handler gates this on
	// source='user' — system rules go through SetEnabled(false).
	Delete(ctx context.Context, orgID string, id string) error

	// Reorder updates sort_order for each rule based on its
	// position in the given ID list. IDs not in the list keep
	// their current sort_order. Wrapped in a transaction so a
	// mid-stream failure doesn't leave half the list reordered.
	// Non-existent IDs are silently skipped — the only caller is
	// the frontend which sends IDs it just read; a stale ID
	// means a concurrent delete and the next re-fetch corrects
	// the list.
	Reorder(ctx context.Context, orgID string, ids []string) error

	// GetEnabledForEvent returns all enabled rules for a given
	// event_type. The router calls this on every routed event;
	// returning a small slice (a handful of rules per event_type
	// in practice) keeps the per-event allocation tight.
	GetEnabledForEvent(ctx context.Context, orgID string, eventType string) ([]domain.TaskRule, error)
}
