-- +goose Up
-- Multi-tenant Postgres baseline (SKY-247 / D3).
--
-- Targets the supabase/postgres:15.1.0.147 image, which pre-creates the
-- auth + vault + extensions schemas and pre-loads supabase_vault,
-- pgsodium, pgcrypto, pgjwt, uuid-ossp, pg_graphql. We CREATE EXTENSION
-- defensively but it's a no-op in practice.
--
-- Structure (mirrors spec §10 step 3):
--   (a) tf schema
--   (b) tf_app role + grants
--   (c) Vault wrappers
--   (d) RLS helpers
--   (e) tenancy tables
--   (f) update_project_knowledge OCC function
--   (g) settings tables
--   (h) TF tables in multi shape
--   (i) REVOKE on global ref tables
--   (j) indexes
--   (k) ENABLE RLS + per-table policies
--   (l) seed data
--
-- Future Postgres schema changes go in NEW migration files, never edit
-- this baseline. Down is a no-op (see migrations.go).

-- (a) ----------------------------------------------------------------
CREATE SCHEMA IF NOT EXISTS tf;

-- (b) ----------------------------------------------------------------
-- Idempotent role creation. The image ships `authenticator` (LOGIN,
-- NOINHERIT); we add `tf_app` (NOLOGIN, NOINHERIT, BYPASSRLS=false)
-- and let authenticator switch into it via SET LOCAL ROLE.
-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'tf_app') THEN
    CREATE ROLE tf_app NOLOGIN NOINHERIT;
  END IF;
END
$$;
-- +goose StatementEnd

GRANT tf_app TO authenticator;

GRANT USAGE ON SCHEMA public, tf TO tf_app;

-- Future tables created in public by `postgres` inherit these grants.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO tf_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
  GRANT USAGE, SELECT ON SEQUENCES TO tf_app;

-- (c) ----------------------------------------------------------------
-- Defensive — image already loads this.
CREATE EXTENSION IF NOT EXISTS supabase_vault WITH SCHEMA vault;

-- Org-prefixed secret naming makes cross-org leakage structurally
-- impossible at the Vault layer.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.vault_put_org_secret(
  p_org_id      UUID,
  p_key         TEXT,
  p_secret      TEXT,
  p_description TEXT DEFAULT NULL
) RETURNS UUID
LANGUAGE plpgsql SECURITY DEFINER
SET search_path TO public, vault, pg_temp
AS $$
DECLARE
  v_full_name TEXT := 'org/' || p_org_id::text || '/' || p_key;
  v_existing  UUID;
  -- vault.secrets.description is NOT NULL; coalesce NULL → '' so callers
  -- can pass NULL ergonomically.
  v_desc      TEXT := COALESCE(p_description, '');
BEGIN
  SELECT id INTO v_existing FROM vault.decrypted_secrets WHERE name = v_full_name;
  IF v_existing IS NOT NULL THEN
    PERFORM vault.update_secret(v_existing, p_secret, v_full_name, v_desc);
    RETURN v_existing;
  END IF;
  RETURN vault.create_secret(p_secret, v_full_name, v_desc);
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.vault_get_org_secret(
  p_org_id UUID,
  p_key    TEXT
) RETURNS TEXT
LANGUAGE plpgsql SECURITY DEFINER
SET search_path TO public, vault, pg_temp
AS $$
DECLARE
  v_full_name TEXT := 'org/' || p_org_id::text || '/' || p_key;
  v_secret    TEXT;
BEGIN
  SELECT decrypted_secret INTO v_secret
    FROM vault.decrypted_secrets
   WHERE name = v_full_name;
  RETURN v_secret;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.vault_delete_org_secret(
  p_org_id UUID,
  p_key    TEXT
) RETURNS BOOLEAN
LANGUAGE plpgsql SECURITY DEFINER
SET search_path TO public, vault, pg_temp
AS $$
DECLARE
  v_full_name TEXT := 'org/' || p_org_id::text || '/' || p_key;
  v_existing  UUID;
BEGIN
  SELECT id INTO v_existing FROM vault.decrypted_secrets WHERE name = v_full_name;
  IF v_existing IS NULL THEN
    RETURN FALSE;
  END IF;
  DELETE FROM vault.secrets WHERE id = v_existing;
  RETURN TRUE;
END;
$$;
-- +goose StatementEnd

-- REVOKE from PUBLIC AND from the supabase auto-grant targets
-- (anon/authenticated/service_role). The supabase image installs
-- event triggers that grant EXECUTE on new public.* functions to
-- those roles automatically; a bare "REVOKE FROM PUBLIC" leaves the
-- explicit per-role grants intact and anon can still call the
-- wrapper. Order: REVOKE first (against the auto-grants that fired
-- at CREATE FUNCTION time), then GRANT to tf_app.
REVOKE ALL ON FUNCTION public.vault_put_org_secret    FROM PUBLIC, anon, authenticated, service_role;
REVOKE ALL ON FUNCTION public.vault_get_org_secret    FROM PUBLIC, anon, authenticated, service_role;
REVOKE ALL ON FUNCTION public.vault_delete_org_secret FROM PUBLIC, anon, authenticated, service_role;
GRANT EXECUTE ON FUNCTION public.vault_put_org_secret    TO tf_app;
GRANT EXECUTE ON FUNCTION public.vault_get_org_secret    TO tf_app;
GRANT EXECUTE ON FUNCTION public.vault_delete_org_secret TO tf_app;

-- (d) ----------------------------------------------------------------
-- Claim-reading helpers come first; user_has_org_access lives below
-- the tenancy tables (it references memberships + teams, which
-- LANGUAGE SQL parses at function-creation time).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tf.current_user_id() RETURNS UUID
LANGUAGE SQL STABLE
AS $$
  SELECT NULLIF(current_setting('request.jwt.claims', true)::jsonb ->> 'sub', '')::uuid;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tf.current_org_id() RETURNS UUID
