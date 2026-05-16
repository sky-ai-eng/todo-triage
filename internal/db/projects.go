package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=ProjectStore --output=./mocks --case=underscore --with-expecter

// ProjectStore owns the projects table — user-curated work groupings
// (a Linear/Jira "project" mirrored locally, with pinned repos and
// the curator session that maintains the project's knowledge dir).
//
// All methods take orgID; local mode passes runmode.LocalDefaultOrgID.
// Create additionally takes teamID — projects are user-driven writes
// and the human picks which team owns the project at the Create UI;
// the store does not synthesize a team. creator_user_id is resolved
// from tf.current_user_id() set by WithTx (falling back to org owner
// only on the admin-pool test path where claims aren't set). SKY-294
// owns the broader team-selection UX work (per-page filter + write-
// time picker + sticky default).
//
// Postgres wires against the app pool — every consumer is request-
// equivalent (projects handler, curator, backfill, project_entities)
// or runs in a startup goroutine that already operates within the
// org's identity scope (projectclassify runner). RLS policies
// projects_select / projects_insert / projects_update / projects_delete
// gate every statement; org_id defense-in-depth fires alongside.
type ProjectStore interface {
	// Create inserts a new project and returns its id. If p.ID is
	// non-empty it's used verbatim; otherwise a uuid is generated.
	// PinnedRepos serializes to JSON (nil → []). teamID populates
	// team_id (required by the projects_team_visibility_requires_team
	// CHECK when the row defaults to visibility='team'); the SQLite
	// impl uses the local sentinel teamID, the Postgres impl binds
	// the caller-supplied value directly and refuses the SQLite
	// sentinel.
	Create(ctx context.Context, orgID, teamID string, p domain.Project) (string, error)

	// Get returns a project by id, or (nil, nil) if not found.
	Get(ctx context.Context, orgID, id string) (*domain.Project, error)

	// List returns all projects ordered by name (case-insensitive).
	// No pagination — counts stay small (≤100 in any plausible install).
	// Empty result returns []domain.Project{}, not nil.
	List(ctx context.Context, orgID string) ([]domain.Project, error)

	// Update writes the full mutable row from p (caller is responsible
	// for merging partial PATCH input over an existing row first).
	// updated_at is stamped server-side. created_at + creator_user_id
	// + team_id + visibility are preserved. Returns sql.ErrNoRows when
	// the project doesn't exist so handlers can map to 404.
	Update(ctx context.Context, orgID string, p domain.Project) error

	// Delete removes the project. The entities.project_id FK is
	// declared ON DELETE SET NULL so tagged entities become untagged
	// automatically — callers don't need to clear them first. Returns
	// sql.ErrNoRows when the project doesn't exist.
	//
	// On-disk knowledge artifacts (`~/.triagefactory/projects/<id>/`)
	// are NOT removed here — the handler owns that to keep this layer
	// pure DB. Same split as the rest of the codebase.
	Delete(ctx context.Context, orgID, id string) error

	// --- Admin-pool variants (`...System`) ---
	//
	// ListSystem mirrors List but routes through the admin pool in
	// Postgres. The consumer is the project classifier (SKY-297) —
	// a background goroutine spawned from main.go that pairs each
	// org's unclassified entities against that org's project set.
	// The classifier has no JWT-claims context, so the read needs
	// to bypass RLS the same way EntityStore.ListUnclassifiedSystem
	// does for the sibling read on the entity side. org_id stays in
	// the WHERE clause as defense in depth; behavior matches List.
	ListSystem(ctx context.Context, orgID string) ([]domain.Project, error)
}
