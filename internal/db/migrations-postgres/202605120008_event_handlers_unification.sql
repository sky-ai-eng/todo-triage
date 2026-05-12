-- +goose Up
-- SKY-259: Collapse task_rules + prompt_triggers into one event_handlers
-- table with a kind ∈ ('rule','trigger') discriminator. The semantics
-- already converged after the team/agent reframe — rule and trigger are
-- the same primitive with an "auto-claim?" knob (D-Claims, SKY-261, lands
-- next). Two tables / two stores / two RLS policy sets / two router
-- queries become one of each.
--
-- This migration:
--   (1) creates event_handlers with per-kind CHECK constraints
--   (2) ports the post-SKY-246 RLS pattern (split insert/update/delete,
--       admin-only on visibility='org') from prompts / task_rules
--   (3) backfills every row from task_rules + prompt_triggers, preserving
--       IDs verbatim — both backfills use the row's existing UUID so any
--       downstream reference (runs.trigger_id, pending_firings.trigger_id)
--       continues to resolve after the unification
--   (4) repoints the FKs on runs.trigger_id + pending_firings.trigger_id
--       from prompt_triggers to event_handlers (REQUIRED — Postgres won't
--       let us drop a parent with live child FKs)
--   (5) drops task_rules + prompt_triggers
--
-- See docs/specs/sky-259-event-handlers-unification.html for the full design.

-- ============================================================================
-- (1) event_handlers table
-- ============================================================================

CREATE TABLE event_handlers (
  id                       UUID NOT NULL DEFAULT gen_random_uuid(),
  org_id                   UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  creator_user_id          UUID REFERENCES users(id) ON DELETE CASCADE,
  team_id                  UUID REFERENCES teams(id) ON DELETE SET NULL,
  visibility               TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private','team','org')),
  CONSTRAINT event_handlers_team_visibility_requires_team
    CHECK (visibility <> 'team' OR team_id IS NOT NULL),

  -- Discriminator. Determines which per-kind fields are populated.
  kind                     TEXT NOT NULL CHECK (kind IN ('rule','trigger')),

  event_type               TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
  scope_predicate_json     JSONB,
  enabled                  BOOLEAN NOT NULL DEFAULT TRUE,
  source                   TEXT NOT NULL DEFAULT 'user',

  -- Rule-only fields. NULL for triggers.
  name                     TEXT,
  default_priority         REAL,
  sort_order               INT,

  -- Trigger-only fields. NULL for rules.
  prompt_id                TEXT,
  breaker_threshold        INT,
  min_autonomy_suitability REAL,

  created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- Two unique keys mirror the prompts pattern: PRIMARY KEY (org_id, id)
  -- for the org-scoped natural read, UNIQUE (id, org_id) so downstream
  -- composite FKs (runs.trigger_id, pending_firings.trigger_id) can
  -- target (id, org_id) — Postgres requires column order to match a
  -- unique constraint exactly.
  PRIMARY KEY (org_id, id),
  UNIQUE (id, org_id),

  -- Composite FK to prompts on the trigger leg. NULL prompt_id (rule
  -- rows) skips the FK check.
  FOREIGN KEY (prompt_id, org_id) REFERENCES prompts (id, org_id) ON DELETE CASCADE,

  -- Per-kind shape enforcement.
  CONSTRAINT event_handlers_rule_shape
    CHECK (kind <> 'rule' OR (
      prompt_id IS NULL
      AND breaker_threshold IS NULL
      AND min_autonomy_suitability IS NULL
      AND name IS NOT NULL
      AND default_priority IS NOT NULL
      AND sort_order IS NOT NULL
    )),
  CONSTRAINT event_handlers_trigger_shape
    CHECK (kind <> 'trigger' OR (
      prompt_id IS NOT NULL
      AND breaker_threshold IS NOT NULL
      AND min_autonomy_suitability IS NOT NULL
      AND default_priority IS NULL
      AND sort_order IS NULL
    )),

  -- System-row coherence: system rows have no human creator. Same shape
  -- as task_rules_system_has_no_creator and prompts_system_has_no_creator.
  CONSTRAINT event_handlers_system_has_no_creator
    CHECK ((source = 'system' AND creator_user_id IS NULL)
        OR (source = 'user'   AND creator_user_id IS NOT NULL))
);

CREATE INDEX idx_event_handlers_org_event_enabled
    ON event_handlers(org_id, event_type) WHERE enabled = TRUE;

CREATE INDEX idx_event_handlers_org_kind
    ON event_handlers(org_id, kind);

