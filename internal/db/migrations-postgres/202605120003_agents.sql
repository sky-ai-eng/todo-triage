-- +goose Up
-- SKY-260 D-Agent: workload-identity primitive.
--
-- Two new tables (agents, team_agents), one new RLS helper
-- (tf.user_in_team), four policies. Adds the schema delta the
-- multi-tenant architecture doc has been describing under "agent
-- identity" — see docs/multi-tenant-architecture.html §2 tenant model,
-- §7 schema, §13 D-Agent.
--
-- The agents row is the bot's first-class identity, distinct from the
-- users domain (which mirrors auth.users). Tasks reference agents via
-- claimed_by_agent_id (added by D-Claims / SKY-261); runs reference
-- via actor_agent_id (same ticket). This migration only adds the
-- tables + their RLS — the FK columns on tasks/runs land later.
--
-- # Why agents has no org-scoped composite PK
--
-- Every other team-scoped table in the baseline ships with a composite
-- (id, org_id) PK pattern so cross-org FK references can't sneak past
-- RLS. agents instead uses (id) alone + UNIQUE(org_id). Rationale: the
-- only inbound FK is "this org has one bot," not "this row points at an
-- agent in some org" — so we never compose (agent_id, org_id) at the
-- caller. UNIQUE(org_id) + ON CONFLICT DO NOTHING gives idempotent
-- bootstrap; the per-row org_id column is still present for RLS gates.
--
-- # Pool routing
--
-- agents.Create routes through the admin pool (BYPASSRLS) because at
-- org-create time the founder has no org_memberships row yet — the
-- agents row inserts in the same transaction as their first membership.
-- All reads + non-bootstrap writes route through the app pool and rely
-- on the policies below.

-- ===== agents =====

CREATE TABLE agents (
  id                              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id                          UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  display_name                    TEXT NOT NULL DEFAULT 'Triage Factory Bot',
  default_model                   TEXT,
  default_autonomy_suitability    REAL,
  github_app_installation_id      TEXT,
  github_pat_user_id              UUID REFERENCES users(id) ON DELETE SET NULL,
  jira_service_account_id         TEXT,
  created_at                      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at                      TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id)
);

CREATE INDEX agents_org_idx ON agents(org_id);

-- ===== team_agents =====

CREATE TABLE team_agents (
  team_id                          UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
  agent_id                         UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  enabled                          BOOLEAN NOT NULL DEFAULT TRUE,
  per_team_model                   TEXT,
  per_team_autonomy_suitability    REAL,
  added_at                         TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (team_id, agent_id)
);

CREATE INDEX team_agents_team_idx  ON team_agents(team_id);
CREATE INDEX team_agents_agent_idx ON team_agents(agent_id);

-- ===== updated_at trigger on agents =====

CREATE TRIGGER set_updated_at BEFORE UPDATE ON agents
  FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();

-- ===== tf.user_in_team helper =====
-- Returns TRUE iff the caller has ANY memberships row for target_team
-- (admin/member/viewer all qualify). Distinct from tf.user_is_team_admin
-- (admin role only). team_agents RLS uses this; D-TeamDefault (SKY-262)
-- will reuse it across the broader per-resource policy sweep.
--
-- SECURITY DEFINER so the lookup bypasses memberships' own RLS —
-- otherwise the memberships SELECT policy would call this helper which
-- would in turn evaluate the policy: infinite recursion. Same pattern
-- as tf.user_has_org_access vs org_memberships.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tf.user_in_team(target_team UUID) RETURNS BOOLEAN
LANGUAGE SQL STABLE SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
  SELECT EXISTS (
    SELECT 1 FROM memberships
    WHERE user_id = tf.current_user_id()
      AND team_id = target_team
  );
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION tf.user_in_team FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tf.user_in_team TO tf_app;

-- ===== RLS: agents =====

ALTER TABLE agents ENABLE ROW LEVEL SECURITY;

-- Any org member can read "who is our bot." Display name + default
-- model + credential-source presence are all surfaced in the human
-- triage UI ("the bot will run via the GitHub App"), so SELECT is
-- intentionally permissive within the org.
CREATE POLICY agents_select ON agents
  FOR SELECT TO tf_app
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id));

-- Writes are org-admin only. Bot rename, model change, credential
-- rotation are sensitive — they affect every team. Bootstrap bypasses
-- via the admin pool (BYPASSRLS) because at org-create the founder
-- isn't yet a member per RLS's lookup.
CREATE POLICY agents_insert ON agents FOR INSERT
  WITH CHECK (org_id = tf.current_org_id()
              AND tf.user_is_org_admin(org_id));

CREATE POLICY agents_update ON agents FOR UPDATE
  USING      (org_id = tf.current_org_id() AND tf.user_is_org_admin(org_id))
  WITH CHECK (org_id = tf.current_org_id() AND tf.user_is_org_admin(org_id));

CREATE POLICY agents_delete ON agents FOR DELETE
  USING (org_id = tf.current_org_id() AND tf.user_is_org_admin(org_id));

-- ===== RLS: team_agents =====

ALTER TABLE team_agents ENABLE ROW LEVEL SECURITY;

-- Any team member can read + write their own team's row. Per the
-- locked architectural decision (docs/multi-tenant-architecture.html
-- decision "agent-identity"), bot enable/disable is a team-member
-- power, not an admin-only power — the team owns its own queue, and
-- the bot's per-team participation is part of that.
--
-- Cross-team writes are blocked because tf.user_in_team gates on the
-- team_id of the row being written; an attempt to UPDATE another
-- team's row fails the USING clause and silently drops.
CREATE POLICY team_agents_select ON team_agents
  FOR SELECT TO tf_app
  USING (tf.user_in_team(team_id));

CREATE POLICY team_agents_insert ON team_agents FOR INSERT
  WITH CHECK (tf.user_in_team(team_id));

CREATE POLICY team_agents_update ON team_agents FOR UPDATE
  USING      (tf.user_in_team(team_id))
  WITH CHECK (tf.user_in_team(team_id));

CREATE POLICY team_agents_delete ON team_agents FOR DELETE
  USING (tf.user_in_team(team_id));

-- ===== tf_app grants =====
-- Targeted grants on the two new tables. The bulk
-- "GRANT ... ON ALL TABLES IN SCHEMA public" pattern from the baseline
-- migration would clobber the deliberate revokes on system_prompt_versions
-- and events_catalog (write privileges are deploy-actor-only on those
-- tables, see baseline §0). Targeted grants here add the new tables
-- without touching the existing revoke surface.
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE agents      TO tf_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE team_agents TO tf_app;

-- +goose Down
SELECT 'down not supported';
