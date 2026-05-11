-- +goose Up
-- SKY-269 D-LocalParity: local mode = multi mode at N=1, at the schema layer.
--
-- This migration makes the SQLite schema structurally match the Postgres
-- one (modulo RLS + auth.users FK + role enums, which are explicitly
-- out of scope per the spec). Five new tenancy tables get one sentinel
-- row each; every resource table gains org_id / team_id /
-- creator_user_id columns where Postgres carries them; agents +
-- team_agents are rebuilt to declare the FK constraints that
-- SQLite's ALTER TABLE can't add inline.
--
-- The sentinel UUIDs are the canonical local-mode identity values.
-- They MUST match the runmode.LocalDefault*ID constants in code;
-- TestBootstrapLocalTenancy_ConstantsMatchRows asserts the equivalence.
--
-- Spec: docs/specs/sky-269-d-local-parity.html.

-- === Tenancy tables =======================================================

CREATE TABLE IF NOT EXISTS orgs (
    id           TEXT PRIMARY KEY,
    slug         TEXT NOT NULL UNIQUE,
    name         TEXT NOT NULL,
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS teams (
    id           TEXT PRIMARY KEY,
    org_id       TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    slug         TEXT NOT NULL,
    name         TEXT NOT NULL,
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (org_id, slug)
);

CREATE TABLE IF NOT EXISTS users (
    id              TEXT PRIMARY KEY,
    display_name    TEXT,
    github_username TEXT,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS org_memberships (
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    org_id       TEXT NOT NULL REFERENCES orgs(id)  ON DELETE CASCADE,
    role         TEXT NOT NULL DEFAULT 'member'
                    CHECK (role IN ('owner', 'admin', 'member')),
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, org_id)
);

CREATE TABLE IF NOT EXISTS memberships (
    user_id      TEXT NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    team_id      TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    role         TEXT NOT NULL DEFAULT 'member'
                    CHECK (role IN ('admin', 'member', 'viewer')),
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, team_id)
);

