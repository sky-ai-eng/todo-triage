package db

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Bootstrap functions for agent identity (SKY-260 D-Agent).
//
// All three entry points share one property: they're idempotent. The
// org / team / local bootstrap can run at every startup or every
// handler call without changing user state — INSERT-OR-IGNORE
// semantics live in the AgentStore + TeamAgentStore impls.
//
// Callers:
//
//   - BootstrapLocalAgent — main.go startup, runs alongside
//     seedDefaultPrompts. Existing v1.10.1 → current installs pick up
//     the agents + team_agents rows on first post-upgrade boot.
//   - BootstrapAgentForOrg — multi-mode org-create handler (D7
//     SKY-251), runs after the orgs row inserts + before any team is
//     created for that org.
//   - BootstrapTeamAgent — multi-mode team-create handler (also D7),
//     runs after each new teams row inserts.

// BootstrapLocalAgent inserts the synthetic local-mode agent + the
// local-mode team_agents row. Safe to call at every startup. Returns
// nil if the rows already exist; returns the underlying store error
// otherwise.
//
// In local mode the agent's credential FKs stay NULL — the PAT lives
// in the OS keychain, not in agents.github_pat_user_id (there's no
// users table to FK into). The agents row is metadata that becomes
// load-bearing when D-Claims wires claimed_by_agent_id +
// actor_agent_id; until then, the spawner continues to read PATs
// from the keychain directly.
func BootstrapLocalAgent(ctx context.Context, stores Stores) error {
	agentID, err := stores.Agents.Create(ctx, runmode.LocalDefaultOrg, domain.Agent{
		DisplayName: "Triage Factory Bot",
	})
	if err != nil {
		return fmt.Errorf("bootstrap local agent: %w", err)
	}
	if err := stores.TeamAgents.AddForTeam(ctx, runmode.LocalDefaultOrg, LocalDefaultTeamID, agentID); err != nil {
		return fmt.Errorf("bootstrap local team_agents: %w", err)
	}
	return nil
}

// BootstrapAgentForOrg inserts the org's single agents row. Called by
// the org-create handler (D7 SKY-251) right after the orgs row +
// founder's org_memberships row insert. Returns the agents.id so the
// caller can immediately stamp it into the first team via
// BootstrapTeamAgent.
//
// Routes through the admin pool in the Postgres impl because the
// founder isn't yet an admin per RLS's view at this moment (their
// org_memberships row is in the same transaction).
func BootstrapAgentForOrg(ctx context.Context, stores Stores, orgID string) (string, error) {
	agentID, err := stores.Agents.Create(ctx, orgID, domain.Agent{
		DisplayName: "Triage Factory Bot",
	})
	if err != nil {
		return "", fmt.Errorf("bootstrap agent for org %s: %w", orgID, err)
	}
	return agentID, nil
}

// BootstrapTeamAgent inserts a default-enabled team_agents row for the
// given team. Called by the team-create handler (D7) and by
// org-create after BootstrapAgentForOrg returns. Errors if the org
// has no agent row yet — calling team-bootstrap before org-bootstrap
// is a sequencing bug in the caller and silent-skip would leave teams
// with no bot membership, which is surprising to debug after the fact.
func BootstrapTeamAgent(ctx context.Context, stores Stores, orgID, teamID string) error {
	agent, err := stores.Agents.GetForOrg(ctx, orgID)
	if err != nil {
		return fmt.Errorf("bootstrap team_agents: lookup agent for org %s: %w", orgID, err)
	}
	if agent == nil {
		return fmt.Errorf("bootstrap team_agents: org %s has no agent — call BootstrapAgentForOrg first", orgID)
	}
	if err := stores.TeamAgents.AddForTeam(ctx, orgID, teamID, agent.ID); err != nil {
		return fmt.Errorf("bootstrap team_agents: %w", err)
	}
	return nil
}