LANGUAGE SQL STABLE
AS $$
  SELECT NULLIF(current_setting('request.jwt.claims', true)::jsonb ->> 'org_id', '')::uuid;
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION tf.current_user_id FROM PUBLIC;
REVOKE ALL ON FUNCTION tf.current_org_id  FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tf.current_user_id TO tf_app;
GRANT EXECUTE ON FUNCTION tf.current_org_id  TO tf_app;

-- (e) ----------------------------------------------------------------
-- Tenancy tables. orgs.owner_user_id ↔ users.default_org_id form a
-- chicken-and-egg cycle; we create both without the cross-FK, then
-- attach the constraints at the bottom of this section.

CREATE TABLE users (
  id              UUID PRIMARY KEY REFERENCES auth.users(id) ON DELETE CASCADE,
  display_name    TEXT,
  avatar_url      TEXT,
  timezone        TEXT NOT NULL DEFAULT 'UTC',
  default_org_id  UUID,
  external_id     TEXT,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_external_id_idx ON users(external_id) WHERE external_id IS NOT NULL;

CREATE TABLE orgs (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  slug             TEXT NOT NULL UNIQUE,
  name             TEXT NOT NULL,
  description      TEXT,
  billing_email    TEXT,
  owner_user_id    UUID NOT NULL REFERENCES users(id),
  sso_provider_id  UUID,   -- FK to auth.sso_providers added in follow-up after D6
  sso_enforced     BOOLEAN NOT NULL DEFAULT FALSE,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at       TIMESTAMPTZ
);

ALTER TABLE users
  ADD CONSTRAINT users_default_org_id_fkey
  FOREIGN KEY (default_org_id) REFERENCES orgs(id) ON DELETE SET NULL;

CREATE TABLE teams (
  id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id              UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  slug                TEXT NOT NULL,
  name                TEXT NOT NULL,
  description         TEXT,
  created_by_user_id  UUID REFERENCES users(id) ON DELETE SET NULL,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, slug)
);

CREATE TYPE membership_role AS ENUM ('owner', 'admin', 'member', 'viewer');

CREATE TABLE memberships (
  user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  team_id    UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
  role       membership_role NOT NULL DEFAULT 'member',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, team_id)
);

-- user_has_org_access — joins memberships+teams. MUST be
-- SECURITY DEFINER: without it, the function call inside an RLS policy
-- on (say) tasks would invoke RLS on memberships/teams, whose own
-- policies invoke this function again, recursing until stack
-- exhaustion. SECURITY DEFINER runs the body as the function owner
-- (postgres superuser, who bypasses RLS), breaking the cycle.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tf.user_has_org_access(target_org UUID) RETURNS BOOLEAN
LANGUAGE SQL STABLE SECURITY DEFINER
SET search_path = public, tf, pg_temp
AS $$
  SELECT EXISTS (
    SELECT 1 FROM memberships m
    JOIN teams t ON t.id = m.team_id
    WHERE m.user_id = tf.current_user_id() AND t.org_id = target_org
  );
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION tf.user_has_org_access FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tf.user_has_org_access TO tf_app;

-- AES-GCM ciphertext columns; key = TF_SESSION_KEY env var (D6 wires it).
CREATE TABLE sessions (
  id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id           UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  jwt_enc           BYTEA NOT NULL,
  jwt_nonce         BYTEA NOT NULL,
  refresh_token_enc BYTEA NOT NULL,
  refresh_nonce     BYTEA NOT NULL,
  jwt_expires_at    TIMESTAMPTZ NOT NULL,
  expires_at        TIMESTAMPTZ NOT NULL,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at        TIMESTAMPTZ,
  user_agent        TEXT,
  ip_addr           INET,
  CHECK (expires_at > created_at),
  CHECK (jwt_expires_at <= expires_at)
);

-- (f) ----------------------------------------------------------------
-- project_knowledge + OCC versioning. Created late because RLS policies
-- below reference projects (created in section h).

