-- Foundational schema for the "preserve human verdict in agent memory"
-- workstream (SKY-204). Subsequent PRs (SKY-205 / SKY-206) populate
-- the new columns from the review-submit and discard/requeue paths;
-- this migration is read/write plumbing only — no behavior change for
-- end users.
--
-- Three things happen here:
--   (1) pending_reviews + pending_review_comments gain `original_*`
--       snapshots so the agent's draft survives user edits.
--   (2) run_memory is reshaped: `content` becomes `agent_content`
--       (now nullable), and a sibling `human_content` column is added
--       for the human verdict that follow-up PRs will write.
--   (3) runs.memory_missing — a denormalized flag that drifted from
--       run_memory ground truth — is dropped. The factory view
--       derives the same boolean from `(rm.agent_content IS NULL)`
--       via LEFT JOIN, eliminating the drift surface.

-- (1) Pending review snapshots ============================================
-- Both columns nullable: pre-existing rows have no draft to capture
-- (they were already saved before this migration), and the write-once
-- contract for going-forward rows is enforced in Go via COALESCE
-- (review submit) / explicit copy at insert (comments).
ALTER TABLE pending_reviews ADD COLUMN original_review_body TEXT;
ALTER TABLE pending_review_comments ADD COLUMN original_body TEXT;

-- (2) Reshape run_memory ==================================================
-- One-shot table rebuild instead of separate ALTERs because the
-- NOT NULL drop on the old `content` column already requires a rebuild
-- in SQLite (no in-place ALTER for constraint changes), so combining
-- the rename + nullability + new column is strictly cheaper than three
-- statements. Safe inside the migration runner's transaction because
-- nothing FKs into run_memory (verified) — the old → new switch
-- doesn't transiently break any inbound reference.
CREATE TABLE run_memory_new (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    entity_id TEXT NOT NULL REFERENCES entities(id),
    -- agent_content is nullable: row presence = "termination passed
    -- through the memory gate", agent_content IS NULL = "agent
    -- didn't comply." Pre-rebuild rows always had content (the old
    -- NOT NULL guarantee), so the INSERT-SELECT below carries them
    -- in unchanged.
    agent_content TEXT,
    -- human_content holds the user's verdict when they accept,
    -- edit, or discard the agent's draft. Stays NULL until SKY-205 /
    -- SKY-206 wire the writers; reads in this PR tolerate the NULL.
    human_content TEXT,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(run_id)
);

INSERT INTO run_memory_new (id, run_id, entity_id, agent_content, human_content, created_at)
    SELECT id, run_id, entity_id, content, NULL, created_at FROM run_memory;

DROP TABLE run_memory;
ALTER TABLE run_memory_new RENAME TO run_memory;

-- Indexes recreated post-rebuild (DROP TABLE removes them with the
-- old table). Same shape as baseline — kept here so a fresh install
-- replaying both migrations ends up with the identical index set.
CREATE INDEX IF NOT EXISTS idx_run_memory_entity_created ON run_memory(entity_id, created_at ASC);
CREATE INDEX IF NOT EXISTS idx_run_memory_run ON run_memory(run_id);

-- (3) Drop the denormalized memory_missing flag ===========================
-- Replaced by the JOIN-derived projection in factory.go and agent.go.
-- No index references this column (verified against baseline) so the
-- in-place DROP COLUMN is safe; SQLite ≥ 3.35 supports it without a
-- table rebuild.
ALTER TABLE runs DROP COLUMN memory_missing;
