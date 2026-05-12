-- +goose Up
-- SKY-261 D-Claims follow-up: drop status='claimed' / status='delegated'
-- denormalization. Mirrors the Postgres migration of the same name —
-- see that file's prologue for the orthogonal-axes rationale.

-- (1) claimed → queued + user claim from creator
UPDATE tasks
   SET status              = 'queued',
       claimed_by_user_id  = creator_user_id,
       claimed_by_agent_id = NULL
 WHERE status = 'claimed';

-- (2) delegated → queued + agent claim from the org's agent
-- (subquery falls back to NULL if no agent row exists yet)
UPDATE tasks
   SET status              = 'queued',
       claimed_by_user_id  = NULL,
       claimed_by_agent_id = (
         SELECT a.id FROM agents a WHERE a.org_id = tasks.org_id LIMIT 1
       )
 WHERE status = 'delegated';

-- (3) Backfill claim for tasks with pending pending_firings rows.
-- Drain (router.attemptDrainOne) now requires claimed_by_agent_id to
-- be set or it skips the firing as stale. Pre-upgrade firings are
-- legitimate commitments queued behind a busy entity; preserve them
-- by stamping the org's agent claim on the referenced task. NULL
-- claim cols on both sides means "no human took it either" — safe
-- to stamp.
UPDATE tasks
   SET claimed_by_agent_id = (
         SELECT a.id FROM agents a WHERE a.org_id = tasks.org_id LIMIT 1
       )
 WHERE claimed_by_agent_id IS NULL
   AND claimed_by_user_id  IS NULL
   AND EXISTS (
       SELECT 1 FROM pending_firings pf
        WHERE pf.task_id = tasks.id
          AND pf.org_id  = tasks.org_id
          AND pf.status  = 'pending'
   );

-- +goose Down
SELECT 'down not supported';
