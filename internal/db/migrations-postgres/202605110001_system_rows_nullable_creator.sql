-- +goose Up
-- SKY-246 follow-up: stop forcing system-shipped rows to claim a
-- creator they don't have.
--
-- Pre-existing shape on `task_rules` + `prompts`:
--   - creator_user_id UUID NOT NULL REFERENCES users(id)
--   - Seeders (TaskRuleStore.Seed, PromptStore.SeedOrUpdate) ran a
--     COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $1))
--     to satisfy the NOT NULL — i.e., the seeder lied to the schema
--     and claimed the org founder authored every shipped row.
--   - The downstream `xxx_modify` RLS policy required
--     creator_user_id = tf.current_user_id() for any write, so only
--     the org owner could actually disable a shipped rule. Every
--     other org member was structurally locked out of toggling
--     shipped defaults.
--
-- New shape:
--   - creator_user_id is NULL for system-shipped rows; non-NULL for
--     user-created rows. A CHECK constraint pairs `source` with
--     `creator_user_id` so the seeder can't drift back to lying.
--   - System rows ship with visibility = 'org' so the unchanged
--     xxx_select policy (which already has an `OR visibility = 'org'`
--     branch) lets every org member read them without coupling to
--     creator identity.
--   - The xxx_modify policy is split into separate insert / update /
--     delete policies so we can grant org-wide UPDATE on system rows
--     (needed to disable a shipped default) without granting org-wide
--     INSERT or DELETE (which would let any tf_app caller forge or
--     destroy system rows).
--
-- Scope: this migration touches `task_rules` and `prompts` because
-- both have shipping seeders that hit this issue today. `prompt_triggers`
-- has the same disease (system triggers ship disabled per spec) but
-- TriggerStore lands in a separate PR and will get the same treatment
-- there — keeping schema changes co-located with the store work that
-- exercises them. `projects` / `runs` / etc. have no system-shipped
-- rows so they're unaffected.

-- (1) Drop NOT NULL ------------------------------------------------
ALTER TABLE task_rules ALTER COLUMN creator_user_id DROP NOT NULL;
ALTER TABLE prompts    ALTER COLUMN creator_user_id DROP NOT NULL;

-- (2) Backfill any existing system rows -----------------------------
-- Pre-existing test/dev installs may have system rows with the org
-- owner stamped as creator from the COALESCE path. Reset those to
-- the new shape BEFORE the CHECK constraint goes on, otherwise the
-- ALTER would fail on legacy data. Multi-tenant Postgres is not yet
-- shipped to production so this is exclusively about dev installs.
UPDATE task_rules SET creator_user_id = NULL, visibility = 'org' WHERE source = 'system';
UPDATE prompts    SET creator_user_id = NULL, visibility = 'org' WHERE source = 'system';

-- (3) source ↔ creator_user_id coherence ----------------------------
-- task_rules.source values: 'system' | 'user'.
ALTER TABLE task_rules
  ADD CONSTRAINT task_rules_system_has_no_creator
  CHECK ((source = 'system' AND creator_user_id IS NULL)
      OR (source = 'user'   AND creator_user_id IS NOT NULL));

-- prompts.source values: 'system' | 'user' | 'imported'.
-- Only 'system' is claims-less; 'imported' rows still record the
-- user who triggered the import (today: the deploy-time / boot-time
-- actor — same behavior as before this migration).
ALTER TABLE prompts
  ADD CONSTRAINT prompts_system_has_no_creator
  CHECK ((source = 'system'  AND creator_user_id IS NULL)
      OR (source <> 'system' AND creator_user_id IS NOT NULL));

-- (4) Split task_rules write policy ---------------------------------
DROP POLICY task_rules_modify ON task_rules;

-- INSERT: only a real user creating their own user-source rule.
-- System rows are inserted by the deploy-time actor (BYPASSRLS),
-- which doesn't go through this policy at all. tf_app calls can
-- never produce a NULL creator because the WITH CHECK requires
-- creator_user_id = tf.current_user_id() which is itself non-NULL
-- (NULL claim means no session, which fails the eq check).
CREATE POLICY task_rules_insert ON task_rules FOR INSERT
  WITH CHECK (org_id = tf.current_org_id()
              AND tf.user_has_org_access(org_id)
              AND creator_user_id = tf.current_user_id());

-- UPDATE: own user-source row OR any system+org-visible row.
-- The system branch lets any org member disable a shipped default
-- (the current product behavior that the old policy was structurally
-- denying to non-owners). The CHECK constraint above pins
-- creator_user_id NULL ↔ source='system', so this clause can't be
-- abused to mutate a user row into a system row (the CHECK would
-- reject the resulting state).
CREATE POLICY task_rules_update ON task_rules FOR UPDATE
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND (creator_user_id = tf.current_user_id()
              OR (source = 'system' AND visibility = 'org')))
  WITH CHECK (org_id = tf.current_org_id()
              AND tf.user_has_org_access(org_id)
              AND (creator_user_id = tf.current_user_id()
                   OR (source = 'system' AND visibility = 'org')));

-- DELETE: own user-source row only. Handlers route system-row
-- deletes to SetEnabled(false) — making the structural denial here
-- the safety net rather than the primary gate.
CREATE POLICY task_rules_delete ON task_rules FOR DELETE
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND creator_user_id = tf.current_user_id());

-- (5) Split prompts write policy ------------------------------------
DROP POLICY prompts_modify ON prompts;

CREATE POLICY prompts_insert ON prompts FOR INSERT
  WITH CHECK (org_id = tf.current_org_id()
              AND tf.user_has_org_access(org_id)
              AND creator_user_id = tf.current_user_id());

CREATE POLICY prompts_update ON prompts FOR UPDATE
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND (creator_user_id = tf.current_user_id()
              OR (source = 'system' AND visibility = 'org')))
  WITH CHECK (org_id = tf.current_org_id()
              AND tf.user_has_org_access(org_id)
              AND (creator_user_id = tf.current_user_id()
                   OR (source = 'system' AND visibility = 'org')));

CREATE POLICY prompts_delete ON prompts FOR DELETE
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND creator_user_id = tf.current_user_id());

-- +goose Down
SELECT 'down not supported';