-- (g) ----------------------------------------------------------------
CREATE TABLE org_settings (
  org_id                                UUID PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
  github_base_url                       TEXT,
  github_poll_interval                  INTERVAL NOT NULL DEFAULT '5 minutes',
  github_clone_protocol                 TEXT NOT NULL DEFAULT 'https' CHECK (github_clone_protocol IN ('https','ssh')),
  jira_base_url                         TEXT,
  jira_poll_interval                    INTERVAL NOT NULL DEFAULT '5 minutes',
  ai_reprioritize_threshold_default     INT,
  ai_preference_update_interval_default INTERVAL,
  updated_at                            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE team_settings (
  team_id                       UUID PRIMARY KEY REFERENCES teams(id) ON DELETE CASCADE,
  jira_projects                 TEXT[] NOT NULL DEFAULT '{}',
  ai_reprioritize_threshold     INT,
  ai_preference_update_interval INTERVAL,
  updated_at                    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE user_settings (
  user_id                  UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  ai_model                 TEXT NOT NULL DEFAULT 'haiku',
  ai_auto_delegate_enabled BOOLEAN NOT NULL DEFAULT FALSE,
  updated_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE jira_project_status_rules (
  org_id                UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  project_key           TEXT NOT NULL,
  pickup_members        TEXT[] NOT NULL DEFAULT '{}',
  in_progress_members   TEXT[] NOT NULL DEFAULT '{}',
  in_progress_canonical TEXT,
  done_members          TEXT[] NOT NULL DEFAULT '{}',
  done_canonical        TEXT,
  updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, project_key)
);

-- (h) ----------------------------------------------------------------
-- TF tables in multi shape. Every org-scoped table gets:
--   org_id UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE
-- Where applicable, also:
--   creator_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE
--   visibility TEXT NOT NULL DEFAULT 'private'
--   team_id UUID REFERENCES teams(id) ON DELETE SET NULL
-- SQLite TEXT→UUID/TEXT, DATETIME→TIMESTAMPTZ, JSON-text→JSONB,
-- INTEGER PK AUTOINCREMENT→BIGSERIAL, BOOLEAN DEFAULT 0→BOOLEAN DEFAULT FALSE.

-- Global reference tables — no org_id, RLS off.
CREATE TABLE events_catalog (
  id          TEXT PRIMARY KEY,
  source      TEXT NOT NULL,
  category    TEXT NOT NULL,
  label       TEXT NOT NULL,
  description TEXT NOT NULL
);

-- Prompts — org/team/user-scoped with visibility toggle.
CREATE TABLE prompts (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id          UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  creator_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  team_id         UUID REFERENCES teams(id) ON DELETE SET NULL,
  visibility      TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private','team','org')),
  name            TEXT NOT NULL,
  body            TEXT NOT NULL,
  source          TEXT NOT NULL DEFAULT 'user',
  usage_count     INTEGER NOT NULL DEFAULT 0,
  hidden          BOOLEAN NOT NULL DEFAULT FALSE,
  user_modified   BOOLEAN NOT NULL DEFAULT FALSE,
  allowed_tools   TEXT NOT NULL DEFAULT '',
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- system_prompt_versions — read-only global ref; written only by migrations.
CREATE TABLE system_prompt_versions (
  prompt_id    UUID PRIMARY KEY REFERENCES prompts(id) ON DELETE CASCADE,
  content_hash TEXT NOT NULL,
  applied_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE projects (
  id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id                    UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  creator_user_id           UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  team_id                   UUID REFERENCES teams(id) ON DELETE SET NULL,
  visibility                TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private','team','org')),
  name                      TEXT NOT NULL,
  description               TEXT NOT NULL DEFAULT '',
  curator_session_id        TEXT,
  pinned_repos              JSONB NOT NULL DEFAULT '[]'::jsonb,
  jira_project_key          TEXT,
  linear_project_key        TEXT,
  spec_authorship_prompt_id UUID REFERENCES prompts(id) ON DELETE SET NULL,
  created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- project_knowledge — durable curator artifact, org-shared with OCC.
CREATE TABLE project_knowledge (
  id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id              UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  project_id          UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  key                 TEXT NOT NULL,
  content             TEXT NOT NULL DEFAULT '',
  version             INT NOT NULL DEFAULT 1,
  last_updated_by     UUID REFERENCES users(id) ON DELETE SET NULL,
  last_updated_by_run UUID,                                            -- FK added when runs exists below
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (project_id, key)
);

-- entities — org-shared infrastructure, no creator scope.
CREATE TABLE entities (
  id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id                   UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  source                   TEXT NOT NULL,
  source_id                TEXT NOT NULL,
  kind                     TEXT NOT NULL,
  title                    TEXT,
  url                      TEXT,
  snapshot_json            JSONB,
  description              TEXT NOT NULL DEFAULT '',
  state                    TEXT NOT NULL DEFAULT 'active',
  project_id               UUID REFERENCES projects(id) ON DELETE SET NULL,
  classified_at            TIMESTAMPTZ,
  classification_rationale TEXT,
  last_polled_at           TIMESTAMPTZ,
  closed_at                TIMESTAMPTZ,
  created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, source, source_id)
);

CREATE TABLE entity_links (
  from_entity_id UUID NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
  to_entity_id   UUID NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
  kind           TEXT NOT NULL,
  origin         TEXT NOT NULL,
  org_id         UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (from_entity_id, to_entity_id, kind)
);

CREATE TABLE events (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id        UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  entity_id     UUID REFERENCES entities(id),
  event_type    TEXT NOT NULL REFERENCES events_catalog(id),
  dedup_key     TEXT NOT NULL DEFAULT '',
  metadata_json JSONB,
  occurred_at   TIMESTAMPTZ,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE task_rules (
  id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id               UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  creator_user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  team_id              UUID REFERENCES teams(id) ON DELETE SET NULL,
  visibility           TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private','team','org')),
  event_type           TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
  scope_predicate_json JSONB,
  enabled              BOOLEAN NOT NULL DEFAULT TRUE,
  name                 TEXT NOT NULL,
  default_priority     REAL NOT NULL DEFAULT 0.5,
  sort_order           INT NOT NULL DEFAULT 0,
  source               TEXT NOT NULL DEFAULT 'user',
  created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE prompt_triggers (
  id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id                   UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  creator_user_id          UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  team_id                  UUID REFERENCES teams(id) ON DELETE SET NULL,
  visibility               TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private','team','org')),
  prompt_id                UUID NOT NULL REFERENCES prompts(id) ON DELETE CASCADE,
  trigger_type             TEXT NOT NULL DEFAULT 'event',
  event_type               TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
  scope_predicate_json     JSONB,
  breaker_threshold        INT NOT NULL DEFAULT 4,
  min_autonomy_suitability REAL NOT NULL DEFAULT 0.0,
  enabled                  BOOLEAN NOT NULL DEFAULT TRUE,
  created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE tasks (
  id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id               UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  creator_user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  team_id              UUID REFERENCES teams(id) ON DELETE SET NULL,
  visibility           TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private','team','org')),
  entity_id            UUID NOT NULL REFERENCES entities(id),
  event_type           TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
  dedup_key            TEXT NOT NULL DEFAULT '',
  primary_event_id     UUID NOT NULL REFERENCES events(id),
  status               TEXT NOT NULL DEFAULT 'queued',
  priority_score       REAL,
  ai_summary           TEXT,
  autonomy_suitability REAL,
  priority_reasoning   TEXT,
  scoring_status       TEXT NOT NULL DEFAULT 'pending',
  severity             TEXT,
  relevance_reason     TEXT,
  source_status        TEXT,
  snooze_until         TIMESTAMPTZ,
  close_reason         TEXT,
  close_event_type     TEXT REFERENCES events_catalog(id),
  closed_at            TIMESTAMPTZ,
  created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE task_events (
  task_id    UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  event_id   UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
  org_id     UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  kind       TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (task_id, event_id)
);

CREATE TABLE runs (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id          UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  creator_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  team_id         UUID REFERENCES teams(id) ON DELETE SET NULL,
  visibility      TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private','team','org')),
  task_id         UUID NOT NULL REFERENCES tasks(id),
  prompt_id       UUID NOT NULL REFERENCES prompts(id),
  trigger_id      UUID REFERENCES prompt_triggers(id),
  trigger_type    TEXT NOT NULL DEFAULT 'manual',
  status          TEXT NOT NULL DEFAULT 'cloning',
  model           TEXT,
  session_id      TEXT,
  worktree_path   TEXT,
  result_summary  TEXT,
  stop_reason     TEXT,
  started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  completed_at    TIMESTAMPTZ,
  duration_ms     INT,
  num_turns       INT,
  total_cost_usd  REAL
);

-- Now that runs exists, attach the FK for project_knowledge.last_updated_by_run.
ALTER TABLE project_knowledge
  ADD CONSTRAINT project_knowledge_last_updated_by_run_fkey
  FOREIGN KEY (last_updated_by_run) REFERENCES runs(id) ON DELETE SET NULL;

CREATE TABLE run_artifacts (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id        UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  run_id        UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  kind          TEXT NOT NULL,
  url           TEXT,
  title         TEXT,
  metadata_json JSONB,
  is_primary    BOOLEAN NOT NULL DEFAULT FALSE,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE run_messages (
  id                    BIGSERIAL PRIMARY KEY,
  org_id                UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  run_id                UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  role                  TEXT NOT NULL,
  content               TEXT,
  subtype               TEXT DEFAULT 'text',
  tool_calls            JSONB,
  tool_call_id          TEXT,
  is_error              BOOLEAN NOT NULL DEFAULT FALSE,
  metadata              JSONB,
  model                 TEXT,
  input_tokens          INT,
  output_tokens         INT,
  cache_read_tokens     INT,
  cache_creation_tokens INT,
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE run_memory (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id        UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  run_id        UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  entity_id     UUID NOT NULL REFERENCES entities(id),
  agent_content TEXT,
  human_content TEXT,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (run_id)
);

CREATE TABLE pending_firings (
  id                  BIGSERIAL PRIMARY KEY,
  org_id              UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  creator_user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  entity_id           UUID NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
  task_id             UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  trigger_id          UUID NOT NULL REFERENCES prompt_triggers(id) ON DELETE CASCADE,
  triggering_event_id UUID NOT NULL REFERENCES events(id),
  status              TEXT NOT NULL DEFAULT 'pending',
  skip_reason         TEXT,
  queued_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  drained_at          TIMESTAMPTZ,
  fired_run_id        UUID REFERENCES runs(id)
);

CREATE TABLE run_worktrees (
  run_id         UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  org_id         UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  repo_id        TEXT NOT NULL,
  path           TEXT NOT NULL,
  feature_branch TEXT NOT NULL,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (run_id, repo_id)
);

CREATE TABLE pending_prs (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id         UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  run_id         UUID NOT NULL UNIQUE REFERENCES runs(id) ON DELETE CASCADE,
  owner          TEXT NOT NULL,
  repo           TEXT NOT NULL,
  head_branch    TEXT NOT NULL,
  head_sha       TEXT NOT NULL,
  base_branch    TEXT NOT NULL,
  title          TEXT NOT NULL,
  body           TEXT,
  original_title TEXT,
  original_body  TEXT,
  locked         BOOLEAN NOT NULL DEFAULT FALSE,
  draft          BOOLEAN NOT NULL DEFAULT FALSE,
  submitted_at   TIMESTAMPTZ,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE swipe_events (
  id              BIGSERIAL PRIMARY KEY,
  org_id          UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  creator_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  task_id         UUID NOT NULL REFERENCES tasks(id),
  action          TEXT NOT NULL,
  hesitation_ms   INT,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE poller_state (
  org_id     UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  source     TEXT NOT NULL,
  source_id  TEXT NOT NULL,
  state_json JSONB NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, source, source_id)
);

CREATE TABLE repo_profiles (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id           UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  owner            TEXT NOT NULL,
  repo             TEXT NOT NULL,
  description      TEXT,
  has_readme       BOOLEAN NOT NULL DEFAULT FALSE,
  has_claude_md    BOOLEAN NOT NULL DEFAULT FALSE,
  has_agents_md    BOOLEAN NOT NULL DEFAULT FALSE,
  profile_text     TEXT,
  clone_url        TEXT,
  default_branch   TEXT,
  base_branch      TEXT,
  clone_status     TEXT NOT NULL DEFAULT 'pending',
  clone_error      TEXT,
  clone_error_kind TEXT,
  profiled_at      TIMESTAMPTZ,
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, owner, repo)
);

CREATE TABLE pending_reviews (
  id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id                UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  pr_number             INT NOT NULL,
  owner                 TEXT NOT NULL,
  repo                  TEXT NOT NULL,
  commit_sha            TEXT NOT NULL,
  diff_lines            TEXT,
  run_id                UUID REFERENCES runs(id) ON DELETE SET NULL,
  review_body           TEXT,
  review_event          TEXT,
  original_review_body  TEXT,
  original_review_event TEXT,
  diff_hunks            TEXT NOT NULL DEFAULT '',
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE pending_review_comments (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id        UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  review_id     UUID NOT NULL REFERENCES pending_reviews(id) ON DELETE CASCADE,
  path          TEXT NOT NULL,
  line          INT NOT NULL,
  start_line    INT,
  body          TEXT NOT NULL,
  original_body TEXT,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- preferences — per-user AI behavioral preferences, no org scope (one
-- profile per user, spanning all their orgs).
CREATE TABLE preferences (
  user_id    UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  summary_md TEXT,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Curator: per-user-per-project chat; writes hit org-shared project_knowledge.
CREATE TABLE curator_requests (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id          UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  creator_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  project_id      UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  status          TEXT NOT NULL DEFAULT 'queued',
  user_input      TEXT NOT NULL,
  error_msg       TEXT,
  cost_usd        REAL NOT NULL DEFAULT 0,
  duration_ms     INT NOT NULL DEFAULT 0,
  num_turns       INT NOT NULL DEFAULT 0,
  started_at      TIMESTAMPTZ,
  finished_at     TIMESTAMPTZ,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE curator_messages (
  id                    BIGSERIAL PRIMARY KEY,
  org_id                UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  creator_user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  request_id            UUID NOT NULL REFERENCES curator_requests(id) ON DELETE CASCADE,
  role                  TEXT NOT NULL,
  subtype               TEXT NOT NULL DEFAULT 'text',
  content               TEXT NOT NULL DEFAULT '',
  tool_calls            JSONB,
  tool_call_id          TEXT,
  is_error              BOOLEAN NOT NULL DEFAULT FALSE,
  metadata              JSONB,
  model                 TEXT,
  input_tokens          INT,
  output_tokens         INT,
  cache_read_tokens     INT,
  cache_creation_tokens INT,
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE curator_pending_context (
  id                     BIGSERIAL PRIMARY KEY,
  org_id                 UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  creator_user_id        UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  project_id             UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  curator_session_id     TEXT NOT NULL,
  change_type            TEXT NOT NULL,
  baseline_value         TEXT NOT NULL,
  consumed_at            TIMESTAMPTZ,
  consumed_by_request_id UUID REFERENCES curator_requests(id) ON DELETE SET NULL,
  created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- update_project_knowledge OCC function — compare-and-swap on version.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.update_project_knowledge(
  p_id               UUID,
  p_expected_version INT,
  p_content          TEXT,
  p_updated_by       UUID,
  p_updated_by_run   UUID DEFAULT NULL
) RETURNS INT
LANGUAGE plpgsql SECURITY INVOKER
AS $$
DECLARE
  v_new_version INT;
BEGIN
  UPDATE project_knowledge
     SET content = p_content,
         version = version + 1,
         last_updated_by = p_updated_by,
         last_updated_by_run = p_updated_by_run,
         updated_at = now()
   WHERE id = p_id
     AND version = p_expected_version
  RETURNING version INTO v_new_version;

  IF v_new_version IS NULL THEN
    RAISE EXCEPTION 'concurrent update of project_knowledge %', p_id
      USING ERRCODE = '40001';
  END IF;
  RETURN v_new_version;
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION public.update_project_knowledge FROM PUBLIC, anon, authenticated, service_role;
GRANT EXECUTE ON FUNCTION public.update_project_knowledge TO tf_app;

-- (i) ----------------------------------------------------------------
-- Global ref tables are read-only for tf_app. Migration writes seed
-- rows; application code never INSERTs.
REVOKE INSERT, UPDATE, DELETE ON events_catalog          FROM tf_app;
REVOKE INSERT, UPDATE, DELETE ON system_prompt_versions  FROM tf_app;

-- (j) ----------------------------------------------------------------
-- Indexes. Org-scoped tables get org_id-leading variants; partial
-- indexes preserve the SQLite predicate; FK-based access patterns keep
-- the FK column as the lead.

CREATE INDEX idx_entities_org_state           ON entities(org_id, state);
CREATE INDEX idx_entities_org_source_polled   ON entities(org_id, source, last_polled_at);
CREATE INDEX idx_entities_closed_at           ON entities(closed_at) WHERE closed_at IS NOT NULL;
CREATE INDEX idx_entities_project_id          ON entities(project_id) WHERE project_id IS NOT NULL;

CREATE INDEX idx_entity_links_from_kind ON entity_links(from_entity_id, kind);
CREATE INDEX idx_entity_links_to_kind   ON entity_links(to_entity_id, kind);

CREATE INDEX idx_events_org_entity_created ON events(org_id, entity_id, created_at DESC);
CREATE INDEX idx_events_org_type_created   ON events(org_id, event_type, created_at DESC);
CREATE INDEX idx_events_org_entity_occurred ON events(org_id, entity_id, occurred_at DESC);
CREATE INDEX idx_events_type_entity        ON events(event_type, entity_id) WHERE entity_id IS NOT NULL;

CREATE INDEX idx_task_rules_org_event_enabled
    ON task_rules(org_id, event_type) WHERE enabled = TRUE;

CREATE UNIQUE INDEX idx_prompt_triggers_prompt_event_trigger_unique
    ON prompt_triggers(prompt_id, event_type, trigger_type);
CREATE INDEX idx_prompt_triggers_org_event ON prompt_triggers(org_id, event_type) WHERE enabled = TRUE;
CREATE INDEX idx_prompt_triggers_prompt_created ON prompt_triggers(prompt_id, created_at);

CREATE UNIQUE INDEX idx_tasks_active_entity_event_dedup
    ON tasks(entity_id, event_type, dedup_key) WHERE status NOT IN ('done', 'dismissed');
CREATE INDEX idx_tasks_org_status          ON tasks(org_id, status);
CREATE INDEX idx_tasks_entity              ON tasks(entity_id);
CREATE INDEX idx_tasks_org_status_priority ON tasks(org_id, status, priority_score DESC);

CREATE INDEX idx_task_events_task  ON task_events(task_id);
CREATE INDEX idx_task_events_event ON task_events(event_id);

CREATE INDEX idx_runs_task            ON runs(task_id);
CREATE INDEX idx_runs_prompt_started  ON runs(prompt_id, started_at DESC);
CREATE INDEX idx_runs_trigger         ON runs(trigger_id);
CREATE INDEX idx_runs_org_status      ON runs(org_id, status);

CREATE UNIQUE INDEX idx_run_artifacts_primary_per_run ON run_artifacts(run_id) WHERE is_primary = TRUE;
CREATE INDEX idx_run_artifacts_run                    ON run_artifacts(run_id);
CREATE INDEX idx_run_artifacts_kind_created           ON run_artifacts(kind, created_at DESC);

CREATE INDEX idx_run_messages_run ON run_messages(run_id);

CREATE INDEX idx_run_memory_entity_created ON run_memory(entity_id, created_at ASC);
CREATE INDEX idx_run_memory_run            ON run_memory(run_id);

CREATE INDEX idx_pending_firings_entity_pending ON pending_firings(entity_id, queued_at) WHERE status = 'pending';
CREATE UNIQUE INDEX idx_pending_firings_dedup   ON pending_firings(task_id, trigger_id) WHERE status = 'pending';

CREATE INDEX idx_run_worktrees_run ON run_worktrees(run_id);

CREATE INDEX idx_pending_prs_run ON pending_prs(run_id);

CREATE INDEX idx_swipe_events_task           ON swipe_events(task_id);
CREATE INDEX idx_swipe_events_action_created ON swipe_events(action, created_at);

CREATE INDEX idx_repo_profiles_org_owner_repo ON repo_profiles(org_id, owner, repo);

CREATE INDEX idx_pending_review_comments_review_id ON pending_review_comments(review_id);

CREATE INDEX idx_curator_requests_project_created ON curator_requests(project_id, created_at);
CREATE INDEX idx_curator_requests_in_flight        ON curator_requests(project_id) WHERE status IN ('queued', 'running');

CREATE INDEX idx_curator_messages_request_created ON curator_messages(request_id, created_at, id);

CREATE UNIQUE INDEX idx_curator_pending_context_one_pending_per_type
    ON curator_pending_context(project_id, curator_session_id, change_type) WHERE consumed_at IS NULL;
CREATE INDEX idx_curator_pending_context_consumer
    ON curator_pending_context(consumed_by_request_id) WHERE consumed_by_request_id IS NOT NULL;

CREATE INDEX project_knowledge_org_idx ON project_knowledge(org_id, project_id);

-- (k) ----------------------------------------------------------------
-- Enable RLS on every org-scoped table. Policies follow.
-- Tenancy + settings:
ALTER TABLE orgs                       ENABLE ROW LEVEL SECURITY;
ALTER TABLE teams                      ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships                ENABLE ROW LEVEL SECURITY;
ALTER TABLE sessions                   ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_settings               ENABLE ROW LEVEL SECURITY;
ALTER TABLE team_settings              ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_settings              ENABLE ROW LEVEL SECURITY;
ALTER TABLE jira_project_status_rules  ENABLE ROW LEVEL SECURITY;
ALTER TABLE preferences                ENABLE ROW LEVEL SECURITY;
-- TF data tables:
ALTER TABLE prompts                    ENABLE ROW LEVEL SECURITY;
ALTER TABLE projects                   ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_knowledge          ENABLE ROW LEVEL SECURITY;
ALTER TABLE entities                   ENABLE ROW LEVEL SECURITY;
ALTER TABLE entity_links               ENABLE ROW LEVEL SECURITY;
ALTER TABLE events                     ENABLE ROW LEVEL SECURITY;
ALTER TABLE task_rules                 ENABLE ROW LEVEL SECURITY;
ALTER TABLE prompt_triggers            ENABLE ROW LEVEL SECURITY;
ALTER TABLE tasks                      ENABLE ROW LEVEL SECURITY;
ALTER TABLE task_events                ENABLE ROW LEVEL SECURITY;
ALTER TABLE runs                       ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_artifacts              ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_messages               ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_memory                 ENABLE ROW LEVEL SECURITY;
ALTER TABLE pending_firings            ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_worktrees              ENABLE ROW LEVEL SECURITY;
ALTER TABLE pending_prs                ENABLE ROW LEVEL SECURITY;
ALTER TABLE swipe_events               ENABLE ROW LEVEL SECURITY;
ALTER TABLE poller_state               ENABLE ROW LEVEL SECURITY;
ALTER TABLE repo_profiles              ENABLE ROW LEVEL SECURITY;
ALTER TABLE pending_reviews            ENABLE ROW LEVEL SECURITY;
ALTER TABLE pending_review_comments    ENABLE ROW LEVEL SECURITY;
ALTER TABLE curator_requests           ENABLE ROW LEVEL SECURITY;
ALTER TABLE curator_messages           ENABLE ROW LEVEL SECURITY;
ALTER TABLE curator_pending_context    ENABLE ROW LEVEL SECURITY;

-- Tenancy + auth policies:
CREATE POLICY orgs_select ON orgs FOR SELECT
  USING (id = tf.current_org_id() AND tf.user_has_org_access(id));
CREATE POLICY orgs_update ON orgs FOR UPDATE
  USING (id = tf.current_org_id() AND tf.user_has_org_access(id));

CREATE POLICY teams_select ON teams FOR SELECT
  USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));
CREATE POLICY teams_modify ON teams FOR ALL
  USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id))
  WITH CHECK (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));

CREATE POLICY memberships_select ON memberships FOR SELECT
  USING (user_id = tf.current_user_id()
         OR EXISTS (SELECT 1 FROM teams t WHERE t.id = team_id AND t.org_id = tf.current_org_id()
                                              AND tf.user_has_org_access(t.org_id)));

CREATE POLICY sessions_select ON sessions FOR SELECT USING (user_id = tf.current_user_id());
CREATE POLICY sessions_modify ON sessions FOR ALL    USING (user_id = tf.current_user_id())
                                                      WITH CHECK (user_id = tf.current_user_id());

CREATE POLICY user_settings_select ON user_settings FOR SELECT USING (user_id = tf.current_user_id());
CREATE POLICY user_settings_modify ON user_settings FOR ALL    USING (user_id = tf.current_user_id())
                                                                WITH CHECK (user_id = tf.current_user_id());

CREATE POLICY preferences_select ON preferences FOR SELECT USING (user_id = tf.current_user_id());
CREATE POLICY preferences_modify ON preferences FOR ALL    USING (user_id = tf.current_user_id())
                                                            WITH CHECK (user_id = tf.current_user_id());

CREATE POLICY org_settings_select ON org_settings FOR SELECT
  USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));
CREATE POLICY org_settings_modify ON org_settings FOR ALL
  USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id))
  WITH CHECK (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));

CREATE POLICY team_settings_select ON team_settings FOR SELECT
  USING (EXISTS (SELECT 1 FROM teams t WHERE t.id = team_id AND t.org_id = tf.current_org_id()
                                              AND tf.user_has_org_access(t.org_id)));
CREATE POLICY team_settings_modify ON team_settings FOR ALL
  USING (EXISTS (SELECT 1 FROM teams t WHERE t.id = team_id AND t.org_id = tf.current_org_id()
                                              AND tf.user_has_org_access(t.org_id)))
  WITH CHECK (EXISTS (SELECT 1 FROM teams t WHERE t.id = team_id AND t.org_id = tf.current_org_id()
                                                  AND tf.user_has_org_access(t.org_id)));

CREATE POLICY jira_rules_select ON jira_project_status_rules FOR SELECT
  USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));
CREATE POLICY jira_rules_modify ON jira_project_status_rules FOR ALL
  USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id))
  WITH CHECK (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));

-- Per-user resources: creator-scoped read + write. v1 defaults to private;
-- visibility column lets v2 elevate to team/org without an ALTER.
CREATE POLICY prompts_select ON prompts FOR SELECT
  USING (org_id = tf.current_org_id()
         AND (creator_user_id = tf.current_user_id()
              OR (visibility = 'team' AND team_id IS NOT NULL
                  AND EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = tf.current_user_id() AND m.team_id = team_id))
              OR visibility = 'org'));
