-- +goose Up
-- SKY-262 D-TeamDefault: flip visibility default from 'private' to 'team'
-- across the five team-scopable tables, backfill existing private rows to
-- team-scoped with the creator's primary team, promote team_id to NOT NULL
-- on tasks/runs/event_handlers, and replace single-creator-only modify
-- policies with the canonical three-branch (private/team/org) shape that
-- lets team members modify team-visible rows + admin-only on org-visible.
--
-- This is the schema foundation for the team-as-unit-of-work reframe
-- (arch decision `resource-scoping`) and the team-scoped triage queue
-- (arch decision `task-state-axes`). D-Claims (SKY-261) writes claim
-- columns onto the team-scoped surface this migration creates.
--
-- See Linear SKY-262 for the full scope.

-- ============================================================================
-- (1) Pre-flight check
-- ============================================================================
-- Refuse to run if any existing 'private' row has a creator_user_id whose
-- memberships can't be resolved to a team in the same org. The backfill
-- SELECT below would silently produce NULL team_id for those rows,
-- violating the team-required CHECK once visibility flips to 'team'.
-- Abort here with a diagnostic so the operator can investigate.

-- +goose StatementBegin
DO $$
DECLARE
  bad_tasks       INT;
  bad_runs        INT;
  bad_event_h     INT;
  bad_prompts     INT;
  bad_projects    INT;
BEGIN
  SELECT count(*) INTO bad_tasks
  FROM tasks
  WHERE visibility = 'private'
    AND NOT EXISTS (
      SELECT 1 FROM memberships m
      JOIN teams t ON t.id = m.team_id
      WHERE m.user_id = tasks.creator_user_id
        AND t.org_id  = tasks.org_id
    );

  SELECT count(*) INTO bad_runs
  FROM runs
  WHERE visibility = 'private'
    AND NOT EXISTS (
      SELECT 1 FROM memberships m
      JOIN teams t ON t.id = m.team_id
      WHERE m.user_id = runs.creator_user_id
        AND t.org_id  = runs.org_id
    );

  SELECT count(*) INTO bad_event_h
  FROM event_handlers
  WHERE visibility = 'private'
    AND creator_user_id IS NOT NULL  -- system rows have NULL creator + visibility='org'
    AND NOT EXISTS (
      SELECT 1 FROM memberships m
      JOIN teams t ON t.id = m.team_id
      WHERE m.user_id = event_handlers.creator_user_id
        AND t.org_id  = event_handlers.org_id
    );

  SELECT count(*) INTO bad_prompts
  FROM prompts
  WHERE visibility = 'private'
    AND creator_user_id IS NOT NULL
    AND NOT EXISTS (
      SELECT 1 FROM memberships m
      JOIN teams t ON t.id = m.team_id
      WHERE m.user_id = prompts.creator_user_id
        AND t.org_id  = prompts.org_id
    );

  SELECT count(*) INTO bad_projects
  FROM projects
  WHERE visibility = 'private'
    AND creator_user_id IS NOT NULL
    AND NOT EXISTS (
      SELECT 1 FROM memberships m
      JOIN teams t ON t.id = m.team_id
      WHERE m.user_id = projects.creator_user_id
        AND t.org_id  = projects.org_id
    );

  IF bad_tasks + bad_runs + bad_event_h + bad_prompts + bad_projects > 0 THEN
    RAISE EXCEPTION 'SKY-262 backfill pre-flight failed: % tasks, % runs, % event_handlers, % prompts, % projects have private rows with no team membership to map to. Resolve by assigning team_id manually or removing the rows before re-running.',
      bad_tasks, bad_runs, bad_event_h, bad_prompts, bad_projects;
  END IF;
END $$;
-- +goose StatementEnd

-- ============================================================================
-- (2) Backfill: private → team with creator's primary team
-- ============================================================================
-- Earliest membership = primary team. Forward-only — historic private rows
-- become team rows so the team queue isn't missing history.
-- System rows (creator_user_id IS NULL + visibility='org') are skipped by
-- the WHERE visibility='private' filter.

UPDATE tasks
   SET visibility = 'team',
       team_id = COALESCE(
         team_id,
         (SELECT m.team_id
            FROM memberships m
            JOIN teams t ON t.id = m.team_id
           WHERE m.user_id = tasks.creator_user_id
             AND t.org_id  = tasks.org_id
        ORDER BY m.created_at ASC
           LIMIT 1)
       )
 WHERE visibility = 'private';

