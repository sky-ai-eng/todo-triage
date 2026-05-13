-- +goose Up
-- SKY-261 D-Claims follow-up: runs.creator_user_id becomes nullable.
--
-- Why
--
-- The pre-D-Claims baseline had `creator_user_id UUID NOT NULL`. To
-- satisfy that for trigger-spawned runs (auto-fire with no human
-- requesting the work), the spawner / seeder COALESCE'd to the
-- LocalDefaultUserID sentinel (local) or the org owner (multi). The
-- audit log then claimed a human created every auto-trigger run —
-- the seeder was lying to the schema. Same shape SKY-262's
-- 202605110001_system_rows_nullable_creator.sql fixed for
-- task_rules / prompts / prompt_triggers / projects.
--
-- This migration carries the same fix to `runs`: trigger-spawned
-- runs land with `creator_user_id IS NULL` + `trigger_type = 'event'`;
-- manual runs keep their `creator_user_id = <whoever clicked>` +
-- `trigger_type = 'manual'`. A CHECK pairs the two so the seeder
-- can't drift back to lying.
--
-- RLS: the existing runs_select / runs_update / runs_delete policies
-- already handle creator_user_id IS NULL via the visibility='org'
-- branch (system rows ship visibility='org' + creator NULL). No
-- policy changes needed.

ALTER TABLE runs ALTER COLUMN creator_user_id DROP NOT NULL;

-- No DEFAULT in Postgres. Pre-filling a sentinel user_id on a real
-- multi-tenant deployment would attribute every default-insert to
-- a fake user that doesn't correspond to any real human (and may
-- not even satisfy the users(id) FK). Postgres has no production
-- rows yet — direct-SQL callers must supply creator_user_id
-- explicitly; the spawner does so based on trigger_type.

-- Backfill: pre-fix event-triggered runs carry creator_user_id =
-- LocalDefaultUserID (local) or the COALESCE'd org owner (multi) —
-- the seeder's lie. Wipe those to NULL so the CHECK below can land
-- without rejecting historical rows. Manual runs are unchanged.
-- Postgres has no production rows yet; this is a no-op in fresh
-- multi-mode deployments and a true backfill in local-mode upgrades
-- that share the same migration tree.
UPDATE runs SET creator_user_id = NULL WHERE trigger_type = 'event';

-- CHECK ties trigger_type and creator_user_id nullability together.
-- 'manual' runs MUST have a creator (a human clicked something);
-- 'event' runs MUST NOT (the auto-trigger fired without a human).
-- The trigger_type column has been present since baseline and is
-- written by every spawner path.
ALTER TABLE runs
  ADD CONSTRAINT runs_creator_matches_trigger_type CHECK (
    (trigger_type = 'manual' AND creator_user_id IS NOT NULL)
    OR
    (trigger_type = 'event'  AND creator_user_id IS NULL)
  );

-- +goose Down
SELECT 'down not supported';
