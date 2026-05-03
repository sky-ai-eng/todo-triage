-- Curator runtime schema (SKY-216). Owns the long-lived Claude Code
-- session per project, plus the per-message exchange log that the
-- Projects page (SKY-217) renders as a chat history.
--
-- Three things happen here:
--
--   (1) projects.designer_session_id → curator_session_id. The original
--       name was a leftover from an earlier "Designer" sketch; the
--       column has the same purpose ("the CC session id this project's
--       Curator resumes against") but the new name is the one this
--       runtime actually uses. SQLite's ALTER TABLE RENAME COLUMN is
--       idempotent here because the migration runner only runs each
--       migration once.
--
--   (2) curator_requests — one row per user→agent exchange. Mirrors the
--       runs table's role: status + accounting + error rollup. A new
--       row is inserted as `queued` the moment the user posts a
--       message; the per-project goroutine flips it to `running` when
--       it picks up the request, and to a terminal status when
--       agentproc returns. Cost / duration / num_turns are written at
--       termination so historical replay matches what was billed.
--
--   (3) curator_messages — stream output, one row per accumulated
--       assistant or tool message, mirroring run_messages. The user's
--       own input is stored on curator_requests.user_input rather than
--       as a row here, so this table is purely the agent's side of the
--       exchange. ON DELETE CASCADE from curator_requests so deleting
--       a request (rare, but possible for cleanup) takes its messages
--       with it; CASCADE on project_id (transitively, via the request
--       FK) keeps the on-delete semantics consistent with what
--       SKY-215 set up for entities.

ALTER TABLE projects RENAME COLUMN designer_session_id TO curator_session_id;

CREATE TABLE IF NOT EXISTS curator_requests (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- status vocabulary:
    --   queued    — inserted, waiting for the per-project goroutine to pick up
    --   running   — agentproc.Run is in flight
    --   done      — agent emitted a result event; treated as a successful turn
    --   cancelled — user fired the cancel endpoint, or project was deleted mid-turn
    --   failed    — agentproc returned an error or no result was produced
    -- The per-project goroutine is the sole writer of every transition
    -- after `queued`; the cancel endpoint goes through the goroutine's
    -- ctx so we don't have two writers racing on a single row.
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

-- Drives the chat-history fetch: "give me the requests for project X
-- in chronological order." Created_at is monotonic per project so a
-- single ORDER BY created_at ASC is enough for the pager.
CREATE INDEX IF NOT EXISTS idx_curator_requests_project_created
    ON curator_requests(project_id, created_at);

-- Drives the cancel endpoint: "is there a non-terminal request for
-- this project?" Partial index keeps the footprint proportional to
-- in-flight + queued rows, not historical.
CREATE INDEX IF NOT EXISTS idx_curator_requests_in_flight
    ON curator_requests(project_id)
    WHERE status IN ('queued', 'running');

CREATE TABLE IF NOT EXISTS curator_messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id TEXT NOT NULL REFERENCES curator_requests(id) ON DELETE CASCADE,
    role TEXT NOT NULL,
    subtype TEXT NOT NULL DEFAULT 'text',
    content TEXT NOT NULL DEFAULT '',
    tool_calls TEXT,         -- JSON array, mirrors run_messages.tool_calls
    tool_call_id TEXT,
    is_error BOOLEAN NOT NULL DEFAULT 0,
    metadata TEXT,           -- JSON, reserved
    model TEXT,
    input_tokens INTEGER,
    output_tokens INTEGER,
    cache_read_tokens INTEGER,
    cache_creation_tokens INTEGER,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Drives "stream messages for this request in order" — the only read
-- pattern this table has today.
CREATE INDEX IF NOT EXISTS idx_curator_messages_request_created
    ON curator_messages(request_id, created_at, id);