CREATE POLICY prompts_modify ON prompts FOR ALL
  USING (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id())
  WITH CHECK (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id()
              AND tf.user_has_org_access(org_id));

CREATE POLICY projects_select ON projects FOR SELECT
  USING (org_id = tf.current_org_id()
         AND (creator_user_id = tf.current_user_id() OR visibility IN ('team','org')));
CREATE POLICY projects_modify ON projects FOR ALL
  USING (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id())
  WITH CHECK (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id()
              AND tf.user_has_org_access(org_id));

-- Org-shared resources (no creator scope): every org member reads/writes.
CREATE POLICY project_knowledge_all ON project_knowledge FOR ALL
  USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id))
  WITH CHECK (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));

CREATE POLICY entities_all     ON entities     FOR ALL USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id))
                                                       WITH CHECK (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));
CREATE POLICY entity_links_all ON entity_links FOR ALL USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id))
                                                       WITH CHECK (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));
CREATE POLICY events_all       ON events       FOR ALL USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id))
                                                       WITH CHECK (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));
CREATE POLICY repo_profiles_all ON repo_profiles FOR ALL USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id))
                                                         WITH CHECK (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));
CREATE POLICY poller_state_all ON poller_state FOR ALL USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id))
                                                       WITH CHECK (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));

-- Creator-scoped resources (task_rules, prompt_triggers).
CREATE POLICY task_rules_select ON task_rules FOR SELECT
  USING (org_id = tf.current_org_id()
         AND (creator_user_id = tf.current_user_id() OR visibility IN ('team','org')));
