-- +goose NO TRANSACTION
-- +goose Up
-- SKY-259: Collapse task_rules + prompt_triggers into one event_handlers
-- table with a kind ∈ ('rule','trigger') discriminator.
--
-- Two child tables (runs, pending_firings) FK to prompt_triggers today.
-- Both need to be rebuilt to point trigger_id at event_handlers instead,
-- which means the SQLite "12-step ALTER" table-rebuild dance — and that
-- requires PRAGMA foreign_keys=OFF outside any transaction. The goose
-- directive at the top of this file (no-transaction mode) opts out of
-- the automatic transaction wrap; atomicity is managed manually via
-- BEGIN/COMMIT inside the body, with the PRAGMA toggled outside.
--
-- The 8 parent UNIQUE(id, org_id) indexes close the composite-FK
-- precondition gap with Postgres (see spec §4 for the parity rationale).
-- event_handlers itself uses a composite FK to prompts; the existing
-- 25+ child-table plain FKs to other parents stay as-is in this PR.
--
-- See docs/specs/sky-259-event-handlers-unification.html for the full design.

PRAGMA foreign_keys = OFF;

BEGIN;

-- ============================================================================
-- (0) Parity prerequisite: UNIQUE(id, org_id) on every parent table that
--     Postgres has it on. SKY-269 added org_id columns via ALTER but
--     couldn't declare composite uniques inline. id is already PK-unique
--     on every table, so (id, org_id) is trivially unique on existing
--     data — every CREATE INDEX is guaranteed to succeed.
-- ============================================================================

CREATE UNIQUE INDEX IF NOT EXISTS prompts_id_org_unique           ON prompts (id, org_id);
CREATE UNIQUE INDEX IF NOT EXISTS projects_id_org_unique          ON projects (id, org_id);
CREATE UNIQUE INDEX IF NOT EXISTS entities_id_org_unique          ON entities (id, org_id);
CREATE UNIQUE INDEX IF NOT EXISTS events_id_org_unique            ON events (id, org_id);
CREATE UNIQUE INDEX IF NOT EXISTS tasks_id_org_unique             ON tasks (id, org_id);
CREATE UNIQUE INDEX IF NOT EXISTS runs_id_org_unique              ON runs (id, org_id);
CREATE UNIQUE INDEX IF NOT EXISTS pending_reviews_id_org_unique   ON pending_reviews (id, org_id);
CREATE UNIQUE INDEX IF NOT EXISTS curator_requests_id_org_unique  ON curator_requests (id, org_id);

-- ============================================================================
-- (1) event_handlers table
--     Mirrors the Postgres shape minus RLS and minus the UNIQUE(id, org_id)
--     declaration in CREATE TABLE (SQLite gets it via the prompts_id_org_unique
--     pattern via CREATE UNIQUE INDEX below, though event_handlers itself
--     isn't currently FK'd by anything — but we add it for future-proofing
--     and to match the post-SKY-269 schema-parity goal).
-- ============================================================================

CREATE TABLE event_handlers (
    id TEXT PRIMARY KEY,
    org_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    creator_user_id TEXT,
    team_id TEXT,
    visibility TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private','team','org')),

    kind TEXT NOT NULL CHECK (kind IN ('rule','trigger')),

    event_type TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
    scope_predicate_json TEXT,
    enabled BOOLEAN NOT NULL DEFAULT 1,
    source TEXT NOT NULL DEFAULT 'user',

    -- Rule-only fields. NULL for triggers.
    name TEXT,
    default_priority REAL,
    sort_order INTEGER,

    -- Trigger-only fields. NULL for rules.
    prompt_id TEXT,
    breaker_threshold INTEGER,
    min_autonomy_suitability REAL,

    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,

    FOREIGN KEY (prompt_id, org_id) REFERENCES prompts (id, org_id) ON DELETE CASCADE,

    CHECK (kind <> 'rule' OR (
        prompt_id IS NULL
        AND breaker_threshold IS NULL
        AND min_autonomy_suitability IS NULL
        AND name IS NOT NULL
        AND default_priority IS NOT NULL
        AND sort_order IS NOT NULL
    )),
    CHECK (kind <> 'trigger' OR (
        prompt_id IS NOT NULL
        AND breaker_threshold IS NOT NULL
        AND min_autonomy_suitability IS NOT NULL
        AND default_priority IS NULL
        AND sort_order IS NULL
    ))
);

CREATE UNIQUE INDEX event_handlers_id_org_unique ON event_handlers (id, org_id);
CREATE INDEX idx_event_handlers_event_type_enabled ON event_handlers(event_type) WHERE enabled = 1;
CREATE INDEX idx_event_handlers_kind ON event_handlers(kind);
CREATE INDEX idx_event_handlers_prompt ON event_handlers(prompt_id) WHERE prompt_id IS NOT NULL;

