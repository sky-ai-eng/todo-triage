-- +goose Up
-- SKY-260 review fixup: pin team_agents reads + writes to the active
-- org context, and constrain agents.github_pat_user_id to the agent's
-- own org.
--
-- # team_agents
--
-- The original 202605120003 policies + 202605120004 same-org agents
-- check enforced "team member writes" and "team and agent share an
-- org," but did NOT pin the operation to tf.current_org_id(). A user
-- with memberships in multiple orgs could SELECT/UPDATE/DELETE
-- team_agents rows in org B while their JWT claims org_id = A, which
-- breaks the path-based tenancy pattern every other table uses
-- (compare teams_select / memberships_select — both join teams and
-- require t.org_id = tf.current_org_id()).
--
-- The fix adds a tf.team_in_current_org(team_id) helper and rebuilds
-- all four team_agents policies to require it on top of the existing
-- predicates.
--
-- # agents.github_pat_user_id
--
-- agents_insert / agents_update gate writes on tf.user_is_org_admin
-- (current_org pinned), but the github_pat_user_id column has no
-- intrinsic constraint that the referenced user belongs to the
-- agent's org. An org A admin could write
-- agents.github_pat_user_id = <bob-from-org-B> without RLS refusal.
-- Downstream credential lookup goes through the Vault wrappers which
-- gate on current_org_id (so cross-org PAT theft isn't reachable
-- this way), but the row's integrity is still wrong. Defense in
-- depth: refuse the write at policy time.

-- ===== helper =====
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tf.team_in_current_org(target_team UUID) RETURNS BOOLEAN
LANGUAGE SQL STABLE SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
  SELECT EXISTS (
    SELECT 1 FROM teams
    WHERE id = target_team
      AND org_id = tf.current_org_id()
  );
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION tf.team_in_current_org FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tf.team_in_current_org TO tf_app;

-- ===== team_agents policy rebuild =====

DROP POLICY team_agents_select ON team_agents;
DROP POLICY team_agents_insert ON team_agents;
DROP POLICY team_agents_update ON team_agents;
DROP POLICY team_agents_delete ON team_agents;

CREATE POLICY team_agents_select ON team_agents
  FOR SELECT TO tf_app
  USING (tf.team_in_current_org(team_id) AND tf.user_in_team(team_id));

CREATE POLICY team_agents_insert ON team_agents FOR INSERT
  WITH CHECK (
    tf.team_in_current_org(team_id)
    AND tf.user_in_team(team_id)
    -- The agents row referenced by agent_id must live in the active
    -- org. teams_in_current_org pins the team to current_org; this
    -- EXISTS pins the agent the same way without retracting the
    -- same-team-and-agent-org match from 202605120004.
    AND EXISTS (
      SELECT 1 FROM agents a
      WHERE a.id = agent_id
        AND a.org_id = tf.current_org_id()
    )
  );

CREATE POLICY team_agents_update ON team_agents FOR UPDATE
  USING (tf.team_in_current_org(team_id) AND tf.user_in_team(team_id))
  WITH CHECK (
    tf.team_in_current_org(team_id)
    AND tf.user_in_team(team_id)
    AND EXISTS (
      SELECT 1 FROM agents a
      WHERE a.id = agent_id
        AND a.org_id = tf.current_org_id()
    )
  );

CREATE POLICY team_agents_delete ON team_agents FOR DELETE
  USING (tf.team_in_current_org(team_id) AND tf.user_in_team(team_id));

-- ===== agents.github_pat_user_id integrity =====
-- The PAT-borrow user must be in the agent's org. NULL stays allowed
-- (most agents will use App install or no creds during early v1).

DROP POLICY agents_insert ON agents;
DROP POLICY agents_update ON agents;

CREATE POLICY agents_insert ON agents FOR INSERT
  WITH CHECK (
    org_id = tf.current_org_id()
    AND tf.user_is_org_admin(org_id)
    AND (
      github_pat_user_id IS NULL
      OR EXISTS (
        SELECT 1 FROM org_memberships
        WHERE user_id = github_pat_user_id
          AND org_id = agents.org_id
      )
    )
  );

CREATE POLICY agents_update ON agents FOR UPDATE
  USING      (org_id = tf.current_org_id() AND tf.user_is_org_admin(org_id))
  WITH CHECK (
    org_id = tf.current_org_id()
    AND tf.user_is_org_admin(org_id)
    AND (
      github_pat_user_id IS NULL
      OR EXISTS (
        SELECT 1 FROM org_memberships
        WHERE user_id = github_pat_user_id
          AND org_id = agents.org_id
      )
    )
  );

-- agents_select + agents_delete unchanged — they already gate on
-- (org_id = tf.current_org_id() AND tf.user_has_org_access / tf.user_is_org_admin).

-- +goose Down
SELECT 'down not supported';