UPDATE runs
   SET visibility = 'team',
       team_id = COALESCE(
         team_id,
         (SELECT m.team_id
            FROM memberships m
            JOIN teams t ON t.id = m.team_id
           WHERE m.user_id = runs.creator_user_id
             AND t.org_id  = runs.org_id
        ORDER BY m.created_at ASC
           LIMIT 1)
       )
 WHERE visibility = 'private';

UPDATE event_handlers
   SET visibility = 'team',
       team_id = COALESCE(
         team_id,
         (SELECT m.team_id
            FROM memberships m
            JOIN teams t ON t.id = m.team_id
           WHERE m.user_id = event_handlers.creator_user_id
             AND t.org_id  = event_handlers.org_id
        ORDER BY m.created_at ASC
           LIMIT 1)
       )
 WHERE visibility = 'private'
   AND creator_user_id IS NOT NULL;

UPDATE prompts
   SET visibility = 'team',
       team_id = COALESCE(
         team_id,
         (SELECT m.team_id
            FROM memberships m
            JOIN teams t ON t.id = m.team_id
           WHERE m.user_id = prompts.creator_user_id
             AND t.org_id  = prompts.org_id
        ORDER BY m.created_at ASC
           LIMIT 1)
       )
 WHERE visibility = 'private'
   AND creator_user_id IS NOT NULL;

UPDATE projects
   SET visibility = 'team',
       team_id = COALESCE(
         team_id,
         (SELECT m.team_id
            FROM memberships m
            JOIN teams t ON t.id = m.team_id
           WHERE m.user_id = projects.creator_user_id
             AND t.org_id  = projects.org_id
        ORDER BY m.created_at ASC
           LIMIT 1)
       )
 WHERE visibility = 'private'
   AND creator_user_id IS NOT NULL;

-- ============================================================================
-- (3) Flip column defaults
-- ============================================================================
-- DEFAULT applies only to future inserts that omit the column. Existing
-- rows already carry 'private' OR 'team' values (post-backfill above);
-- the default change is about handler-omitted future writes.

ALTER TABLE tasks          ALTER COLUMN visibility SET DEFAULT 'team';
ALTER TABLE runs           ALTER COLUMN visibility SET DEFAULT 'team';
ALTER TABLE prompts        ALTER COLUMN visibility SET DEFAULT 'team';
ALTER TABLE projects       ALTER COLUMN visibility SET DEFAULT 'team';
ALTER TABLE event_handlers ALTER COLUMN visibility SET DEFAULT 'team';

-- ============================================================================
-- (4) Promote team_id to NOT NULL where team is the only sensible scope
-- ============================================================================
-- tasks, runs — team_id is now load-bearing for the triage-queue derived
-- filter and the team-scoped Board. These tables are user-authored (no
-- shipped system rows); backfill in (2) guarantees every row has a value.
--
-- event_handlers, prompts, projects — team_id STAYS NULLABLE because
-- system-shipped rows have creator_user_id NULL + visibility='org' and
-- shouldn't be force-pinned to a team. The existing per-table CHECK
-- constraint `<table>_team_visibility_requires_team` enforces team_id
-- NOT NULL whenever visibility='team' — that's the right shape: org-
-- visible system rows skip the constraint, team-visible rows can't.

ALTER TABLE tasks ALTER COLUMN team_id SET NOT NULL;
ALTER TABLE runs  ALTER COLUMN team_id SET NOT NULL;

-- ============================================================================
-- (5) RLS policy sweep — three-branch (private / team / org)
-- ============================================================================
-- The canonical pattern: own private · own/teammate's team · admin-only
-- on org-visible. SKY-259's event_handlers migration ported the shape for
-- the SELECT/INSERT/DELETE sides; here we extend UPDATE to allow team
-- members on team-visible rows. tasks/runs get full new policies (today
-- they're creator-only single-policy from the baseline). prompts/projects
-- get their single-FOR-ALL policy split into insert/update/delete with
-- the team branch added.
--
-- tf.user_in_team(team_id) is the SKY-260 helper; equivalent to the
-- existing inline EXISTS-on-memberships pattern.

-- ---- tasks --------------------------------------------------------------
DROP POLICY IF EXISTS tasks_select ON tasks;
DROP POLICY IF EXISTS tasks_modify ON tasks;

CREATE POLICY tasks_select ON tasks FOR SELECT
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND (
              (visibility = 'private' AND creator_user_id = tf.current_user_id())
           OR (visibility = 'team'    AND tf.user_in_team(team_id))
           OR (visibility = 'org')
         ));