-- === Sentinel rows (synthetic local identity) =============================
-- The sentinel UUIDs are byte-identical to runmode.LocalDefault*ID.
-- INSERT OR IGNORE so this migration is safe to re-run / safe on
-- legacy-runner-populated installs that may have manually inserted
-- rows pre-this-PR (none should — these tables didn't exist).

INSERT OR IGNORE INTO orgs (id, slug, name) VALUES
    ('00000000-0000-0000-0000-000000000001', 'local', 'Local');

INSERT OR IGNORE INTO teams (id, org_id, slug, name) VALUES
    ('00000000-0000-0000-0000-000000000010',
     '00000000-0000-0000-0000-000000000001',
     'default', 'Default');

INSERT OR IGNORE INTO users (id, display_name) VALUES
    ('00000000-0000-0000-0000-000000000100', 'You');

INSERT OR IGNORE INTO org_memberships (user_id, org_id, role) VALUES
    ('00000000-0000-0000-0000-000000000100',
     '00000000-0000-0000-0000-000000000001',
     'owner');

INSERT OR IGNORE INTO memberships (user_id, team_id, role) VALUES
    ('00000000-0000-0000-0000-000000000100',
     '00000000-0000-0000-0000-000000000010',
     'admin');

-- === Resource table column sweep ==========================================
-- ALTER TABLE ADD COLUMN with NOT NULL DEFAULT is the only mode where
-- SQLite permits the column add on an existing populated table without
-- a rebuild. We pick that branch for every table here. FK constraints
-- cannot be added via ALTER on existing columns in SQLite — the
-- column-level NOT NULL + the synthetic-UUID DEFAULT is the
-- correctness gate; the structural FK lives in Postgres only. Data
-- integrity holds because the only INSERTs going forward come from
-- store code passing runmode.LocalDefault*ID, which by construction
-- references the sentinels we just seeded.
--
-- Column matrix (matches Postgres baseline shape per resource):
--   org_id only         — derived/log tables (no human author, inherit org from parent)
--   org_id + creator    — user-authored tables without team scope
--   org_id + creator + team — team-shareable user-authored tables

-- prompts: org-scoped, user-authored, team-shareable
ALTER TABLE prompts ADD COLUMN org_id     TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE prompts ADD COLUMN team_id    TEXT;
ALTER TABLE prompts ADD COLUMN creator_user_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000100';

-- projects: same shape as prompts
ALTER TABLE projects ADD COLUMN org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE projects ADD COLUMN team_id         TEXT;
ALTER TABLE projects ADD COLUMN creator_user_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000100';

-- entities / entity_links / events: system-emitted, org-scoped only
ALTER TABLE entities     ADD COLUMN org_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE entity_links ADD COLUMN org_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE events       ADD COLUMN org_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';

-- task_rules + prompt_triggers: user-authored, team-shareable
ALTER TABLE task_rules ADD COLUMN org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE task_rules ADD COLUMN team_id         TEXT;
ALTER TABLE task_rules ADD COLUMN creator_user_id TEXT;
-- (creator_user_id nullable on task_rules because system rows have NULL
-- creator per the 202605110001_system_rows_nullable_creator pattern;
-- backfill: every existing row's creator stays NULL until a user
-- modifies it.)

ALTER TABLE prompt_triggers ADD COLUMN org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE prompt_triggers ADD COLUMN team_id         TEXT;
ALTER TABLE prompt_triggers ADD COLUMN creator_user_id TEXT;

-- tasks + runs: user-attributable, team-shareable
ALTER TABLE tasks ADD COLUMN org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE tasks ADD COLUMN team_id         TEXT;
ALTER TABLE tasks ADD COLUMN creator_user_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000100';

ALTER TABLE runs ADD COLUMN org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE runs ADD COLUMN team_id         TEXT;
ALTER TABLE runs ADD COLUMN creator_user_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000100';

-- Derived/log tables: org_id only (parents enforce the rest via their RLS in Postgres)
ALTER TABLE task_events      ADD COLUMN org_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE run_artifacts    ADD COLUMN org_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE run_messages     ADD COLUMN org_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE run_memory       ADD COLUMN org_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE pending_firings  ADD COLUMN org_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE run_worktrees    ADD COLUMN org_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE pending_prs      ADD COLUMN org_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE repo_profiles    ADD COLUMN org_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE poller_state     ADD COLUMN org_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE pending_reviews  ADD COLUMN org_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE pending_review_comments ADD COLUMN org_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';

-- swipe_events: user-attributable, no team
ALTER TABLE swipe_events ADD COLUMN org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE swipe_events ADD COLUMN creator_user_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000100';

-- curator tables: user-attributable
ALTER TABLE curator_requests        ADD COLUMN org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE curator_requests        ADD COLUMN creator_user_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000100';
ALTER TABLE curator_messages        ADD COLUMN org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE curator_messages        ADD COLUMN creator_user_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000100';
ALTER TABLE curator_pending_context ADD COLUMN org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';
ALTER TABLE curator_pending_context ADD COLUMN creator_user_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000100';

-- === agents / team_agents rebuild =========================================
-- These two tables shipped in 202605120001_agents.sql without org/team
-- FK columns (or with team_id as plain TEXT). SQLite ALTER TABLE
-- doesn't support adding FK constraints to existing columns, so we
-- do the rename-create-copy-drop dance. The data is one row each in
-- practice; the rebuild is fast.
--
-- agents: gains a real org_id column with FK to orgs. github_pat_user_id
-- column already exists; we add the FK to users by virtue of the
-- rebuild (the original column had no REFERENCES clause).

ALTER TABLE agents RENAME TO agents_pre_269;

CREATE TABLE agents (
    id                              TEXT PRIMARY KEY,
    org_id                          TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    display_name                    TEXT NOT NULL DEFAULT 'Triage Factory Bot',
    default_model                   TEXT,
    default_autonomy_suitability    REAL,
    github_app_installation_id      TEXT,
    github_pat_user_id              TEXT REFERENCES users(id) ON DELETE SET NULL,
    jira_service_account_id         TEXT,
    created_at                      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at                      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (org_id)
);

-- Rewrite id to the new local sentinel — the pre-269 id was derived
-- from "default" via UUID5, which the post-269 BootstrapAgentID
-- (runmode.LocalDefaultOrgID branch) no longer produces. Existing
-- installs would otherwise have an unreachable row id. The
-- "WHERE id = (SELECT MIN(id) ...)" guard preserves "one local agent"
-- by picking exactly the pre-existing row (or no row if the table
-- was somehow empty, which would only happen if the SKY-260 bootstrap
-- never ran for some reason).
INSERT INTO agents (id, org_id, display_name, default_model, default_autonomy_suitability,
                    github_app_installation_id, github_pat_user_id, jira_service_account_id,
                    created_at, updated_at)
SELECT '00000000-0000-0000-0000-000000001000',
       '00000000-0000-0000-0000-000000000001',
       display_name,
       default_model,
       default_autonomy_suitability,
       github_app_installation_id,
       github_pat_user_id,
       jira_service_account_id,
       created_at,
       updated_at
FROM agents_pre_269
WHERE id = (SELECT MIN(id) FROM agents_pre_269);

DROP TABLE agents_pre_269;

-- team_agents: existing row keeps its team_id value (the synthetic
-- 'default' string from 202605120001), but we'd like the FK on the
-- new teams table. Backfill rewrites the team_id from 'default' to
-- the sentinel UUID so the FK resolves.

ALTER TABLE team_agents RENAME TO team_agents_pre_269;

CREATE TABLE team_agents (
    team_id                          TEXT NOT NULL REFERENCES teams(id)  ON DELETE CASCADE,
    agent_id                         TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    enabled                          INTEGER NOT NULL DEFAULT 1,
    per_team_model                   TEXT,
    per_team_autonomy_suitability    REAL,
    added_at                         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (team_id, agent_id)
);

-- Rewrite team_id ('default' → sentinel) AND agent_id (pre-269
-- derivation → LocalDefaultAgentID sentinel) so the new FK constraints
-- on both columns resolve. The team_agents row count is at most 1 in
-- practice (one bot per team in local mode).
INSERT INTO team_agents (team_id, agent_id, enabled, per_team_model,
                         per_team_autonomy_suitability, added_at)
SELECT CASE WHEN team_id = 'default'
            THEN '00000000-0000-0000-0000-000000000010'
            ELSE team_id END,
       '00000000-0000-0000-0000-000000001000',
       enabled, per_team_model, per_team_autonomy_suitability, added_at
FROM team_agents_pre_269;

DROP TABLE team_agents_pre_269;

CREATE INDEX IF NOT EXISTS idx_team_agents_agent ON team_agents(agent_id);

-- === Upgrade-path PAT backfill =============================================
-- A pre-269 install carries an agents row with github_pat_user_id NULL
-- (the column had no FK target locally back then). Post-269 the column
-- references users(id) — and Create's INSERT OR IGNORE shape means
-- BootstrapLocalAgent on a re-boot does NOT update the row's
-- credential fields when the UNIQUE(org_id) conflict fires. Without
-- this UPDATE, the upgrade path leaves github_pat_user_id NULL forever
-- even though the sentinel user is right there to point at.
--
-- The gate (both credential fields IS NULL) preserves any deliberate
-- user state: if someone had set an App install or a custom PAT-borrow
-- target pre-269 (which the SKY-260 SQLite schema did permit even
-- though it wasn't wired into Create's default path), we don't
-- clobber it.
UPDATE agents
SET github_pat_user_id = '00000000-0000-0000-0000-000000000100',
    updated_at = CURRENT_TIMESTAMP
WHERE org_id = '00000000-0000-0000-0000-000000000001'
  AND github_pat_user_id IS NULL
  AND github_app_installation_id IS NULL;

-- +goose Down
SELECT 'down not supported';
