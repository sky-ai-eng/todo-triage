-- +goose Up
-- Prompt chaining: linear sequences of prompt steps that share one
-- worktree, with each step able to record a verdict to abort the chain.
--
-- Design:
--   - prompts.kind distinguishes 'leaf' (the existing single-prompt
--     model) from 'chain' (an ordered list of leaf prompts). Triggers
--     fire prompts by id regardless of kind; only the spawner branches.
--   - prompt_chain_steps is the ordered membership list. ON DELETE
--     RESTRICT on step_prompt_id keeps a leaf prompt referenced by a
--     chain from being deleted out from under the chain (the UI
--     surfaces this as "used by N chain(s)").
--   - chain_runs is the chain instance. It owns the worktree shared
--     across every step and carries the abort/cancel/complete state
--     for the chain as a whole. Per-step state stays on runs rows
--     (one runs row per step, linked back via chain_run_id).
--   - run_artifacts(kind='chain:verdict') carries the per-step
--     proceed/abort verdict — no new column needed.

ALTER TABLE prompts
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'leaf';   -- 'leaf' | 'chain'

CREATE TABLE IF NOT EXISTS prompt_chain_steps (
    chain_prompt_id TEXT NOT NULL REFERENCES prompts(id) ON DELETE CASCADE,
    step_index      INTEGER NOT NULL,           -- 0-based; densely packed by ReplaceChainSteps
    step_prompt_id  TEXT NOT NULL REFERENCES prompts(id) ON DELETE RESTRICT,
    -- Author-supplied one-liner shown in the wrapper user prompt and
    -- in the run-detail UI. Falls back to step_prompt.name when empty.
    brief TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (chain_prompt_id, step_index)
);

CREATE INDEX IF NOT EXISTS idx_prompt_chain_steps_step_prompt
    ON prompt_chain_steps(step_prompt_id);

CREATE TABLE IF NOT EXISTS chain_runs (
    id              TEXT PRIMARY KEY,
    chain_prompt_id TEXT NOT NULL REFERENCES prompts(id),
    task_id         TEXT NOT NULL REFERENCES tasks(id),
    trigger_type    TEXT NOT NULL DEFAULT 'manual',
    trigger_id      TEXT REFERENCES event_handlers(id),
    -- 'running' | 'completed' | 'aborted' | 'failed' | 'cancelled'
    status          TEXT NOT NULL DEFAULT 'running',
    abort_reason    TEXT,
    aborted_at_step INTEGER,
    worktree_path   TEXT NOT NULL,
    started_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at    DATETIME
);

CREATE INDEX IF NOT EXISTS idx_chain_runs_task ON chain_runs(task_id);
CREATE INDEX IF NOT EXISTS idx_chain_runs_status ON chain_runs(status);

ALTER TABLE runs ADD COLUMN chain_run_id TEXT REFERENCES chain_runs(id);
ALTER TABLE runs ADD COLUMN chain_step_index INTEGER;

CREATE INDEX IF NOT EXISTS idx_runs_chain ON runs(chain_run_id, chain_step_index);

-- +goose Down
SELECT 'down not supported';
