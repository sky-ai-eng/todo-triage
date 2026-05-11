package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=TeamAgentStore --output=./mocks --case=underscore --with-expecter

// TeamAgentStore owns team_agents — the per-team membership row for
// the org's agent, plus per-team config overrides. One row per
// (team_id, agent_id). Default-enabled at team creation by the
// bootstrap path; team members toggle + override post-creation.
//
// Audiences:
//
//   - Bootstrap (internal/db/bootstrap.go) — AddForTeam on org-create
//     (for each team that already exists) and on every subsequent
//     team-create handler call.
//   - D-Claims (SKY-261) router — GetForTeam to decide whether a
//     trigger fire creates a claimed task (enabled team) or falls
//     back to unclaimed (disabled team, prompt pre-filled).
//   - Future admin UI (SKY-257 / D14) — SetEnabled + SetOverrides
//     for per-team toggling. ListForOrg for the "per-team bot config"
//     table.
//
// # Pool split (Postgres)
//
//   - app pool — tf_app, RLS-active. Everything except AddForTeam.
//     team_agents_select plus the write policies
//     team_agents_insert/team_agents_update/team_agents_delete all
//     gate on tf.user_in_team(team_id), so team members can read and
//     write their own team's row but not other teams'. Per the locked
//     architecture decision: team-bot toggling is a team-member power,
//     not an admin-only power.
//   - admin pool — supabase_admin, BYPASSRLS. AddForTeam only. Same
//     reasoning as AgentStore.Create: bootstrap runs without claims.
//
// SQLite collapses both pools to one connection; assertLocalOrg pins
// orgID to LocalDefaultOrg.
type TeamAgentStore interface {
	// GetForTeam returns the row for (team_id, agent_id), or (nil, nil)
	// if absent. The router calls this on every trigger fire to gate
	// the claim-vs-unclaimed branch.
	GetForTeam(ctx context.Context, orgID, teamID, agentID string) (*domain.TeamAgent, error)

	// AddForTeam inserts a default-enabled membership row. Idempotent
	// on (team_id, agent_id) — duplicate calls leave the existing row
	// alone (no re-flipping Enabled). Bootstrap-only path; Postgres
	// routes through the admin pool.
	AddForTeam(ctx context.Context, orgID, teamID, agentID string) error

	// SetEnabled flips the bot on or off for a single team. Team-
	// member-writable per the locked architectural decision. App pool
	// in Postgres; RLS enforces team membership.
	SetEnabled(ctx context.Context, orgID, teamID, agentID string, enabled bool) error

	// SetOverrides writes per-team model + autonomy overrides. Nil
	// pointer / empty string clears the override and falls back to
	// the agent defaults. App pool in Postgres.
	SetOverrides(ctx context.Context, orgID, teamID, agentID string, model *string, autonomy *float64) error

	// Remove deletes the membership entirely. Rare path — usually the
	// caller wants SetEnabled(false) so the team_agents row persists
	// with its overrides intact. App pool in Postgres.
	Remove(ctx context.Context, orgID, teamID, agentID string) error

	// ListForOrg returns every team_agents row for the org's agent.
	// Used by the admin UI's per-team config table and by future
	// router optimization that wants the full set in memory.
	ListForOrg(ctx context.Context, orgID, agentID string) ([]domain.TeamAgent, error)
}

// LocalDefaultTeamID is the synthetic team id used in local mode where
// there's no real teams table. Mirrors runmode.LocalDefaultOrg's role
// for orgID: a fixed string that every team-scoped store call passes
// through in local mode so the SQLite impl's team_id column has a
// stable, recognizable value. Multi mode uses real UUIDs from the
// teams table and never sees this constant.
const LocalDefaultTeamID = "default"
