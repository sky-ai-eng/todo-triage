-- +goose Up
-- Consolidated baseline after the move to goose-managed migrations
-- (SKY-245 / D1). Captures the cumulative post-state of the 18
-- hand-rolled migrations that shipped before this file (preserved in
-- git history under internal/db/migrations/).
--
-- Existing installs are stamped at this version by the legacy-import
-- shim in migrations.go (any DB whose pre-goose `schema_migrations`
-- table contains rows is taken to already be at this state); the file
-- is only executed on fresh installs that have neither a
-- `goose_db_version` nor a populated `schema_migrations` table.
--
-- Future schema changes go in NEW NNN-numbered files alongside this
-- one — never edit this baseline. Down migrations are not supported
-- (see internal/db/migrations.go and SKY-245's spec); the trailing
-- Down block is a deliberate no-op.

-- === Prompts ==============================================================
CREATE TABLE IF NOT EXISTS prompts (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    body TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT 'user',
    usage_count INTEGER DEFAULT 0,
    hidden BOOLEAN DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    user_modified INTEGER NOT NULL DEFAULT 0,
    allowed_tools TEXT NOT NULL DEFAULT ''
);

-- === Events catalog (read-only system registry) ===========================
CREATE TABLE IF NOT EXISTS events_catalog (
    id TEXT PRIMARY KEY,
    source TEXT NOT NULL,
    category TEXT NOT NULL,
    label TEXT NOT NULL,
    description TEXT NOT NULL
);

-- === Projects =============================================================
-- Top-level concept that segments work items by *concept* rather than by
-- repo (SKY-211 / SKY-215). Created before entities so the entities FK
-- target exists at fresh-install CREATE time.
CREATE TABLE IF NOT EXISTS projects (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    curator_session_id TEXT,
    pinned_repos TEXT NOT NULL DEFAULT '[]',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    jira_project_key TEXT,
    linear_project_key TEXT,
    spec_authorship_prompt_id TEXT REFERENCES prompts(id) ON DELETE SET NULL
);

-- === Entities =============================================================
CREATE TABLE IF NOT EXISTS entities (
    id TEXT PRIMARY KEY,
    source TEXT NOT NULL,
    source_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    title TEXT,
    url TEXT,
    snapshot_json TEXT,
    description TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'active',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_polled_at DATETIME,
    closed_at DATETIME,
    project_id TEXT REFERENCES projects(id) ON DELETE SET NULL,
    classified_at DATETIME,
    classification_rationale TEXT,
    UNIQUE(source, source_id)
);

CREATE INDEX IF NOT EXISTS idx_entities_state ON entities(state);
CREATE INDEX IF NOT EXISTS idx_entities_source_polled ON entities(source, last_polled_at);
CREATE INDEX IF NOT EXISTS idx_entities_closed_at ON entities(closed_at)
    WHERE closed_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_entities_project_id ON entities(project_id)
    WHERE project_id IS NOT NULL;

-- === Entity links =========================================================
CREATE TABLE IF NOT EXISTS entity_links (
    from_entity_id TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    to_entity_id TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    origin TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (from_entity_id, to_entity_id, kind)
);

CREATE INDEX IF NOT EXISTS idx_entity_links_from_kind ON entity_links(from_entity_id, kind);
CREATE INDEX IF NOT EXISTS idx_entity_links_to_kind ON entity_links(to_entity_id, kind);

-- === Events (append-only audit log) =======================================
CREATE TABLE IF NOT EXISTS events (
    id TEXT PRIMARY KEY,
    entity_id TEXT REFERENCES entities(id),
    event_type TEXT NOT NULL REFERENCES events_catalog(id),
    dedup_key TEXT NOT NULL DEFAULT '',
    metadata_json TEXT,
    occurred_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_events_entity_created ON events(entity_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_type_created ON events(event_type, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_entity_occurred ON events(entity_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_type_entity ON events(event_type, entity_id)
    WHERE entity_id IS NOT NULL;

-- === Task rules (declarative task creation) ===============================
CREATE TABLE IF NOT EXISTS task_rules (
    id TEXT PRIMARY KEY,
    event_type TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
    scope_predicate_json TEXT,
    enabled BOOLEAN NOT NULL DEFAULT 1,
    name TEXT NOT NULL,
    default_priority REAL NOT NULL DEFAULT 0.5,
    sort_order INTEGER NOT NULL DEFAULT 0,
    source TEXT NOT NULL DEFAULT 'user',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_task_rules_event_type_enabled
    ON task_rules(event_type) WHERE enabled = 1;

-- === Prompt triggers (automation rules) ===================================
CREATE TABLE IF NOT EXISTS prompt_triggers (
    id TEXT PRIMARY KEY,
    prompt_id TEXT NOT NULL REFERENCES prompts(id) ON DELETE CASCADE,
    trigger_type TEXT NOT NULL DEFAULT 'event',
    event_type TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
    scope_predicate_json TEXT,
    breaker_threshold INTEGER NOT NULL DEFAULT 4,
    min_autonomy_suitability REAL NOT NULL DEFAULT 0.0,
    enabled BOOLEAN NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_prompt_triggers_prompt_event_trigger_unique
    ON prompt_triggers(prompt_id, event_type, trigger_type);
CREATE INDEX IF NOT EXISTS idx_prompt_triggers_event_type ON prompt_triggers(event_type) WHERE enabled = 1;
CREATE INDEX IF NOT EXISTS idx_prompt_triggers_prompt_created ON prompt_triggers(prompt_id, created_at);

-- === Tasks ================================================================
-- matched_repos / blocked_reason were dropped by SKY-233 (lazy Jira
-- worktrees) so they are absent from the consolidated baseline.
CREATE TABLE IF NOT EXISTS tasks (
    id TEXT PRIMARY KEY,
    entity_id TEXT NOT NULL REFERENCES entities(id),
    event_type TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
    dedup_key TEXT NOT NULL DEFAULT '',
    primary_event_id TEXT NOT NULL REFERENCES events(id),
    status TEXT NOT NULL DEFAULT 'queued',
    priority_score REAL,
    ai_summary TEXT,
    autonomy_suitability REAL,
    priority_reasoning TEXT,
    scoring_status TEXT NOT NULL DEFAULT 'pending',
    severity TEXT,
    relevance_reason TEXT,
    source_status TEXT,
    snooze_until DATETIME,
    close_reason TEXT,
    close_event_type TEXT REFERENCES events_catalog(id),
    closed_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_active_entity_event_dedup
    ON tasks(entity_id, event_type, dedup_key) WHERE status NOT IN ('done', 'dismissed');
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);
CREATE INDEX IF NOT EXISTS idx_tasks_entity ON tasks(entity_id);
CREATE INDEX IF NOT EXISTS idx_tasks_status_priority ON tasks(status, priority_score DESC);

-- === task_events (junction) ===============================================
CREATE TABLE IF NOT EXISTS task_events (
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    event_id TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (task_id, event_id)
);

CREATE INDEX IF NOT EXISTS idx_task_events_task ON task_events(task_id);
CREATE INDEX IF NOT EXISTS idx_task_events_event ON task_events(event_id);

-- === Runs =================================================================
-- memory_missing was dropped by SKY-204 (it drifted from run_memory ground
-- truth; the factory derives the same boolean from agent_content IS NULL).
CREATE TABLE IF NOT EXISTS runs (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    prompt_id TEXT NOT NULL REFERENCES prompts(id),
    trigger_id TEXT REFERENCES prompt_triggers(id),
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
    total_cost_usd REAL
);

CREATE INDEX IF NOT EXISTS idx_runs_task ON runs(task_id);
CREATE INDEX IF NOT EXISTS idx_runs_prompt_started ON runs(prompt_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_trigger ON runs(trigger_id);
CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status);

-- === Run artifacts ========================================================
CREATE TABLE IF NOT EXISTS run_artifacts (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    url TEXT,
    title TEXT,
    metadata_json TEXT,
    is_primary BOOLEAN NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_run_artifacts_primary_per_run
    ON run_artifacts(run_id) WHERE is_primary = 1;
CREATE INDEX IF NOT EXISTS idx_run_artifacts_run ON run_artifacts(run_id);
CREATE INDEX IF NOT EXISTS idx_run_artifacts_kind_created ON run_artifacts(kind, created_at DESC);

-- === Run messages =========================================================
CREATE TABLE IF NOT EXISTS run_messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    role TEXT NOT NULL,
    content TEXT,
    subtype TEXT DEFAULT 'text',
    tool_calls TEXT,
    tool_call_id TEXT,
    is_error BOOLEAN DEFAULT 0,
    metadata TEXT,
    model TEXT,
    input_tokens INTEGER,
    output_tokens INTEGER,
    cache_read_tokens INTEGER,
    cache_creation_tokens INTEGER,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_run_messages_run ON run_messages(run_id);

-- === Run memory ===========================================================
-- agent_content / human_content shape from SKY-204; agent_content is
-- nullable (NULL == "agent didn't comply with the memory gate").
CREATE TABLE IF NOT EXISTS run_memory (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    entity_id TEXT NOT NULL REFERENCES entities(id),
    agent_content TEXT,
    human_content TEXT,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(run_id)
);

CREATE INDEX IF NOT EXISTS idx_run_memory_entity_created ON run_memory(entity_id, created_at ASC);
CREATE INDEX IF NOT EXISTS idx_run_memory_run ON run_memory(run_id);

-- === Pending firings ======================================================
CREATE TABLE IF NOT EXISTS pending_firings (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_id TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    trigger_id TEXT NOT NULL REFERENCES prompt_triggers(id) ON DELETE CASCADE,
    triggering_event_id TEXT NOT NULL REFERENCES events(id),
    status TEXT NOT NULL DEFAULT 'pending',
    skip_reason TEXT,
    queued_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    drained_at DATETIME,
    fired_run_id TEXT REFERENCES runs(id)
);

CREATE INDEX IF NOT EXISTS idx_pending_firings_entity_pending
    ON pending_firings(entity_id, queued_at) WHERE status = 'pending';
CREATE UNIQUE INDEX IF NOT EXISTS idx_pending_firings_dedup
    ON pending_firings(task_id, trigger_id) WHERE status = 'pending';

-- === Run worktrees ========================================================
-- SKY-233: per-run materialized worktrees for lazy Jira delegation.
CREATE TABLE IF NOT EXISTS run_worktrees (
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    repo_id TEXT NOT NULL,
    path TEXT NOT NULL,
    feature_branch TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (run_id, repo_id)
);
CREATE INDEX IF NOT EXISTS idx_run_worktrees_run ON run_worktrees(run_id);

-- === Pending PRs ==========================================================
CREATE TABLE IF NOT EXISTS pending_prs (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL UNIQUE REFERENCES runs(id) ON DELETE CASCADE,
    owner TEXT NOT NULL,
    repo TEXT NOT NULL,
    head_branch TEXT NOT NULL,
    head_sha TEXT NOT NULL,
    base_branch TEXT NOT NULL,
    title TEXT NOT NULL,
    body TEXT,
    original_title TEXT,
    original_body TEXT,
    locked INTEGER NOT NULL DEFAULT 0,
    submitted_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    draft INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_pending_prs_run ON pending_prs(run_id);

-- === Swipe events =========================================================
CREATE TABLE IF NOT EXISTS swipe_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    action TEXT NOT NULL,
    hesitation_ms INTEGER,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_swipe_events_task ON swipe_events(task_id);
CREATE INDEX IF NOT EXISTS idx_swipe_events_action_created ON swipe_events(action, created_at);

-- === Poller state =========================================================
CREATE TABLE IF NOT EXISTS poller_state (
    source TEXT NOT NULL,
    source_id TEXT NOT NULL,
    state_json TEXT NOT NULL,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (source, source_id)
);

-- === Repo profiles ========================================================
CREATE TABLE IF NOT EXISTS repo_profiles (
    id TEXT PRIMARY KEY,
    owner TEXT NOT NULL,
    repo TEXT NOT NULL,
    description TEXT,
    has_readme BOOLEAN DEFAULT 0,
    has_claude_md BOOLEAN DEFAULT 0,
    has_agents_md BOOLEAN DEFAULT 0,
    profile_text TEXT,
    clone_url TEXT,
    default_branch TEXT,
    base_branch TEXT,
    profiled_at DATETIME,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    clone_status TEXT NOT NULL DEFAULT 'pending',
    clone_error TEXT,
    clone_error_kind TEXT,
    UNIQUE(owner, repo)
);

CREATE INDEX IF NOT EXISTS idx_repo_profiles_owner_repo ON repo_profiles(owner, repo);

-- === Pending reviews ======================================================
CREATE TABLE IF NOT EXISTS pending_reviews (
    id TEXT PRIMARY KEY,
    pr_number INTEGER NOT NULL,
    owner TEXT NOT NULL,
    repo TEXT NOT NULL,
    commit_sha TEXT NOT NULL,
    diff_lines TEXT,
    run_id TEXT,
    review_body TEXT,
    review_event TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    original_review_body TEXT,
    original_review_event TEXT,
    diff_hunks TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS pending_review_comments (
    id TEXT PRIMARY KEY,
    review_id TEXT NOT NULL REFERENCES pending_reviews(id),
    path TEXT NOT NULL,
    line INTEGER NOT NULL,
    start_line INTEGER,
    body TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    original_body TEXT
);

CREATE INDEX IF NOT EXISTS idx_pending_review_comments_review_id ON pending_review_comments(review_id);

-- === Preferences ==========================================================
CREATE TABLE IF NOT EXISTS preferences (
    id INTEGER PRIMARY KEY,
    summary_md TEXT,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- === System prompt versions ===============================================
CREATE TABLE IF NOT EXISTS system_prompt_versions (
    prompt_id TEXT PRIMARY KEY REFERENCES prompts(id) ON DELETE CASCADE,
    content_hash TEXT NOT NULL,
    applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- === Curator requests / messages / pending context ========================
CREATE TABLE IF NOT EXISTS curator_requests (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    status TEXT NOT NULL DEFAULT 'queued',
    user_input TEXT NOT NULL,
    error_msg TEXT,
    cost_usd REAL NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    num_turns INTEGER NOT NULL DEFAULT 0,
    started_at DATETIME,
    finished_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_curator_requests_project_created
    ON curator_requests(project_id, created_at);
CREATE INDEX IF NOT EXISTS idx_curator_requests_in_flight
    ON curator_requests(project_id)
    WHERE status IN ('queued', 'running');

CREATE TABLE IF NOT EXISTS curator_messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id TEXT NOT NULL REFERENCES curator_requests(id) ON DELETE CASCADE,
    role TEXT NOT NULL,
    subtype TEXT NOT NULL DEFAULT 'text',
    content TEXT NOT NULL DEFAULT '',
    tool_calls TEXT,
    tool_call_id TEXT,
    is_error BOOLEAN NOT NULL DEFAULT 0,
    metadata TEXT,
    model TEXT,
    input_tokens INTEGER,
    output_tokens INTEGER,
    cache_read_tokens INTEGER,
    cache_creation_tokens INTEGER,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_curator_messages_request_created
    ON curator_messages(request_id, created_at, id);

CREATE TABLE IF NOT EXISTS curator_pending_context (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    curator_session_id TEXT NOT NULL,
    change_type TEXT NOT NULL,
    baseline_value TEXT NOT NULL,
    consumed_at DATETIME,
    consumed_by_request_id TEXT REFERENCES curator_requests(id) ON DELETE SET NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_curator_pending_context_one_pending_per_type
    ON curator_pending_context(project_id, curator_session_id, change_type)
    WHERE consumed_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_curator_pending_context_consumer
    ON curator_pending_context(consumed_by_request_id)
    WHERE consumed_by_request_id IS NOT NULL;

-- === Settings (singleton) =================================================
CREATE TABLE IF NOT EXISTS settings (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    data       TEXT NOT NULL,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- +goose Down
SELECT 'down not supported';
