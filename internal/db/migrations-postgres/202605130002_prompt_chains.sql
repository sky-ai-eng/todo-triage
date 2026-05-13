-- +goose Up
-- Prompt chaining (multi-tenant mirror of the SQLite migration of the
-- same version). Linear sequences of prompt steps that share one
-- worktree; each step records a verdict to advance, abort, or finalize
-- the chain.
--
-- Schema parity notes vs the SQLite tree:
--   - All new tables are org-scoped with composite FKs `(child, org_id)`
--     against `prompts(id, org_id)`, `tasks(id, org_id)`,
--     `event_handlers(id, org_id)`, matching the defense-in-depth
--     pattern established in the baseline.
--   - chain_runs gets a creator_user_id (creator-scoped, like runs/
--     tasks) so RLS can apply the same predicate.
--   - prompt_chain_steps is a child of prompts; its RLS inherits the
--     chain prompt's visibility via EXISTS on prompts.
--   - runs.chain_run_id uses a composite FK so a run can only link a
--     chain_run in the same org.

ALTER TABLE prompts
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'leaf';

-- prompt_chain_steps ------------------------------------------------
-- Ordered membership list. ON DELETE CASCADE on chain_prompt_id wipes
-- the step list when the parent chain prompt is deleted; ON DELETE
-- RESTRICT on step_prompt_id stops a leaf prompt from being deleted
-- out from under a chain that references it. Both FKs are composite
-- against prompts(id, org_id) to prevent cross-tenant references.
CREATE TABLE prompt_chain_steps (
    org_id          UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    chain_prompt_id TEXT NOT NULL,
    step_index      INTEGER NOT NULL,
    step_prompt_id  TEXT NOT NULL,
    brief           TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, chain_prompt_id, step_index),
    FOREIGN KEY (chain_prompt_id, org_id) REFERENCES prompts(id, org_id) ON DELETE CASCADE,
    FOREIGN KEY (step_prompt_id, org_id)  REFERENCES prompts(id, org_id) ON DELETE RESTRICT
);

CREATE INDEX idx_prompt_chain_steps_step_prompt
    ON prompt_chain_steps(step_prompt_id, org_id);

-- chain_runs --------------------------------------------------------
-- Chain instance. Owns the worktree shared across every step and
-- carries chain-wide abort/cancel/complete state. Per-step state
-- stays on `runs` rows linked via runs.chain_run_id.
CREATE TABLE chain_runs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    creator_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    chain_prompt_id TEXT NOT NULL,
    task_id         UUID NOT NULL,
    trigger_type    TEXT NOT NULL,
    trigger_id      UUID,
    status          TEXT NOT NULL DEFAULT 'running'
                    CHECK (status IN ('running','completed','aborted','failed','cancelled')),
    abort_reason    TEXT,
    aborted_at_step INTEGER,
    worktree_path   TEXT NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ,
    UNIQUE (id, org_id),
    FOREIGN KEY (chain_prompt_id, org_id) REFERENCES prompts(id, org_id),
    FOREIGN KEY (task_id, org_id)         REFERENCES tasks(id, org_id),
    FOREIGN KEY (trigger_id, org_id)      REFERENCES event_handlers(id, org_id)
);

CREATE INDEX idx_chain_runs_task   ON chain_runs(task_id, org_id);
CREATE INDEX idx_chain_runs_status ON chain_runs(status) WHERE status = 'running';

-- runs.chain_run_id / chain_step_index ------------------------------
-- A run links back to its chain_run via composite FK so cross-tenant
-- linkage is structurally impossible.
ALTER TABLE runs ADD COLUMN chain_run_id     UUID;
ALTER TABLE runs ADD COLUMN chain_step_index INTEGER;
ALTER TABLE runs
    ADD CONSTRAINT runs_chain_run_fkey
    FOREIGN KEY (chain_run_id, org_id) REFERENCES chain_runs(id, org_id);

CREATE INDEX idx_runs_chain ON runs(chain_run_id, chain_step_index)
    WHERE chain_run_id IS NOT NULL;

-- RLS ---------------------------------------------------------------
ALTER TABLE prompt_chain_steps ENABLE ROW LEVEL SECURITY;
ALTER TABLE chain_runs         ENABLE ROW LEVEL SECURITY;

-- prompt_chain_steps inherits the chain prompt's visibility — if the
-- caller can't see the parent prompt, they can't see its step list.
-- prompts RLS already applies creator + team/org visibility rules.
CREATE POLICY prompt_chain_steps_all ON prompt_chain_steps FOR ALL
  USING      (EXISTS (SELECT 1 FROM prompts p WHERE p.id = prompt_chain_steps.chain_prompt_id))
  WITH CHECK (EXISTS (SELECT 1 FROM prompts p WHERE p.id = prompt_chain_steps.chain_prompt_id));

-- chain_runs are creator-scoped, same as runs/tasks. Org membership +
-- being the creator gates both read and write.
CREATE POLICY chain_runs_select ON chain_runs FOR SELECT
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND creator_user_id = tf.current_user_id());

CREATE POLICY chain_runs_modify ON chain_runs FOR ALL
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND creator_user_id = tf.current_user_id())
  WITH CHECK (org_id = tf.current_org_id()
              AND creator_user_id = tf.current_user_id()
              AND tf.user_has_org_access(org_id));

-- Grants. The baseline's `GRANT ON ALL TABLES IN SCHEMA public` only
-- covers tables that existed at baseline-run time; new tables added by
-- later migrations need explicit grants. See the baseline header note
-- about avoiding ALTER DEFAULT PRIVILEGES.
GRANT SELECT, INSERT, UPDATE, DELETE ON prompt_chain_steps TO tf_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON chain_runs         TO tf_app;

-- +goose Down
SELECT 'down not supported';
