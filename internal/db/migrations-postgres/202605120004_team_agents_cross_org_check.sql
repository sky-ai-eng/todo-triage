-- +goose Up
-- SKY-260 review fixup: team_agents_insert + team_agents_update gate
-- on tf.user_in_team(team_id) only. Because team_agents.agent_id has
-- no compositional same-org constraint with team_id, a team member
-- who learns or guesses another org's agent UUID can INSERT a
-- (team_id=mine, agent_id=other-org-agent) row.
--
-- The downstream consequences are subtle but real:
--   - team_agents_select gates on user_in_team(team_id) → every member
--     of the abuser's team can read the foreign agent's row id.
--   - The future router (D-Claims SKY-261) will resolve a team's bot
--     by joining team_agents → agents; the cross-org row would route
--     work to the wrong tenant's bot.
--
-- The fix adds an EXISTS predicate that requires agents.org_id =
-- teams.org_id at write time. Defense in depth on top of the existing
-- user_in_team gate.
--
-- The original 202605120003 migration's policies are dropped and
-- recreated with the stricter shape rather than altered, because
-- Postgres has no ALTER POLICY ... ADD CHECK affordance.

DROP POLICY team_agents_insert ON team_agents;
DROP POLICY team_agents_update ON team_agents;

CREATE POLICY team_agents_insert ON team_agents FOR INSERT
  WITH CHECK (
    tf.user_in_team(team_id)
    AND EXISTS (
      SELECT 1 FROM teams t
      JOIN agents a ON a.org_id = t.org_id
      WHERE t.id = team_id
        AND a.id = agent_id
    )
  );

CREATE POLICY team_agents_update ON team_agents FOR UPDATE
  USING      (tf.user_in_team(team_id))
  WITH CHECK (
    tf.user_in_team(team_id)
    AND EXISTS (
      SELECT 1 FROM teams t
      JOIN agents a ON a.org_id = t.org_id
      WHERE t.id = team_id
        AND a.id = agent_id
    )
  );

-- +goose Down
SELECT 'down not supported';
