package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=PromptStore --output=./mocks --case=underscore --with-expecter

// PromptStore owns prompts + the system_prompt_versions sidecar that
// tracks shipped-content hashes. Three audiences:
//
//   - HTTP handlers (server/prompts_handler.go, server/triggers_handler.go,
//     server/projects.go) — full CRUD.
//   - Delegated agents (delegate/*, curator/skill.go) — read prompt body
//     before dispatch + bump usage_count.
//   - Startup seeder (seed.go) — SeedOrUpdate per shipped system prompt.
//   - Skills importer (skills/importer.go) — Get/Create/UpdateImported/Hide
//     to mirror local SKILL.md files into the prompts table.
//
// Postgres / RLS note: in multi mode, system_prompt_versions has
// INSERT/UPDATE/DELETE REVOKE'd from tf_app per D3 — only the deploy
// actor (supabase_admin) can write the sidecar. SeedOrUpdate is the
// one method that touches that sidecar, so the Postgres impl routes
// it to the admin pool internally; every other method runs on the
// app pool. SQLite has no role concept; both pools collapse to one
// connection and assertLocalOrg pins orgID to LocalDefaultOrg.
type PromptStore interface {
	// SeedOrUpdate inserts a shipped system prompt if missing or
	// updates it when the shipped (name, body, source) hash changed
	// since the last seed, skipping user-modified rows so local
	// customizations survive a re-seed. Recorded in
	// system_prompt_versions so identical re-seeds are no-ops (no
	// churn to prompts.updated_at — the UI orders by it).
	//
	// Atomic: prompts insert/update + system_prompt_versions upsert
	// happen in one transaction so the version row never races ahead
	// of the prompt row.
	//
	// Source contract: p.Source must be "" (defaulted to "system") or
	// "system". Non-system sources are rejected — SeedOrUpdate is the
	// shipped-content seeder; anything else (user prompts via the
	// HTTP handler, imported skills via the file importer) goes
	// through Create / UpdateImported. The version-sidecar would
	// otherwise track non-system rows and re-seeds could silently
	// overwrite them.
	//
	// Postgres-only: must run on the admin connection because
	// system_prompt_versions writes are REVOKE'd from tf_app. The
	// impl picks the right pool internally — callers don't (and
	// shouldn't) choose.
	SeedOrUpdate(ctx context.Context, orgID string, p domain.Prompt) error

	// List returns all non-hidden prompts ordered by updated_at DESC.
	List(ctx context.Context, orgID string) ([]domain.Prompt, error)

	// Get returns one prompt by id (regardless of hidden state) or
	// (nil, nil) if not found.
	Get(ctx context.Context, orgID string, id string) (*domain.Prompt, error)

	// Create inserts a new prompt (user or imported source).
	// Caller-provided ID — the handler generates UUIDs upstream.
	Create(ctx context.Context, orgID string, p domain.Prompt) error

	// Update changes name + body and stamps user_modified=true. The
	// flag tells SeedOrUpdate to leave the row alone on subsequent
	// shipped-content updates.
	Update(ctx context.Context, orgID string, id, name, body string) error

	// UpdateImported updates a re-imported skill's metadata + body
	// + allowed_tools WITHOUT setting user_modified, because the
	// change came from a file re-import not a user edit.
	UpdateImported(ctx context.Context, orgID string, id, name, body, allowedTools string) error

	// Delete hard-deletes a prompt + its bindings (FK CASCADE).
	// Handlers call this only for user-source prompts; system /
	// imported prompts go through Hide instead.
	Delete(ctx context.Context, orgID string, id string) error

	// Hide soft-deletes a prompt (hidden=true). Used for system /
	// imported prompts so they disappear from List but remain
	// available to historical runs that already reference them.
	Hide(ctx context.Context, orgID string, id string) error

	// Unhide reverses Hide.
	Unhide(ctx context.Context, orgID string, id string) error

	// IncrementUsage bumps usage_count by 1. Called from the
	// delegate spawner when a run picks the prompt; the count
	// drives the prompts page's sort heuristic.
	IncrementUsage(ctx context.Context, orgID string, id string) error

	// Stats aggregates runs.* for this prompt — totals, success
	// rate, cost, last-used, runs-per-day-for-30-days. The
	// underlying queries hit the runs table; logically a Run-side
	// concern but keyed on prompt_id, so it lives here so the
	// prompts handler can depend on a single store. When RunStore
	// lands (wave 3b) this stays put — the read still keys on
	// prompt_id and PromptStore is the right ownership root.
	Stats(ctx context.Context, orgID string, id string) (*domain.PromptStats, error)
}
