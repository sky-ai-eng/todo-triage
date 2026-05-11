-- +goose Up
-- SKY-260 follow-up: close the cross-tenancy gaps in baseline RLS that
-- the team_agents audit (202605120005) surfaced as a pattern.
--
-- The shape: write policies gated on org-blind helpers like
-- tf.user_is_team_admin / tf.user_is_org_admin_via_team /
-- tf.user_is_org_admin pass if the caller has the right role in ANY
-- org. A user with memberships in multiple orgs (e.g. a consultant
-- on both Acme and Globex) could write rows in org B while their JWT
-- claims org_id = A. The path-based tenancy convention every other
-- table uses (teams_*, org_settings_*, jira_rules_*, the resource
-- tables) pins to tf.current_org_id(); these four didn't.
--
-- Fixed in this migration:
--
--   1. users_select — currently uses tf.user_has_org_access(om.org_id)
--      which passes if the caller has any membership in that org.
--      Multi-org caller in Acme context could enumerate users from
--      Globex. The migration's own comment said the intent was "a user
--      in orgA never sees that orgB's users exist," so this is a
--      correctness fix matching the comment to the code.
--
--   2. memberships_{insert,update,delete} — gated on
--      tf.user_is_team_admin / tf.user_is_org_admin_via_team, both
--      org-blind. Multi-org admin could add/remove team members in
--      another org while operating from the current one. Wrap each
--      with tf.team_in_current_org(target_team) so the target team
--      must live in the active org.
--
--   3. org_memberships_{insert,update,delete} — gated on
--      tf.user_is_org_admin(org_memberships.org_id) which checks the
--      target row's org, not the active session's. Add the pin
--      org_memberships.org_id = tf.current_org_id(). The bootstrap
--      branch (founder self-inserts via tf.user_owns_org) is preserved
--      unchanged because claims aren't yet re-issued with the new
--      org_id at that moment.
--
--   4. team_settings_{select,insert,update,delete} — same shape as
--      memberships writes: gated on memberships membership /
--      tf.user_is_team_admin, both org-blind. Pin via
--      tf.team_in_current_org.
--
-- memberships_delete's self-leave branch (user_id = current_user_id)
-- stays org-blind because a user should always be able to leave a
-- team they belong to, regardless of which org their session is on.
-- Same for org_memberships_delete's self-leave branch.

-- ===== users_select =====

DROP POLICY users_select ON users;

CREATE POLICY users_select ON users FOR SELECT
  USING (
    id = tf.current_user_id()
    OR EXISTS (
      SELECT 1 FROM org_memberships om
      WHERE om.user_id = users.id
        AND om.org_id = tf.current_org_id()
        AND tf.user_has_org_access(om.org_id)
    )
  );

-- ===== memberships writes =====

DROP POLICY memberships_insert ON memberships;
DROP POLICY memberships_update ON memberships;
DROP POLICY memberships_delete ON memberships;

CREATE POLICY memberships_insert ON memberships FOR INSERT
  WITH CHECK (
    tf.team_in_current_org(memberships.team_id)
    AND (tf.user_is_team_admin(memberships.team_id)
         OR tf.user_is_org_admin_via_team(memberships.team_id))
  );

CREATE POLICY memberships_update ON memberships FOR UPDATE
  USING (
    tf.team_in_current_org(memberships.team_id)
    AND (tf.user_is_team_admin(memberships.team_id)
         OR tf.user_is_org_admin_via_team(memberships.team_id))
  )
  WITH CHECK (
    tf.team_in_current_org(memberships.team_id)
    AND (tf.user_is_team_admin(memberships.team_id)
         OR tf.user_is_org_admin_via_team(memberships.team_id))
  );

-- DELETE: self-leave stays org-blind (a member can leave a team
-- regardless of active session org). Admin branches are pinned.
CREATE POLICY memberships_delete ON memberships FOR DELETE
  USING (
    user_id = tf.current_user_id()
    OR (
      tf.team_in_current_org(memberships.team_id)
      AND (tf.user_is_team_admin(memberships.team_id)
           OR tf.user_is_org_admin_via_team(memberships.team_id))
    )
  );

-- ===== org_memberships writes =====

DROP POLICY org_memberships_insert ON org_memberships;
DROP POLICY org_memberships_update ON org_memberships;
DROP POLICY org_memberships_delete ON org_memberships;

CREATE POLICY org_memberships_insert ON org_memberships FOR INSERT
  WITH CHECK (
    -- Bootstrap: org founder self-inserts as 'owner' before claims
    -- are re-issued with the new org_id, so we can't require
    -- current_org_id matching here. user_owns_org is the safety net.
    (user_id = tf.current_user_id() AND tf.user_owns_org(org_memberships.org_id))
    -- Admin branch: must be operating in the target org's session.
    OR (org_memberships.org_id = tf.current_org_id()
        AND tf.user_is_org_admin(org_memberships.org_id))
  );

CREATE POLICY org_memberships_update ON org_memberships FOR UPDATE
  USING      (org_memberships.org_id = tf.current_org_id() AND tf.user_is_org_admin(org_memberships.org_id))
  WITH CHECK (org_memberships.org_id = tf.current_org_id() AND tf.user_is_org_admin(org_memberships.org_id));

-- DELETE: self-leave stays org-blind. Admin branch is pinned.
CREATE POLICY org_memberships_delete ON org_memberships FOR DELETE
  USING (
    user_id = tf.current_user_id()
    OR (org_memberships.org_id = tf.current_org_id()
        AND tf.user_is_org_admin(org_memberships.org_id))
  );

-- ===== team_settings =====

DROP POLICY team_settings_select ON team_settings;
DROP POLICY team_settings_insert ON team_settings;
DROP POLICY team_settings_update ON team_settings;
DROP POLICY team_settings_delete ON team_settings;

CREATE POLICY team_settings_select ON team_settings FOR SELECT
  USING (
    tf.team_in_current_org(team_settings.team_id)
    AND EXISTS (
      SELECT 1 FROM memberships m
      WHERE m.team_id = team_settings.team_id
        AND m.user_id = tf.current_user_id()
    )
  );

CREATE POLICY team_settings_insert ON team_settings FOR INSERT
  WITH CHECK (
    tf.team_in_current_org(team_settings.team_id)
    AND tf.user_is_team_admin(team_settings.team_id)
  );

CREATE POLICY team_settings_update ON team_settings FOR UPDATE
  USING      (tf.team_in_current_org(team_settings.team_id) AND tf.user_is_team_admin(team_settings.team_id))
  WITH CHECK (tf.team_in_current_org(team_settings.team_id) AND tf.user_is_team_admin(team_settings.team_id));

CREATE POLICY team_settings_delete ON team_settings FOR DELETE
  USING (tf.team_in_current_org(team_settings.team_id) AND tf.user_is_team_admin(team_settings.team_id));

-- +goose Down
SELECT 'down not supported';