CREATE POLICY tasks_insert ON tasks FOR INSERT
  WITH CHECK (org_id = tf.current_org_id()
              AND tf.user_has_org_access(org_id)
              AND creator_user_id = tf.current_user_id()
              AND (visibility <> 'team' OR tf.user_in_team(team_id)));

-- Defense in depth: `tf.user_in_team(team_id)` only checks the
-- `memberships` table, not `org_memberships`. A memberships row can
-- exist without a corresponding org membership (stale state, bug,
-- attacker-controlled team_id), so the team-branch alone is NOT
-- sufficient to authorize org-scoped writes. The outer
-- `tf.user_has_org_access(org_id)` guard ensures the caller is a real
-- member of THIS org before we even consider the team-membership
-- check. tf.user_is_org_admin internally checks org_memberships, so
-- the org branch is safe without an additional guard, but we keep the
-- outer guard for uniformity.
CREATE POLICY tasks_update ON tasks FOR UPDATE
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND (
              (visibility = 'private' AND creator_user_id = tf.current_user_id())
           OR (visibility = 'team'    AND tf.user_in_team(team_id))
           OR (visibility = 'org'     AND tf.user_is_org_admin(org_id))
         ))
  WITH CHECK (org_id = tf.current_org_id()
              AND tf.user_has_org_access(org_id)
              AND (
                   (visibility = 'private' AND creator_user_id = tf.current_user_id())
                OR (visibility = 'team'    AND tf.user_in_team(team_id))
                OR (visibility = 'org'     AND tf.user_is_org_admin(org_id))
              ));

CREATE POLICY tasks_delete ON tasks FOR DELETE
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND (
              (visibility = 'private' AND creator_user_id = tf.current_user_id())
           OR (visibility = 'team'    AND tf.user_in_team(team_id))
         ));

-- ---- runs ---------------------------------------------------------------
DROP POLICY IF EXISTS runs_select ON runs;
DROP POLICY IF EXISTS runs_modify ON runs;

CREATE POLICY runs_select ON runs FOR SELECT
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND (
              (visibility = 'private' AND creator_user_id = tf.current_user_id())
           OR (visibility = 'team'    AND tf.user_in_team(team_id))
           OR (visibility = 'org')
         ));

CREATE POLICY runs_insert ON runs FOR INSERT
  WITH CHECK (org_id = tf.current_org_id()
              AND tf.user_has_org_access(org_id)
              AND creator_user_id = tf.current_user_id()
              AND (visibility <> 'team' OR tf.user_in_team(team_id)));

-- Same defense-in-depth pattern as tasks_update — outer org_access
-- guard covers the team branch's missing-org-membership case.
CREATE POLICY runs_update ON runs FOR UPDATE
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND (
              (visibility = 'private' AND creator_user_id = tf.current_user_id())
           OR (visibility = 'team'    AND tf.user_in_team(team_id))
           OR (visibility = 'org'     AND tf.user_is_org_admin(org_id))
         ))
  WITH CHECK (org_id = tf.current_org_id()
              AND tf.user_has_org_access(org_id)
              AND (
                   (visibility = 'private' AND creator_user_id = tf.current_user_id())
                OR (visibility = 'team'    AND tf.user_in_team(team_id))
                OR (visibility = 'org'     AND tf.user_is_org_admin(org_id))
              ));

CREATE POLICY runs_delete ON runs FOR DELETE
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND (
              (visibility = 'private' AND creator_user_id = tf.current_user_id())
           OR (visibility = 'team'    AND tf.user_in_team(team_id))
         ));

-- ---- prompts ------------------------------------------------------------
-- prompts_select stays as-is from the baseline (already has the three-
-- branch shape using the inline EXISTS form). SKY-246 PR #146 already
-- split modify into insert/update/delete via
-- 202605110001_system_rows_nullable_creator.sql — drop those existing
-- policies and recreate with the team branch + admin-on-org branch.

DROP POLICY IF EXISTS prompts_modify ON prompts;
DROP POLICY IF EXISTS prompts_insert ON prompts;
DROP POLICY IF EXISTS prompts_update ON prompts;
DROP POLICY IF EXISTS prompts_delete ON prompts;

CREATE POLICY prompts_insert ON prompts FOR INSERT
  WITH CHECK (org_id = tf.current_org_id()
              AND tf.user_has_org_access(org_id)
              AND creator_user_id = tf.current_user_id()
              AND (visibility <> 'team' OR (team_id IS NOT NULL AND tf.user_in_team(team_id))));

