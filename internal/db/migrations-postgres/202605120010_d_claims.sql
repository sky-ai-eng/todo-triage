-- +goose Up
-- SKY-261 D-Claims: claim flow on tasks + runs.
--
-- Adds three nullable FK columns and one CHECK constraint:
--   tasks.claimed_by_agent_id  → agents(id, org_id)  (composite FK PG)
--   tasks.claimed_by_user_id   → users(id)
--   runs.actor_agent_id        → agents(id, org_id)  (composite FK PG)
--
-- XOR CHECK on tasks: at most one of (claimed_by_agent_id,
-- claimed_by_user_id) can be set on a row. Both NULL = unclaimed (in
-- the team queue). Exactly one set = the entity currently responsible.
-- Both set = forbidden.
--
-- Parity prereq: UNIQUE(id, org_id) on agents so the composite FKs
-- target a unique constraint as Postgres requires. SKY-260 explicitly
-- skipped this because no caller composed (agent_id, org_id) — this
-- ticket is where that rationale stops holding.
--
-- See docs/specs/sky-261-d-claims.html for the full design + the
-- §4 state-transition matrix.

-- ============================================================================
-- (1) Agents parity: composite UNIQUE so the agent-id FKs can be composite.
-- ============================================================================
-- id is already PK-unique, so adding (id, org_id) UNIQUE is trivially
-- satisfied by existing data — one row per id, no values to migrate.

ALTER TABLE agents ADD CONSTRAINT agents_id_org_unique UNIQUE (id, org_id);

-- ============================================================================
-- (2) tasks.claimed_by_agent_id + tasks.claimed_by_user_id + XOR CHECK
-- ============================================================================
-- Both columns nullable. ON DELETE SET NULL on both: deletion of the
-- referenced agent or user clears the claim on existing rows but
-- preserves the row itself (audit shape: "the task existed, but its
-- claim pointer outlived its target"). Pair with a JOIN for display
-- name; if the JOIN turns up empty, the UI renders "—".

ALTER TABLE tasks
  ADD COLUMN claimed_by_agent_id UUID,
  ADD CONSTRAINT tasks_claimed_agent_fkey
    FOREIGN KEY (claimed_by_agent_id, org_id)
    REFERENCES agents (id, org_id) ON DELETE SET NULL;

ALTER TABLE tasks
  ADD COLUMN claimed_by_user_id UUID REFERENCES users(id) ON DELETE SET NULL;

-- XOR: at most one claimant. NULL/NULL is the unclaimed-in-queue state.
ALTER TABLE tasks ADD CONSTRAINT tasks_claim_xor
  CHECK (claimed_by_agent_id IS NULL OR claimed_by_user_id IS NULL);

-- Partial indexes for the Board's per-member filter hot read.
CREATE INDEX tasks_claimed_agent_idx ON tasks(claimed_by_agent_id)
  WHERE claimed_by_agent_id IS NOT NULL;
CREATE INDEX tasks_claimed_user_idx ON tasks(claimed_by_user_id)
  WHERE claimed_by_user_id IS NOT NULL;

-- ============================================================================
-- (3) runs.actor_agent_id — immutable per-run audit pointer
-- ============================================================================
-- Stamped by the spawner at run start. Survives later config edits
-- (e.g., admin re-claims a trigger to a different agent — the in-
-- flight run keeps its original actor). Unlike the task columns, this
-- one isn't expected to flip: once stamped, never re-stamped.

ALTER TABLE runs
  ADD COLUMN actor_agent_id UUID,
  ADD CONSTRAINT runs_actor_agent_fkey
    FOREIGN KEY (actor_agent_id, org_id)
    REFERENCES agents (id, org_id) ON DELETE SET NULL;

CREATE INDEX runs_actor_agent_idx ON runs(actor_agent_id)
  WHERE actor_agent_id IS NOT NULL;

-- No RLS changes — the existing tasks + runs policies (post-SKY-262)
-- already gate on team membership / org access, and the new columns
-- inherit the row's RLS automatically.

-- +goose Down
SELECT 'down not supported';
