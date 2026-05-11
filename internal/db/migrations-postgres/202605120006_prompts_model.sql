-- +goose Up
-- Per-prompt model override. Empty string = inherit settings.AI.Model
-- at dispatch (same sentinel as the sqlite migration).

ALTER TABLE prompts ADD COLUMN model TEXT NOT NULL DEFAULT '';

-- +goose Down
SELECT 'down not supported';