-- Same defense-in-depth pattern as tasks_update. The
-- team_id IS NOT NULL guard inside the team arm is kept (system rows
-- have team_id NULL + visibility='org', which the org arm catches).
CREATE POLICY prompts_update ON prompts FOR UPDATE
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND (
              (visibility = 'private' AND creator_user_id = tf.current_user_id())
           OR (visibility = 'team'    AND team_id IS NOT NULL AND tf.user_in_team(team_id))
           OR (visibility = 'org'     AND tf.user_is_org_admin(org_id))
         ))
  WITH CHECK (org_id = tf.current_org_id()
              AND tf.user_has_org_access(org_id)
              AND (
                   (visibility = 'private' AND creator_user_id = tf.current_user_id())
                OR (visibility = 'team'    AND team_id IS NOT NULL AND tf.user_in_team(team_id))
                OR (visibility = 'org'     AND tf.user_is_org_admin(org_id))
              ));

CREATE POLICY prompts_delete ON prompts FOR DELETE
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND (
              (visibility = 'private' AND creator_user_id = tf.current_user_id())
           OR (visibility = 'team'    AND team_id IS NOT NULL AND tf.user_in_team(team_id))
         ));
-- (System rows: creator_user_id IS NULL, so the creator-check arm is FALSE.
-- visibility='org' rows can't be deleted by anyone via this policy — soft-
-- disable is the right path for system rows.)

-- ---- projects -----------------------------------------------------------
DROP POLICY IF EXISTS projects_modify ON projects;

CREATE POLICY projects_insert ON projects FOR INSERT
  WITH CHECK (org_id = tf.current_org_id()
              AND tf.user_has_org_access(org_id)
              AND creator_user_id = tf.current_user_id()
              AND (visibility <> 'team' OR (team_id IS NOT NULL AND tf.user_in_team(team_id))));

-- Same defense-in-depth pattern as prompts_update.
CREATE POLICY projects_update ON projects FOR UPDATE
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND (
              (visibility = 'private' AND creator_user_id = tf.current_user_id())
           OR (visibility = 'team'    AND team_id IS NOT NULL AND tf.user_in_team(team_id))
           OR (visibility = 'org'     AND tf.user_is_org_admin(org_id))
         ))
  WITH CHECK (org_id = tf.current_org_id()
              AND tf.user_has_org_access(org_id)
              AND (
                   (visibility = 'private' AND creator_user_id = tf.current_user_id())
                OR (visibility = 'team'    AND team_id IS NOT NULL AND tf.user_in_team(team_id))
                OR (visibility = 'org'     AND tf.user_is_org_admin(org_id))
              ));

CREATE POLICY projects_delete ON projects FOR DELETE
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND (
              (visibility = 'private' AND creator_user_id = tf.current_user_id())
           OR (visibility = 'team'    AND team_id IS NOT NULL AND tf.user_in_team(team_id))
         ));

-- ---- event_handlers -----------------------------------------------------
-- event_handlers got the post-SKY-246 split policies from SKY-259's
-- migration. SELECT, INSERT, and DELETE are already correct (creator-only
-- or three-branch read). UPDATE today is creator OR admin-on-org — we
-- extend it to also allow team members on team-visible rows.

DROP POLICY IF EXISTS event_handlers_update ON event_handlers;

-- Same defense-in-depth pattern as tasks_update. Note that this policy
-- accepts creator-on-any-visibility (not just private) — matching the
-- post-SKY-259 shape from prompt_triggers/task_rules which allowed
-- creators to edit their own team-visible rows directly. The team
-- branch adds the parallel path for OTHER team members.
CREATE POLICY event_handlers_update ON event_handlers FOR UPDATE
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND (
              (creator_user_id = tf.current_user_id())
           OR (visibility = 'team' AND tf.user_in_team(team_id))
           OR (visibility = 'org'  AND tf.user_is_org_admin(org_id))
         ))
  WITH CHECK (org_id = tf.current_org_id()
              AND tf.user_has_org_access(org_id)
              AND (
                   (creator_user_id = tf.current_user_id())
                OR (visibility = 'team' AND tf.user_in_team(team_id))
                OR (visibility = 'org'  AND tf.user_is_org_admin(org_id))
              ));

-- Extend event_handlers_delete to allow team members on team-visible rows.
-- (SKY-259's policy was creator-only.)
DROP POLICY IF EXISTS event_handlers_delete ON event_handlers;

CREATE POLICY event_handlers_delete ON event_handlers FOR DELETE
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND (
              (visibility = 'private' AND creator_user_id = tf.current_user_id())
           OR (visibility = 'team'    AND tf.user_in_team(team_id))
         ));

-- +goose Down
SELECT 'down not supported';
