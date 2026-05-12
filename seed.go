package main

import (
	"context"
	"database/sql"
	"log"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedDefaultPrompts seeds the shipped system prompts via PromptStore
// AND the shipped system rules + triggers via EventHandlerStore per org.
// Post-SKY-259 rules and triggers are one table; Seed iterates both
// kinds together. Local mode iterates a single synthetic org
// (runmode.LocalDefaultOrg); multi mode will iterate stores.Orgs.ListAll
// once Wave 4 lands OrgStore.
//
// Calls into:
//   - PromptStore.SeedOrUpdate — SQLite: single conn; Postgres: admin
//     pool internally (the system_prompt_versions sidecar is REVOKE'd
//     from tf_app per D3).
//   - EventHandlerStore.Seed — admin-pool routing in Postgres because
//     shipped rows have NULL creator_user_id + visibility='org' and
//     the modify policies gate org-visible writes on
//     tf.user_is_org_admin().
//
// Order matters: prompts seed first so the FK from
// event_handlers.prompt_id → prompts.id is satisfied for shipped
// trigger rows.
func seedDefaultPrompts(database *sql.DB, prompts db.PromptStore, handlers db.EventHandlerStore) {
	ctx := context.Background()

	// TODO(SKY-246 wave 4): when OrgStore lands, replace this hard-coded
	// slice with stores.Orgs.ListAll(ctx). Until then multi mode is
	// fatal at startup so the local-only slice is correct.
	orgs := []string{runmode.LocalDefaultOrg}

	shipped := []domain.Prompt{
		// Default PR review prompt — manual only. The user picks when
		// to review a PR; no automation makes sense for reviewing
		// (including reviewing one's own draft — that's just running
		// this prompt by hand).
		{ID: "system-pr-review", Name: "PR Code Review", Body: ai.PRReviewPromptTemplate, Source: "system"},

		// Merge conflict resolution prompt — auto-fired on merge
		// conflicts on the user's own PRs via the matching trigger
		// below.
		{ID: "system-conflict-resolution", Name: "Merge Conflict Resolution", Body: ai.ConflictResolutionPromptTemplate, Source: "system"},

		// CI fix prompt — auto-fired on CI failures via prompt_trigger.
		{ID: "system-ci-fix", Name: "CI Fix", Body: ai.CIFixPromptTemplate, Source: "system"},

		// Jira implementation prompt — auto-fired on issues assigned
		// to the user via the matching trigger below.
		{ID: "system-jira-implement", Name: "Jira Issue Implementation", Body: ai.JiraImplementPromptTemplate, Source: "system"},

		// Fix review feedback — fires on reviews landed on the user's
		// PRs. Same action regardless of whether the reviewer is the
		// user (self-review loop) or someone else (normal code
		// review): read the review, fix what's right, push back on
		// what isn't, push to branch.
		{ID: "system-fix-review-feedback", Name: "Fix Review Feedback", Body: ai.FixReviewFeedbackPromptTemplate, Source: "system"},

		// Default Curator spec-authorship skill (SKY-221). The Curator
		// materializes whichever prompt a project points at as a
		// literal Claude Code skill on each dispatch; new projects
		// start pointing at this one. Users override per-project via
		// the Projects page.
		{ID: domain.SystemTicketSpecPromptID, Name: "Curator: Ticket as a Spec", Body: ai.TicketSpecPromptTemplate, Source: "system"},
	}

	for _, orgID := range orgs {
		for _, p := range shipped {
			if err := prompts.SeedOrUpdate(ctx, orgID, p); err != nil {
				log.Printf("[seed] warning: failed to seed %s in org %s: %v", p.ID, orgID, err)
			}
		}
		// Event handlers (rules + triggers, post-SKY-259) ship after
		// prompts in the same orgID iteration so the FK from
		// event_handlers.prompt_id → prompts.id resolves in Postgres
		// (the constraint is composite on (prompt_id, org_id)).
		if err := handlers.Seed(ctx, orgID); err != nil {
			log.Printf("[seed] warning: failed to seed event_handlers in org %s: %v", orgID, err)
		}
	}
	// The shipped rule + trigger entries live in db.ShippedEventHandlers
	// (post-SKY-259 successor to ShippedTaskRules + ShippedPromptTriggers).
	// Edit that list to add or modify a shipped row.
}
