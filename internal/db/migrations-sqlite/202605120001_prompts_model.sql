-- +goose Up
-- Per-prompt model override. Stored as TEXT NOT NULL DEFAULT ''
-- (rather than nullable) for sqlite-scanning ergonomics; empty string
-- is the "inherit settings.AI.Model" sentinel resolved at dispatch.

ALTER TABLE prompts ADD COLUMN model TEXT NOT NULL DEFAULT '';

-- +goose Down
SELECT 'down not supported';