CREATE INDEX idx_event_handlers_prompt
    ON event_handlers(org_id, prompt_id) WHERE prompt_id IS NOT NULL;

ALTER TABLE event_handlers ENABLE ROW LEVEL SECURITY;

-- The baseline's "GRANT ON ALL TABLES IN SCHEMA public TO tf_app" only
-- covered tables that existed at that migration's run time. Each
-- subsequent table needs its own grant or tf_app callers hit
-- permission-denied. Same pattern SKY-260's agents + team_agents
-- migration used.
GRANT SELECT, INSERT, UPDATE, DELETE ON event_handlers TO tf_app;

-- ============================================================================
-- (2) RLS policies — split insert/update/delete, admin-only on org-visible
--     Port of the post-SKY-246 pattern from task_rules + prompt_triggers.
-- ============================================================================

CREATE POLICY event_handlers_select ON event_handlers FOR SELECT
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND (creator_user_id = tf.current_user_id()
              OR (visibility = 'team' AND team_id IS NOT NULL
                  AND EXISTS (SELECT 1 FROM memberships m
                              WHERE m.user_id = tf.current_user_id()
                                AND m.team_id = event_handlers.team_id))
              OR visibility = 'org'));

CREATE POLICY event_handlers_insert ON event_handlers FOR INSERT
  WITH CHECK (org_id = tf.current_org_id()
              AND tf.user_has_org_access(org_id)
              AND creator_user_id = tf.current_user_id());

CREATE POLICY event_handlers_update ON event_handlers FOR UPDATE
  USING (org_id = tf.current_org_id()
         AND ((creator_user_id = tf.current_user_id()
               AND tf.user_has_org_access(org_id))
              OR (visibility = 'org' AND tf.user_is_org_admin(org_id))))
  WITH CHECK (org_id = tf.current_org_id()
              AND ((creator_user_id = tf.current_user_id()
                    AND tf.user_has_org_access(org_id))
                   OR (visibility = 'org' AND tf.user_is_org_admin(org_id))));

CREATE POLICY event_handlers_delete ON event_handlers FOR DELETE
  USING (org_id = tf.current_org_id()
         AND tf.user_has_org_access(org_id)
         AND creator_user_id = tf.current_user_id());

CREATE TRIGGER set_updated_at BEFORE UPDATE ON event_handlers
    FOR EACH ROW EXECUTE FUNCTION tf.set_updated_at();

-- ============================================================================
-- (3) Backfill from task_rules and prompt_triggers
--     Both INSERTs copy id verbatim so downstream FK references survive
--     the unification.
-- ============================================================================

INSERT INTO event_handlers (
  id, org_id, creator_user_id, team_id, visibility,
  kind, event_type, scope_predicate_json, enabled, source,
  name, default_priority, sort_order,
  created_at, updated_at
)
SELECT
  id, org_id, creator_user_id, team_id, visibility,
  'rule', event_type, scope_predicate_json, enabled, source,
  name, default_priority, sort_order,
  created_at, updated_at
FROM task_rules;

INSERT INTO event_handlers (
  id, org_id, creator_user_id, team_id, visibility,
  kind, event_type, scope_predicate_json, enabled, source,
  prompt_id, breaker_threshold, min_autonomy_suitability,
  created_at, updated_at
)
SELECT
  id, org_id, creator_user_id, team_id, visibility,
  'trigger', event_type, scope_predicate_json, enabled, source,
  prompt_id, breaker_threshold, min_autonomy_suitability,
  created_at, updated_at
FROM prompt_triggers;

-- ============================================================================
-- (4) Repoint child FKs from prompt_triggers to event_handlers
--     Both child tables already reference (id, org_id) — the new FKs use
--     the same composite shape, pointing at the unified parent.
-- ============================================================================

ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_trigger_id_org_id_fkey;
ALTER TABLE runs
  ADD CONSTRAINT runs_trigger_id_org_id_fkey
  FOREIGN KEY (trigger_id, org_id) REFERENCES event_handlers (id, org_id);

ALTER TABLE pending_firings DROP CONSTRAINT IF EXISTS pending_firings_trigger_id_org_id_fkey;
ALTER TABLE pending_firings
  ADD CONSTRAINT pending_firings_trigger_id_org_id_fkey
  FOREIGN KEY (trigger_id, org_id) REFERENCES event_handlers (id, org_id) ON DELETE CASCADE;

-- ============================================================================
-- (5) Drop the old tables
--     Their RLS policies / triggers / indexes go with them automatically.
-- ============================================================================

DROP TABLE task_rules;
DROP TABLE prompt_triggers;

-- +goose Down
SELECT 'down not supported';
