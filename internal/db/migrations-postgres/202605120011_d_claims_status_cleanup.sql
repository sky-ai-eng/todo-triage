-- +goose Up
-- SKY-261 D-Claims follow-up: drop status='claimed' / status='delegated'
-- denormalization. After SKY-261's claim columns shipped, those two
-- status values became redundant — both encode "who's responsible"
-- which the claim columns already track. This migration backfills
-- existing rows so the responsibility axis lives only on the claim
-- columns going forward; the status enum collapses to genuine
-- lifecycle states (queued, snoozed, done, dismissed).
--
-- Writers stop emitting 'claimed' / 'delegated' as new status values
-- in the same PR (RecordSwipe + fireDelegate edits); this migration
-- handles the rows that exist today. No CHECK constraint is added
-- to forbid those values — keeping them as historical enum values
-- (just no-longer-written) means a query against an unmigrated
-- replica or a future audit-log restore doesn't break on the
-- constraint. Forward-only by convention.
--
-- See docs/specs/sky-261-d-claims.html v0.6 for the orthogonal-axes
-- framing this enforces.

-- ============================================================================
-- (1) status='claimed' → status='queued' + claimed_by_user_id = creator
-- ============================================================================
-- Claimed rows ARE user-claimed by definition. The creator is the
-- natural owner for a self-claim in the absence of explicit handoff
-- audit, and matches local mode's N=1 case (creator == user).

UPDATE tasks
   SET status              = 'queued',
       claimed_by_user_id  = creator_user_id,
       claimed_by_agent_id = NULL
 WHERE status = 'claimed';

-- ============================================================================
-- (2) status='delegated' → status='queued' + claimed_by_agent_id = org's agent
-- ============================================================================
-- Delegated rows ARE bot-claimed by definition. Look up the org's
-- agent row; if missing (pre-SKY-260 install carried over), leave
-- claim_by_agent_id NULL — the row stays effectively unclaimed,
-- which is a degraded but consistent state.

UPDATE tasks t
   SET status              = 'queued',
       claimed_by_user_id  = NULL,
       claimed_by_agent_id = (SELECT a.id FROM agents a WHERE a.org_id = t.org_id LIMIT 1)
 WHERE t.status = 'delegated';

-- ============================================================================
-- (3) Backfill claim for drain-eligible tasks with pending firings
-- ============================================================================
-- attemptDrainOne (internal/routing/router.go) now refuses to fire a
-- pending firing unless the task carries a bot claim — that's the
-- post-B+ invariant: "drain reflects current commitment." On upgrade,
-- pre-existing pending_firings reference tasks that were status='queued'
-- pre-cleanup (the firing was queued because the entity was busy, not
-- because the task had moved to 'delegated'). Steps (1) and (2) only
-- migrate the already-claimed/delegated rows; queued tasks with a
-- pending firing waiting on them keep claim NULL and the drain path
-- would silently mark every legacy firing 'skipped_stale' on first
-- post-upgrade boot.
--
-- The firing's existence IS the commitment — "the bot already committed
-- to this task; it's just waiting for an entity slot." Stamp the
-- agent claim on those tasks so the drainer fires them correctly.
-- Idempotent against re-runs: NULL guard on claim cols, ANY-pending
-- guard on the firing side, falls back gracefully when the agents
-- table is empty (claim stays NULL → drainer still skips, same as
-- before — no regression).
--
-- Status guard: only stamp when the task is currently drain-eligible
-- (status='queued'). A task that landed in done/dismissed/snoozed
-- since the firing was enqueued is no longer a valid target — the
-- drainer would skip it via the task_closed branch anyway, and
-- stamping a bot claim on a non-active row would pollute the
-- audit ("who was responsible when this finished?" would falsely
-- name the bot). Leaving claim NULL on those rows keeps the firing's
-- eventual skipped_stale verdict honest.

UPDATE tasks t
   SET claimed_by_agent_id = (SELECT a.id FROM agents a WHERE a.org_id = t.org_id LIMIT 1)
 WHERE t.claimed_by_agent_id IS NULL
   AND t.claimed_by_user_id  IS NULL
   AND t.status = 'queued'
   AND EXISTS (
       SELECT 1 FROM pending_firings pf
        WHERE pf.task_id = t.id
          AND pf.org_id  = t.org_id
          AND pf.status  = 'pending'
   );

-- +goose Down
SELECT 'down not supported';
