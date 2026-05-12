-- +goose Up
-- +goose NO TRANSACTION
-- SKY-261 D-Claims: claim flow on tasks + runs.
--
-- Adds two claim columns on tasks (XOR via CHECK) and one actor column
-- on runs. SQLite can't add a table-level CHECK constraint in place,
-- so the tasks table is rebuilt via the 12-step dance to fold in the
-- XOR. runs gets straightforward ADD COLUMN. agents gains a UNIQUE
-- index on (id, org_id) for parity with the Postgres composite FK
-- target (SKY-260 deliberately skipped this; this ticket is where the
-- "we never compose (agent_id, org_id) at the caller" rationale stops
-- holding).
--
-- NO TRANSACTION because PRAGMA foreign_keys can't be set inside an
-- explicit transaction. The rebuild itself is wrapped in BEGIN/COMMIT
-- manually so the partial state never lands on disk.
--
-- See docs/specs/sky-261-d-claims.html.

-- (1) Agents parity index. id is already PK-unique so (id, org_id) is
-- trivially unique on existing data — no values to migrate.
CREATE UNIQUE INDEX IF NOT EXISTS agents_id_org_unique ON agents (id, org_id);

-- (2) runs.actor_agent_id — straightforward ADD COLUMN (no CHECK, no
-- NOT NULL promotion). Inline plain FK to agents(id) — SQLite's ALTER
-- TABLE ADD COLUMN doesn't accept a composite FK, but at N=1 in local
-- mode there's one org so cross-org leakage is structurally impossible.
ALTER TABLE runs ADD COLUMN actor_agent_id TEXT
    REFERENCES agents (id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS runs_actor_agent_idx ON runs(actor_agent_id)
    WHERE actor_agent_id IS NOT NULL;

-- (3) Tasks rebuild: ADD COLUMN both claim columns AND fold in the XOR
-- CHECK. SQLite can't add a table-level CHECK in place; the rebuild is
-- the standard pattern, used by SKY-259's event_handlers migration.
PRAGMA foreign_keys=OFF;

BEGIN;

CREATE TABLE tasks_new (
    id                   TEXT PRIMARY KEY,
    entity_id            TEXT NOT NULL REFERENCES entities(id),
    event_type           TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
    dedup_key            TEXT NOT NULL DEFAULT '',
    primary_event_id     TEXT NOT NULL REFERENCES events(id),
    status               TEXT NOT NULL DEFAULT 'queued',
    priority_score       REAL,
    ai_summary           TEXT,
    autonomy_suitability REAL,
    priority_reasoning   TEXT,
    scoring_status       TEXT NOT NULL DEFAULT 'pending',
    severity             TEXT,
    relevance_reason     TEXT,
    source_status        TEXT,
    snooze_until         DATETIME,
    close_reason         TEXT,
    close_event_type     TEXT REFERENCES events_catalog(id),
    closed_at            DATETIME,
    created_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    org_id               TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    team_id              TEXT,
    creator_user_id      TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000100',
    visibility           TEXT NOT NULL DEFAULT 'team'
        CHECK (visibility IN ('private','team','org')),
    -- SKY-261 D-Claims: two claim cols + XOR.
    claimed_by_agent_id  TEXT REFERENCES agents(id) ON DELETE SET NULL,
    claimed_by_user_id   TEXT REFERENCES users(id)  ON DELETE SET NULL,
    CHECK (claimed_by_agent_id IS NULL OR claimed_by_user_id IS NULL)
);

INSERT INTO tasks_new (
    id, entity_id, event_type, dedup_key, primary_event_id,
    status, priority_score, ai_summary, autonomy_suitability,
    priority_reasoning, scoring_status, severity, relevance_reason,
    source_status, snooze_until, close_reason, close_event_type,
    closed_at, created_at,
    org_id, team_id, creator_user_id, visibility
)
SELECT
    id, entity_id, event_type, dedup_key, primary_event_id,
    status, priority_score, ai_summary, autonomy_suitability,
    priority_reasoning, scoring_status, severity, relevance_reason,
    source_status, snooze_until, close_reason, close_event_type,
    closed_at, created_at,
    org_id, team_id, creator_user_id, visibility
FROM tasks;

DROP TABLE tasks;

ALTER TABLE tasks_new RENAME TO tasks;

-- Recreate the indexes that lived on the original table. Baseline
-- indexes (idx_tasks_*) AND the composite-uniqueness index
-- tasks_id_org_unique that SKY-259 (202605120008_event_handlers_unification.sql:559)
-- added — the table drop above nukes ALL of its indexes, including
-- ones added after baseline by later migrations. Forgetting to
-- recreate tasks_id_org_unique would silently drop the composite-
-- uniqueness invariant the Postgres composite FKs rely on (and that
-- future SQLite child FKs on (task_id, org_id) would need too).
CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_active_entity_event_dedup
    ON tasks(entity_id, event_type, dedup_key) WHERE status NOT IN ('done', 'dismissed');
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);
CREATE INDEX IF NOT EXISTS idx_tasks_entity ON tasks(entity_id);
CREATE INDEX IF NOT EXISTS idx_tasks_status_priority ON tasks(status, priority_score DESC);
CREATE UNIQUE INDEX IF NOT EXISTS tasks_id_org_unique ON tasks (id, org_id);

-- New partial indexes for the per-member Board filter.
CREATE INDEX IF NOT EXISTS tasks_claimed_agent_idx ON tasks(claimed_by_agent_id)
    WHERE claimed_by_agent_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS tasks_claimed_user_idx ON tasks(claimed_by_user_id)
    WHERE claimed_by_user_id IS NOT NULL;

COMMIT;

PRAGMA foreign_keys=ON;

-- +goose Down
SELECT 'down not supported';
