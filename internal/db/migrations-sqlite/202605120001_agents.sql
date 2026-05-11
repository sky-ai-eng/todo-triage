-- +goose Up
-- SKY-260 D-Agent: workload identity primitive.
--
-- Local-mode shape: no FK to orgs/teams/users — those tables don't exist
-- in the SQLite schema. Identity is pinned at the runtime layer via
-- assertLocalOrg in the SQLite store impls + LocalDefaultTeamID for the
-- synthetic team. The agents.id column is TEXT and stores a deterministic
-- UUID derived from db.BootstrapAgentID(orgID) so re-runs of bootstrap on
-- the same install land on the same row.
--
-- Both credential columns (github_app_installation_id, github_pat_user_id)
-- stay NULL in local mode. The keychain remains the source of truth for
-- the user's PAT; the agents row is metadata that becomes load-bearing
-- when D-Claims wires claimed_by_agent_id + actor_agent_id.

CREATE TABLE IF NOT EXISTS agents (
    id                              TEXT PRIMARY KEY,
    display_name                    TEXT NOT NULL DEFAULT 'Triage Factory Bot',
    default_model                   TEXT,
    default_autonomy_suitability    REAL,
    github_app_installation_id      TEXT,
    github_pat_user_id              TEXT,
    jira_service_account_id         TEXT,
    created_at                      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at                      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS team_agents (
    team_id                          TEXT NOT NULL,
    agent_id                         TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    enabled                          INTEGER NOT NULL DEFAULT 1,
    per_team_model                   TEXT,
    per_team_autonomy_suitability    REAL,
    added_at                         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (team_id, agent_id)
);

CREATE INDEX IF NOT EXISTS idx_team_agents_agent ON team_agents(agent_id);

-- +goose Down
SELECT 'down not supported';
