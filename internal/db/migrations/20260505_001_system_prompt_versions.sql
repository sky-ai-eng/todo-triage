-- Track shipped prompt versions and support safe in-place upgrades.
-- user_modified gates future prompt edits done by users; system reseeding
-- only updates untouched rows.

ALTER TABLE prompts
    ADD COLUMN user_modified INTEGER NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS system_prompt_versions (
    prompt_id TEXT PRIMARY KEY REFERENCES prompts(id) ON DELETE CASCADE,
    content_hash TEXT NOT NULL,
    applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
