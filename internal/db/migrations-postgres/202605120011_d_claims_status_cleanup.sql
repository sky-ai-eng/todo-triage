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

-- +goose Down
SELECT 'down not supported';