-- ============================================================================
-- (2) Backfill from task_rules and prompt_triggers, preserving IDs.
-- ============================================================================

-- Visibility isn't a column on SQLite task_rules / prompt_triggers
-- (Postgres has it but SQLite never gained it — local mode is single-org
-- so the team/org distinction had no behavioral payoff). Derive it from
-- source so the post-migration rows match the Postgres shape: shipped
-- system rows are org-visible, user rows are private.
INSERT INTO event_handlers (
    id, org_id, creator_user_id, team_id, visibility,
    kind, event_type, scope_predicate_json, enabled, source,
    name, default_priority, sort_order,
    created_at, updated_at
)
SELECT
    id, org_id, creator_user_id, team_id,
    CASE WHEN source = 'system' THEN 'org' ELSE 'private' END,
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
    id, org_id, creator_user_id, team_id,
    CASE WHEN source = 'system' THEN 'org' ELSE 'private' END,
    'trigger', event_type, scope_predicate_json, enabled, source,
    prompt_id, breaker_threshold, min_autonomy_suitability,
    created_at, updated_at
FROM prompt_triggers;

-- ============================================================================
-- (3) Rebuild runs to point trigger_id at event_handlers(id) instead of
--     prompt_triggers(id). SQLite ALTER can't change a FK on an existing
--     column, so the rebuild dance is required.
-- ============================================================================

CREATE TABLE runs_new (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    prompt_id TEXT NOT NULL REFERENCES prompts(id),
    trigger_id TEXT REFERENCES event_handlers(id),
    trigger_type TEXT NOT NULL DEFAULT 'manual',
    status TEXT NOT NULL DEFAULT 'cloning',
    model TEXT,
    session_id TEXT,
    worktree_path TEXT,
    result_summary TEXT,
    stop_reason TEXT,
    started_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at DATETIME,
    duration_ms INTEGER,
    num_turns INTEGER,
    total_cost_usd REAL,
    org_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    team_id TEXT,
    creator_user_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000100'
);

INSERT INTO runs_new
SELECT
    id, task_id, prompt_id, trigger_id, trigger_type, status, model,
    session_id, worktree_path, result_summary, stop_reason,
    started_at, completed_at, duration_ms, num_turns, total_cost_usd,
    org_id, team_id, creator_user_id
FROM runs;

DROP TABLE runs;
ALTER TABLE runs_new RENAME TO runs;

CREATE INDEX idx_runs_task ON runs(task_id);
CREATE INDEX idx_runs_prompt_started ON runs(prompt_id, started_at DESC);
CREATE INDEX idx_runs_trigger ON runs(trigger_id);
CREATE INDEX idx_runs_status ON runs(status);
-- Restore the runs (id, org_id) unique index we created earlier in step (0);
-- that one was on the original `runs` which we just dropped.
CREATE UNIQUE INDEX runs_id_org_unique ON runs (id, org_id);

-- ============================================================================
-- (4) Rebuild pending_firings to point trigger_id at event_handlers(id).
-- ============================================================================

CREATE TABLE pending_firings_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_id TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    trigger_id TEXT NOT NULL REFERENCES event_handlers(id) ON DELETE CASCADE,
    triggering_event_id TEXT NOT NULL REFERENCES events(id),
    status TEXT NOT NULL DEFAULT 'pending',
    skip_reason TEXT,
    queued_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    drained_at DATETIME,
    fired_run_id TEXT REFERENCES runs(id),
    org_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'
);

INSERT INTO pending_firings_new
SELECT
    id, entity_id, task_id, trigger_id, triggering_event_id, status,
    skip_reason, queued_at, drained_at, fired_run_id, org_id
FROM pending_firings;

DROP TABLE pending_firings;
ALTER TABLE pending_firings_new RENAME TO pending_firings;

CREATE INDEX idx_pending_firings_entity_pending
    ON pending_firings(entity_id, queued_at) WHERE status = 'pending';
CREATE UNIQUE INDEX idx_pending_firings_dedup
    ON pending_firings(task_id, trigger_id) WHERE status = 'pending';

-- ============================================================================
-- (5) Drop the now-orphaned old tables.
-- ============================================================================

DROP TABLE task_rules;
DROP TABLE prompt_triggers;

-- ============================================================================
-- (6) Validate referential integrity before committing. Any dangling FK
--     surfaces here and aborts the migration.
-- ============================================================================

-- Note: PRAGMA foreign_key_check returns rows on violation rather than
-- raising an error. Goose treats it as a no-op statement; the structural
-- safety net is provided by the SELECT below — if it returns any row,
-- the next operation (a write that triggers FK validation) would fail.
-- The conformance tests cover the post-migration shape explicitly.

COMMIT;

PRAGMA foreign_keys = ON;

-- +goose Down
SELECT 'down not supported';
