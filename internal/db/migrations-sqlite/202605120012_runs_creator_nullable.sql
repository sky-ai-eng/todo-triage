-- +goose NO TRANSACTION
-- +goose Up
-- SKY-261 D-Claims follow-up: runs.creator_user_id becomes nullable.
--
-- Why
--
-- The pre-D-Claims baseline (after 202605120003_local_tenancy.sql added
-- the column) had `creator_user_id TEXT NOT NULL DEFAULT <sentinel>`.
-- For trigger-spawned runs the spawner relied on the DEFAULT to satisfy
-- NOT NULL, which baked the lie "the sentinel user created every auto-
-- trigger run" into the audit log. The spec wants NULL for trigger-
-- spawned runs (no human delegator) — the same shape the
-- 202605110001 / 202605120001 migrations cleaned up for task_rules /
-- prompts / prompt_triggers.
--
-- Rebuild path
--
-- SQLite doesn't support ALTER COLUMN DROP NOT NULL, so we rebuild
-- the runs table to drop NOT NULL on creator_user_id and add the
-- trigger_type ↔ creator_user_id pairing CHECK. Mirrors the pattern
-- used by 202605120008's prompts/runs rebuild and 202605120010's
-- tasks rebuild.

PRAGMA foreign_keys=OFF;

BEGIN;

-- Rebuild first (new table without NOT NULL on creator_user_id),
-- backfill trigger-spawned legacy rows to NULL in the INSERT SELECT,
-- then drop + rename. The CHECK below enforces the post-state.

CREATE TABLE runs_new (
    id                TEXT PRIMARY KEY,
    task_id           TEXT NOT NULL REFERENCES tasks(id),
    prompt_id         TEXT NOT NULL REFERENCES prompts(id),
    trigger_id        TEXT REFERENCES event_handlers(id),
    trigger_type      TEXT NOT NULL DEFAULT 'manual',
    status            TEXT NOT NULL DEFAULT 'cloning',
    model             TEXT,
    session_id        TEXT,
    worktree_path     TEXT,
    result_summary    TEXT,
    stop_reason       TEXT,
    started_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at      DATETIME,
    duration_ms       INTEGER,
    num_turns         INTEGER,
    total_cost_usd    REAL,
    org_id            TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    team_id           TEXT,
    -- creator_user_id is now nullable. DEFAULT preserved at the
    -- sentinel for callers that omit the column (legacy spawner
    -- paths, test fixtures) — those land trigger_type='manual'
    -- AND creator=sentinel, which the CHECK below accepts. The
    -- spawner explicitly passes NULL for trigger_type='event'.
    creator_user_id   TEXT DEFAULT '00000000-0000-0000-0000-000000000100' REFERENCES users(id),
    visibility        TEXT NOT NULL DEFAULT 'team'
        CHECK (visibility IN ('private','team','org')),
    actor_agent_id    TEXT REFERENCES agents(id) ON DELETE SET NULL,
    -- Pair trigger_type with creator_user_id nullability so the
    -- seeder can't drift back to lying. Same shape as the
    -- 202605120001 prompts source/creator CHECK.
    CHECK (
        (trigger_type = 'manual' AND creator_user_id IS NOT NULL)
        OR
        (trigger_type = 'event'  AND creator_user_id IS NULL)
    )
);

-- Backfill at insert time: trigger-spawned legacy rows get NULL
-- creator (audit honesty); manual rows keep theirs.
INSERT INTO runs_new (
    id, task_id, prompt_id, trigger_id, trigger_type, status, model,
    session_id, worktree_path, result_summary, stop_reason,
    started_at, completed_at, duration_ms, num_turns, total_cost_usd,
    org_id, team_id, creator_user_id, visibility, actor_agent_id
)
SELECT
    id, task_id, prompt_id, trigger_id, trigger_type, status, model,
    session_id, worktree_path, result_summary, stop_reason,
    started_at, completed_at, duration_ms, num_turns, total_cost_usd,
    org_id, team_id,
    CASE WHEN trigger_type = 'event' THEN NULL ELSE creator_user_id END,
    visibility, actor_agent_id
FROM runs;

DROP TABLE runs;

ALTER TABLE runs_new RENAME TO runs;

-- Recreate every index. Drop nukes ALL of them; forgetting one
-- silently regresses a query. Mirror the SKY-259 + D-Claims
-- recreate pattern.
CREATE INDEX IF NOT EXISTS idx_runs_task            ON runs(task_id);
CREATE INDEX IF NOT EXISTS idx_runs_prompt_started  ON runs(prompt_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_trigger         ON runs(trigger_id);
CREATE INDEX IF NOT EXISTS idx_runs_status          ON runs(status);
CREATE UNIQUE INDEX IF NOT EXISTS runs_id_org_unique ON runs (id, org_id);
CREATE INDEX IF NOT EXISTS runs_actor_agent_idx     ON runs(actor_agent_id)
    WHERE actor_agent_id IS NOT NULL;

COMMIT;

PRAGMA foreign_keys=ON;

-- +goose Down
SELECT 'down not supported';