CREATE POLICY task_rules_modify ON task_rules FOR ALL
  USING (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id())
  WITH CHECK (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id()
              AND tf.user_has_org_access(org_id));

CREATE POLICY prompt_triggers_select ON prompt_triggers FOR SELECT
  USING (org_id = tf.current_org_id()
         AND (creator_user_id = tf.current_user_id() OR visibility IN ('team','org')));
CREATE POLICY prompt_triggers_modify ON prompt_triggers FOR ALL
  USING (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id())
  WITH CHECK (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id()
              AND tf.user_has_org_access(org_id));

CREATE POLICY tasks_select ON tasks FOR SELECT
  USING (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id());
CREATE POLICY tasks_modify ON tasks FOR ALL
  USING (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id())
  WITH CHECK (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id()
              AND tf.user_has_org_access(org_id));

CREATE POLICY runs_select ON runs FOR SELECT
  USING (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id());
CREATE POLICY runs_modify ON runs FOR ALL
  USING (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id())
  WITH CHECK (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id()
              AND tf.user_has_org_access(org_id));

-- Resources denormalized from parents (org_id baked for RLS efficiency).
-- Visibility inherited from parent via app code; RLS gates on org_id alone.
CREATE POLICY task_events_all     ON task_events FOR ALL USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id))
                                                         WITH CHECK (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));
