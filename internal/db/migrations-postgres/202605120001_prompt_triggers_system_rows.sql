-- +goose Up
-- SKY-246 TriggerStore: bring prompt_triggers in line with the
-- system-row pattern established for prompts + task_rules in
-- 202605110001:
--
--   - add `source` column (default 'user')
--   - allow creator_user_id IS NULL for system rows
--   - CHECK constraint pairs (source='system') ↔ (creator_user_id IS NULL)
--   - split the FOR ALL prompt_triggers_modify policy into separate
--     insert/update/delete so admin-only writes can gate `visibility='org'`
--     without leaking the rule to private/team writes
--
-- Same scope / rationale as docs for 202605110001 — see that file's header
-- for the full reasoning. This migration brings the third resource into
-- the same shape so the unification refactor (SKY-259) inherits a clean
-- template across both tables.

-- (1) source column -------------------------------------------------
ALTER TABLE prompt_triggers ADD COLUMN source TEXT NOT NULL DEFAULT 'user';

-- (2) Drop NOT NULL on creator_user_id ------------------------------
ALTER TABLE prompt_triggers ALTER COLUMN creator_user_id DROP NOT NULL;

-- (3) Backfill any pre-existing system rows -------------------------
-- Multi-mode Postgres has no production rows yet; this is purely defensive
-- against dev installs that ran the legacy seed path.
UPDATE prompt_triggers
   SET source = 'system',
       creator_user_id = NULL,
       visibility = 'org'
 WHERE id::text LIKE 'system-%';

-- (4) source ↔ creator_user_id coherence ----------------------------
ALTER TABLE prompt_triggers
  ADD CONSTRAINT prompt_triggers_system_has_no_creator
  CHECK ((source = 'system' AND creator_user_id IS NULL)
      OR (source = 'user'   AND creator_user_id IS NOT NULL));

-- (5) Split prompt_triggers_modify ----------------------------------
DROP POLICY prompt_triggers_modify ON prompt_triggers;

CREATE POLICY prompt_triggers_insert ON prompt_triggers FOR INSERT
  WITH CHECK (org_id = tf.current_org_id()
              AND tf.user_has_org_access(org_id)
              AND creator_user_id = tf.current_user_id());

-- UPDATE: own user-source row OR any org-visible row IFF caller is an
-- org admin. Same shape as task_rules + prompts updates from
-- 202605110001. Admin gate covers both TF-shipped system triggers
-- (source='system', creator NULL) and admin-authored org-shared
-- triggers (source='user', visibility='org').
CREATE POLICY prompt_triggers_update ON prompt_triggers FOR UPDATE
  USING (org_id = tf.current_org_id()
         AND ((creator_user_id = tf.current_user_id()
               AND tf.user_has_org_access(org_id))
              OR (visibility = 'org' AND tf.user_is_org_admin(org_id))))
  WITH CHECK (org_id = tf.current_org_id()
              AND ((creator_user_id = tf.current_user_id()
                    AND tf.user_has_org_access(org_id))
                   OR (visibility = 'org' AND tf.user_is_org_admin(org_id))));

-- DELETE: own user-source row only. Handler-level gate ensures system
-- triggers go through SetEnabled(false) instead, so the next boot's
-- Seed doesn't resurrect them.
CREATE POLICY prompt_triggers_delete ON prompt_triggers FOR DELETE
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND creator_user_id = tf.current_user_id());

-- +goose Down
SELECT 'down not supported';
