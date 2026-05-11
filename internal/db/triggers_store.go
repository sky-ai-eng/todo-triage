package db

import (
	"context"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=TriggerStore --output=./mocks --case=underscore --with-expecter

// TriggerStore owns prompt_triggers — declarative auto-delegation rules
// that say "when an event of type X matches predicate P, fire prompt Q
// against the matching task." Three audiences:
//
//   - HTTP handlers (server/triggers_handler.go, server/prompts_handler.go) —
//     full CRUD.
//   - Router (routing/router.go) — GetActiveForEvent on every event, Get on
//     drain-time firing resolution.
//   - Startup seeder (main.go via seed.go) — Seed on every boot, idempotent.
//
// Companion to TaskRuleStore. Both will eventually merge into one
// EventHandlerStore per SKY-259; until then they live as parallel stores
// with the same shape. The semantic difference: rules create unclaimed
// tasks (human triage); triggers create tasks and stamp claimed_by_agent_id
// at creation (pre-committed bot autonomy). See docs/multi-tenant-architecture.html
// §1 rule-vs-trigger card.
//
// Postgres / RLS note: TriggerStore is wired with two pools (mirroring
// PromptStore + TaskRuleStore):
//
//   - app pool — tf_app, RLS-active. Every CRUD method runs here.
//   - admin pool — supabase_admin, BYPASSRLS. Seed runs here because the
//     prompt_triggers row-modify RLS policy gates inserts on
//     creator_user_id = tf.current_user_id(); boot-time Seed has no JWT
//     claims and would otherwise hit the WITH CHECK on every shipped row.
//
// SQLite has no role concept; both pools collapse to one connection and
// assertLocalOrg pins orgID to LocalDefaultOrg.
type TriggerStore interface {
	// Seed inserts the v1 shipped system triggers if they aren't already
	// present, leaving existing rows untouched so user customizations
	// (re-enabling, retargeting prompts, predicate edits) survive across
	// restarts. Implementation uses INSERT-OR-IGNORE semantics — differs
	// from PromptStore.SeedOrUpdate, which re-syncs shipped content on
	// hash change. Trigger configuration rarely evolves and we'd rather
	// let users own their disabled/enabled state than chase shipped-content
	// drift.
	//
	// Idempotent: a re-seed of unchanged shipped triggers is a no-op.
	// Shipped triggers are seeded with Source='system' + Enabled=false
	// — per project convention they ship disabled and the user opts in.
	Seed(ctx context.Context, orgID string) error

	// List returns every trigger, ordered by created_at DESC. Both
	// system-seeded and user-created triggers are included; callers
	// distinguish via the Source field.
	List(ctx context.Context, orgID string) ([]domain.PromptTrigger, error)

	// Get returns one trigger by id, or (nil, nil) if not found.
	Get(ctx context.Context, orgID string, id string) (*domain.PromptTrigger, error)

	// Create inserts a new user-source trigger. Caller supplies ID
	// (handler generates UUIDs upstream), prompt_id, event_type,
	// trigger_type, predicate, breaker config, min_autonomy_suitability,
	// enabled; source is forced to "user" and timestamps are stamped
	// server-side.
	Create(ctx context.Context, orgID string, t domain.PromptTrigger) error

	// Update changes a trigger's mutable fields (predicate, breaker
	// threshold, min_autonomy_suitability). ID, source, prompt_id,
	// event_type, trigger_type, and created_at are immutable —
	// changing the latter four would semantically be a different
	// trigger entirely, so handlers gate that path on delete+recreate.
	Update(ctx context.Context, orgID string, t domain.PromptTrigger) error

	// SetEnabled flips just the enabled bit. Owned by POST /toggle.
	SetEnabled(ctx context.Context, orgID string, id string, enabled bool) error

	// Delete hard-deletes a trigger. The handler gates this on
	// source='user' — system triggers go through SetEnabled(false)
	// instead so the next boot's Seed doesn't resurrect them as
	// enabled.
	Delete(ctx context.Context, orgID string, id string) error

	// ListForPrompt returns all triggers referencing the given prompt
	// (any source), ordered by created_at DESC. The prompts handler
	// uses it to surface "triggers using this prompt" alongside prompt
	// metadata.
	ListForPrompt(ctx context.Context, orgID string, promptID string) ([]domain.PromptTrigger, error)

	// GetActiveForEvent returns all enabled triggers for a given
	// event_type. The router calls this on every routed event;
	// returning a small slice keeps the per-event allocation tight.
	GetActiveForEvent(ctx context.Context, orgID string, eventType string) ([]domain.PromptTrigger, error)
}

// ShippedPromptTrigger is the tabular shape of one shipped system
// trigger. Predicate "" maps to NULL (match-all) at INSERT time; the
// backend impls translate this into their dialect's null literal.
//
// ID is a human-readable slug ("system-trigger-ci-fix"). SQLite stores
// it verbatim. Postgres stores its UUIDFor(orgID) derivation because
// the column is UUID-typed AND the PRIMARY KEY is global (not scoped
// by org_id) — same constraint TaskRuleStore solved with the same
// per-org UUIDv5 trick.
type ShippedPromptTrigger struct {
	ID                     string
	PromptID               string
	EventType              string
	Predicate              string // "" → NULL (match-all)
	BreakerThreshold       int
	MinAutonomySuitability float64
}

// shippedPromptTriggerNamespace is a fixed UUID v4 used as the seed
// namespace for the per-(slug, orgID) UUID5 derivation. Hardcoded once
// so re-seeds across every install land on the same UUID for a given
// pair. Distinct from shippedTaskRuleNamespace because rules and
// triggers are separate domains today (SKY-259 will eventually merge
// them but the namespaces stay distinct for traceability).
var shippedPromptTriggerNamespace = uuid.MustParse("c4f2e9b8-1a3d-4e7f-9c5b-2d8e6f4a1b3c")

// UUIDFor returns the deterministic per-org UUID for this shipped
// trigger's slug. Same (slug, orgID) → same UUID across installs and
// across re-seeds; two different orgs get two different UUIDs for the
// same shipped slug. Without orgID in the derivation, the global PK
// on prompt_triggers.id would collide across tenants and only the
// first org to seed would get any system triggers.
func (t ShippedPromptTrigger) UUIDFor(orgID string) string {
	return uuid.NewSHA1(shippedPromptTriggerNamespace, []byte(t.ID+"/"+orgID)).String()
}

// ShippedPromptTriggers is the v1 default trigger set previously
// inlined in seed.go. Each ships with Enabled=false (see project
// memory feedback_system_triggers.md — system triggers are static
// reference examples, users opt in or replace them). Both backend
// impls of TriggerStore.Seed iterate this list.
//
// PromptID values reference shipped prompts seeded by PromptStore.SeedOrUpdate
// — call order is PromptStore.SeedOrUpdate → TriggerStore.Seed so the
// FK from prompt_triggers (prompt_id, org_id) → prompts (id, org_id)
// is satisfied at seed time in Postgres.
var ShippedPromptTriggers = []ShippedPromptTrigger{
	{
		ID:                     "system-trigger-ci-fix",
		PromptID:               "system-ci-fix",
		EventType:              domain.EventGitHubPRCICheckFailed,
		Predicate:              `{"author_is_self":true}`,
		BreakerThreshold:       3,
		MinAutonomySuitability: 0.0,
	},
	{
		ID:                     "system-trigger-conflict-resolution",
		PromptID:               "system-conflict-resolution",
		EventType:              domain.EventGitHubPRConflicts,
		Predicate:              `{"author_is_self":true}`,
		BreakerThreshold:       2,
		MinAutonomySuitability: 0.0,
	},
	{
		ID:                     "system-trigger-jira-implement",
		PromptID:               "system-jira-implement",
		EventType:              domain.EventJiraIssueAssigned,
		Predicate:              `{"assignee_is_self":true}`,
		BreakerThreshold:       2,
		MinAutonomySuitability: 0.0,
	},
	{
		// Companion to system-trigger-jira-implement. A ticket that
		// had open subtasks on first poll suppresses assigned/available
		// and only emits became_atomic when the decomposition collapses
		// (SKY-173). Users who enable auto-implementation on assignment
		// almost certainly want the same behavior for this belated
		// signal — ship a parallel trigger rather than quietly drop
		// post-decomposition tickets on the floor.
		ID:                     "system-trigger-jira-implement-atomic",
		PromptID:               "system-jira-implement",
		EventType:              domain.EventJiraIssueBecameAtomic,
		Predicate:              `{"assignee_is_self":true}`,
		BreakerThreshold:       2,
		MinAutonomySuitability: 0.0,
	},
	{
		// Auto-review PRs when someone requests review. No predicate —
		// review_requested only fires when the session user is added
		// to the PR's review-request list, so the event is already
		// user-scoped at emit time.
		ID:                     "system-trigger-pr-review",
		PromptID:               "system-pr-review",
		EventType:              domain.EventGitHubPRReviewRequested,
		Predicate:              "",
		BreakerThreshold:       3,
		MinAutonomySuitability: 0.0,
	},
	{
		// Fix-review-feedback when changes are requested on the user's
		// own PR. Fires regardless of reviewer identity — self-review
		// and external reviewer response route through the same prompt
		// since the action is the same.
		ID:                     "system-trigger-fix-review-feedback",
		PromptID:               "system-fix-review-feedback",
		EventType:              domain.EventGitHubPRReviewChangesRequested,
		Predicate:              `{"author_is_self":true}`,
		BreakerThreshold:       3,
		MinAutonomySuitability: 0.0,
	},
}
