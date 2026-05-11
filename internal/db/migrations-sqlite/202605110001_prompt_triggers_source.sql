-- +goose Up
-- SKY-246 TriggerStore: add `source` column to prompt_triggers so the
-- system-row pattern (shipped triggers, admin-managed) is expressed the
-- same way it is on prompts and task_rules. Without this column, "is
-- this a system row?" would have to be inferred from id-prefix string
-- matching, which is fragile + inconsistent across backends.
--
-- All existing rows (any pre-existing system-trigger seed via the legacy
-- INSERT OR IGNORE path) get source='system' if their id starts with
-- 'system-' to match what the new Seed code would have written; every
-- other row defaults to 'user' which is the right reading for anything
-- the user created via the API.

ALTER TABLE prompt_triggers ADD COLUMN source TEXT NOT NULL DEFAULT 'user';

UPDATE prompt_triggers
   SET source = 'system'
 WHERE id LIKE 'system-%';

-- +goose Down
SELECT 'down not supported';
