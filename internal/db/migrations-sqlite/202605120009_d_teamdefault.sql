-- +goose Up
-- SKY-262 D-TeamDefault: SQLite parity for team-default visibility.
--
-- Local mode has exactly one team (runmode.LocalDefaultTeam, sentinel
-- '00000000-0000-0000-0000-000000000010' seeded by SKY-269) and one
-- user. Most of the Postgres heavy-lift in this domain (RLS sweep,
-- three-branch policies, NOT NULL promotions, FK reshuffling) doesn't
-- earn its keep here because the single-tenant invariant makes cross-
-- team leakage structurally impossible.
--
-- This SQLite migration's job is purely structural parity so domain
-- types resolve identically across backends and the team-scoped queue
-- query works at N=1:
--
--   (1) backfill team_id on existing rows missing it (single team to
--       map to — no ambiguity)
--   (2) add the visibility column on tasks/runs/prompts/projects so
--       the column exists in both backends; event_handlers already
--       carries it from SKY-259
--   (3) flip existing event_handlers user rows from 'private' to
--       'team' (the user-source rows that pre-date this migration)
--   (4) flip existing prompts system rows to 'org' so the new
--       team-default doesn't quietly mis-scope them
--
-- Handler-level validation (added in the same PR) does the work that
-- Postgres CHECK + RLS does — rejects visibility='team' writes with
-- team_id empty. Domain types add a Visibility field on all five
-- tables; SQLite stores read/write it identically to Postgres.

-- ============================================================================
-- (1) Backfill team_id on rows that pre-date the team scope
-- ============================================================================
-- SKY-269 added team_id as a NULLABLE column with no backfill, so every
-- pre-SKY-269 row carries team_id IS NULL. At N=1 there's exactly one
-- team to map them to. System rows on prompts (source='system') stay
-- with team_id NULL — they're org-shared, not team-scoped.

UPDATE tasks
   SET team_id = '00000000-0000-0000-0000-000000000010'
 WHERE team_id IS NULL;

UPDATE runs
   SET team_id = '00000000-0000-0000-0000-000000000010'
 WHERE team_id IS NULL;

UPDATE event_handlers
   SET team_id = '00000000-0000-0000-0000-000000000010'
 WHERE team_id IS NULL
   AND source <> 'system';

UPDATE prompts
   SET team_id = '00000000-0000-0000-0000-000000000010'
 WHERE team_id IS NULL
   AND source <> 'system';

UPDATE projects
   SET team_id = '00000000-0000-0000-0000-000000000010'
 WHERE team_id IS NULL;
-- (projects has no source column in SQLite — it doesn't ship system rows)

-- ============================================================================
-- (2) Flip existing user-source rows on event_handlers from 'private'
--     to 'team'
-- ============================================================================
-- event_handlers landed in SQLite via SKY-259 with visibility DEFAULT
-- 'private'. The seed for shipped (source='system') rows explicitly sets
-- visibility='org'; everything else is currently 'private'. Bring user
-- rows in line with the new team-default world.

UPDATE event_handlers
   SET visibility = 'team'
 WHERE visibility = 'private'
   AND source <> 'system';

-- ============================================================================
-- (3) Add visibility column to tables that don't have it yet
-- ============================================================================
-- tasks / runs / prompts / projects all gain a visibility column with
-- the new team-default. Enum CHECK matches Postgres. The CHECK
-- constraint on team-visible-requires-team-id is Postgres-only defense
-- in depth; handler validation enforces it here.

ALTER TABLE tasks    ADD COLUMN visibility TEXT NOT NULL DEFAULT 'team'
    CHECK (visibility IN ('private','team','org'));

ALTER TABLE runs     ADD COLUMN visibility TEXT NOT NULL DEFAULT 'team'
    CHECK (visibility IN ('private','team','org'));

ALTER TABLE prompts  ADD COLUMN visibility TEXT NOT NULL DEFAULT 'team'
    CHECK (visibility IN ('private','team','org'));

ALTER TABLE projects ADD COLUMN visibility TEXT NOT NULL DEFAULT 'team'
    CHECK (visibility IN ('private','team','org'));

-- ============================================================================
-- (4) Fix existing prompts system rows to be org-visible
-- ============================================================================
-- The DEFAULT 'team' in step (3) backfilled ALL existing prompts to
-- visibility='team' — including the shipped system rows. System
-- prompts should be org-visible (admin-managed) per the Postgres
-- pattern, so flip them here. user rows correctly land on 'team'.

UPDATE prompts SET visibility = 'org' WHERE source = 'system';

-- (event_handlers system rows were already 'org' from the SKY-259 seed —
-- step (2)'s update only touched 'private' rows, so 'org' rows stayed
-- as-is. No flip needed there.)

-- +goose Down
SELECT 'down not supported';