CREATE POLICY run_artifacts_all   ON run_artifacts FOR ALL USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id))
                                                            WITH CHECK (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));
CREATE POLICY run_messages_all    ON run_messages FOR ALL USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id))
                                                           WITH CHECK (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));
CREATE POLICY run_memory_all      ON run_memory FOR ALL USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id))
                                                         WITH CHECK (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));
CREATE POLICY pending_firings_all ON pending_firings FOR ALL USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id))
                                                              WITH CHECK (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));
CREATE POLICY run_worktrees_all   ON run_worktrees FOR ALL USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id))
                                                            WITH CHECK (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));
CREATE POLICY pending_prs_all     ON pending_prs FOR ALL USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id))
                                                          WITH CHECK (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));
CREATE POLICY swipe_events_select ON swipe_events FOR SELECT
  USING (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id());
CREATE POLICY swipe_events_modify ON swipe_events FOR ALL
  USING (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id())
  WITH CHECK (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id()
              AND tf.user_has_org_access(org_id));
CREATE POLICY pending_reviews_all          ON pending_reviews FOR ALL USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id))
                                                                       WITH CHECK (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));
CREATE POLICY pending_review_comments_all  ON pending_review_comments FOR ALL USING (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id))
                                                                               WITH CHECK (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));

-- Curator: per-user-per-project chat (creator-scoped).
CREATE POLICY curator_requests_select ON curator_requests FOR SELECT
  USING (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id());
CREATE POLICY curator_requests_modify ON curator_requests FOR ALL
  USING (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id())
  WITH CHECK (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id()
              AND tf.user_has_org_access(org_id));
CREATE POLICY curator_messages_select ON curator_messages FOR SELECT
  USING (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id());
CREATE POLICY curator_messages_modify ON curator_messages FOR ALL
  USING (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id())
  WITH CHECK (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id()
              AND tf.user_has_org_access(org_id));
CREATE POLICY curator_pending_context_select ON curator_pending_context FOR SELECT
  USING (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id());
CREATE POLICY curator_pending_context_modify ON curator_pending_context FOR ALL
  USING (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id())
  WITH CHECK (org_id = tf.current_org_id() AND creator_user_id = tf.current_user_id()
              AND tf.user_has_org_access(org_id));

-- (l) ----------------------------------------------------------------
-- Seed events_catalog. Mirrors domain.AllEventTypes() — hand-transcribed
-- because plpgsql can't call Go. Drift surfaces via TestSeedData
-- asserting events_catalog rowcount == len(domain.AllEventTypes()).
INSERT INTO events_catalog (id, source, category, label, description) VALUES
  ('github:pr:review_changes_requested', 'github', 'pr', 'Changes Requested',  'A reviewer requested changes on a PR'),
  ('github:pr:review_approved',          'github', 'pr', 'Review: Approved',   'A reviewer approved a PR'),
  ('github:pr:review_commented',         'github', 'pr', 'Review: Commented',  'A reviewer left non-blocking comments on a PR'),
  ('github:pr:review_dismissed',         'github', 'pr', 'Review: Dismissed',  'A reviewer dismissed their previous review on a PR'),
  ('github:pr:review_requested',         'github', 'pr', 'Review Requested',   'Someone requested your review on a PR'),
  ('github:pr:review_submitted',         'github', 'pr', 'Review Submitted',   'I reviewed someone else''s PR (inverse of review_*)'),
  ('github:pr:review_request_removed',   'github', 'pr', 'Review Request Removed', 'Your review request was removed from a PR (review completed or request rescinded)'),
  ('github:pr:ci_check_failed',          'github', 'pr', 'CI Check Failed',    'A CI check failed on a PR'),
  ('github:pr:ci_check_passed',          'github', 'pr', 'CI Check Passed',    'A CI check passed on a PR'),
  ('github:pr:label_added',              'github', 'pr', 'Label Added',        'A label was added to a PR'),
  ('github:pr:label_removed',            'github', 'pr', 'Label Removed',      'A label was removed from a PR'),
  ('github:pr:new_commits',              'github', 'pr', 'New Commits',        'A tracked PR has new commits since the last poll'),
  ('github:pr:conflicts',                'github', 'pr', 'Merge Conflicts',    'A PR has merge conflicts'),
  ('github:pr:ready_for_review',         'github', 'pr', 'Ready for Review',   'A draft PR was marked ready for review'),
  ('github:pr:mentioned',                'github', 'pr', 'Mentioned',          'You were @mentioned in a PR'),
  ('github:pr:opened',                   'github', 'pr', 'PR Opened',          'A pull request was opened'),
  ('github:pr:merged',                   'github', 'pr', 'PR Merged',          'A pull request was merged'),
  ('github:pr:closed',                   'github', 'pr', 'PR Closed',          'A pull request was closed without merging'),
  ('jira:issue:assigned',                'jira',   'issue', 'Issue Assigned',  'Issue was assigned to you'),
  ('jira:issue:available',               'jira',   'issue', 'Issue Available', 'New unassigned issue in pickup queue'),
  ('jira:issue:status_changed',          'jira',   'issue', 'Status Changed',  'Issue status changed (uses dedup_key=new_status)'),
  ('jira:issue:priority_changed',        'jira',   'issue', 'Priority Changed','Issue priority was changed (uses dedup_key=new_priority)'),
  ('jira:issue:commented',               'jira',   'issue', 'New Comment',     'A new comment was added to an issue'),
  ('jira:issue:completed',               'jira',   'issue', 'Issue Completed', 'Issue was marked as done'),
  ('jira:issue:became_atomic',           'jira',   'issue', 'Issue Became Atomic', 'Last open subtask closed — parent is now an atomic work unit'),
  ('system:poll:completed',              'system', 'poll', 'Poll Complete',    'A poller finished a cycle'),
  ('system:scoring:completed',           'system', 'scoring', 'Scoring Complete', 'AI scoring finished for a task'),
  ('system:delegation:completed',        'system', 'delegation', 'Delegation Complete', 'Agent delegation run completed'),
  ('system:delegation:failed',           'system', 'delegation', 'Delegation Failed',   'Agent delegation run failed'),
  ('system:prompt:auto_suspended',       'system', 'delegation', 'Prompt Auto-suspended', 'Per-(entity, prompt) breaker tripped after repeated failures'),
  ('system:task:delegation_blocked_by_subtasks', 'system', 'delegation', 'Delegation Blocked: Subtasks', 'Auto-delegation skipped because parent has open subtasks');

-- +goose Down
SELECT 'down not supported';
