package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// Server is the main HTTP server for Triage Factory.
type Server struct {
	db              *sql.DB
	prompts         db.PromptStore
	swipes          db.SwipeStore
	dashboard       db.DashboardStore
	eventHandlers   db.EventHandlerStore
	agents          db.AgentStore     // SKY-261 D-Claims: resolves the org's agent for claim stamps
	teamAgents      db.TeamAgentStore // SKY-261 D-Claims: re-checks team_agents.enabled on swipe-delegate / factory-delegate
	users           db.UsersStore     // SKY-264: github_username + display_name on the synthetic local user row
	chains          db.ChainStore
	mux             *http.ServeMux
	static          fs.FS
	ws              *websocket.Hub
	spawner         *delegate.Spawner
	curator         *curator.Curator
	ghClient        *ghclient.Client
	jiraClient      *jira.Client
	onGitHubChanged func() // GitHub creds/repos changed — full restart + re-profile
	onJiraChanged   func() // Jira config changed — restart Jira poller only
	scorerTrigger   func() // invoked after non-poll task creation (e.g. carry-over) to kick scoring immediately
	lifetimeCounter *db.LifetimeDistinctCounter

	// authDeps groups the multi-mode-only auth stack (JWKS verifier +
	// session store + gotrue HTTP client). Nil in local mode; checked
	// by middleware before any session lookup so local-mode boots
	// without dragging GoTrue into the dependency graph.
	authDeps  *authDeps
	authCfg   *authConfig
	authProxy http.Handler // /auth/v1/* → gotrue:9999/*

	// refreshLocks serializes inline JWT refreshes per session. Keyed by
	// session UUID → *sync.Mutex. Map grows monotonically with session
	// count (~8 bytes per entry); cleanup is left to process restart,
	// matching skynet/authkit's `self._locks` pattern. If memory ever
	// becomes a concern, the reaper goroutine could sweep entries for
	// revoked/expired sessions.
	refreshLocks sync.Map

	// Jira poll readiness — used by /api/jira/stock to decide whether the
	// poller has completed its first cycle after a restart. Carry-over reads
	// from the DB and needs snapshots to be populated before showing tickets.
	jiraPollMu      sync.RWMutex
	jiraRestartedAt time.Time
	jiraLastPollAt  time.Time

	// projectMutexes serializes PATCH-style read-merge-write
	// operations per project ID so two concurrent autosaves from
	// different widgets (e.g. pinned-repos editor and tracker
	// picker) can't lost-update each other. SQLite serializes
	// individual writes via MaxOpenConns=1, but that's not enough
	// here — handler A reads pre-A state, handler B reads pre-A
	// state, A writes, B writes B's merge over pre-A state, and
	// A's contribution is lost. Holding the per-project mutex
	// across the read+write window closes that hole.
	projectMutexes sync.Map // map[string]*sync.Mutex
}

// projectMutex returns the per-project mutex for serializing
// read-merge-write handlers. Created on demand via LoadOrStore; the
// map grows monotonically with project count, which is fine — they
// stay user-curated and small. Project deletion doesn't bother
// removing the entry: a stale mutex on a missing project is just
// unused memory, and the next call for that ID is a no-op.
func (s *Server) projectMutex(id string) *sync.Mutex {
	if v, ok := s.projectMutexes.Load(id); ok {
		return v.(*sync.Mutex)
	}
	v, _ := s.projectMutexes.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// agentEnabledForLocalTeam returns the resolved agent and whether the
// team_agents.enabled flag is true for it. Wraps the two-step lookup
// (Agents.GetForOrg → TeamAgents.GetForTeam) so swipe-delegate and
// factory-delegate share one code path for the SKY-261 acceptance
// rule "swipe-to-delegate re-checks team_agents.enabled at swipe
// time."
//
// Three outcomes the caller maps:
//   - (a, true, nil)  — proceed with the delegate.
//   - (a, false, nil) — bot disabled for this team; refuse with 409.
//   - (_, _, err)     — store error; refuse with 500.
//
// Nil agent (no bootstrap) returns err so the caller surfaces a
// distinguishable 500 message rather than a misleading "disabled"
// 409. Bootstrap is fatal at startup post-D-Claims so this is
// belt-and-suspenders for tests / degraded states.
func (s *Server) agentEnabledForLocalTeam(ctx context.Context) (*domain.Agent, bool, error) {
	if s.agents == nil {
		return nil, false, fmt.Errorf("agent store not configured")
	}
	a, err := s.agents.GetForOrg(ctx, runmode.LocalDefaultOrg)
	if err != nil {
		return nil, false, fmt.Errorf("agent lookup: %w", err)
	}
	if a == nil {
		return nil, false, fmt.Errorf("no agent bootstrapped — set up the bot first")
	}
	if s.teamAgents == nil {
		// Pre-D-Claims wiring (tests). Treat as enabled to preserve
		// the pre-flag behavior for any test path that hasn't wired
		// teamAgents yet.
		return a, true, nil
	}
	ta, err := s.teamAgents.GetForTeam(ctx, runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, a.ID)
	if err != nil {
		return a, false, fmt.Errorf("team_agents lookup: %w", err)
	}
	if ta == nil {
		// team_agents row missing — treat as disabled. Production
		// installs always have the row via BootstrapLocalAgent; a
		// missing row at runtime means something went sideways.
		return a, false, nil
	}
	return a, ta.Enabled, nil
}

// New creates a new server with the given database + the per-resource
// stores migrated under SKY-246, and registers all routes. The
// argument list grows one store at a time as their callers migrate;
// raw *sql.DB stays available for handlers that haven't been ported
// to a store yet.
func New(database *sql.DB, prompts db.PromptStore, swipes db.SwipeStore, dashboard db.DashboardStore, eventHandlers db.EventHandlerStore, agents db.AgentStore, teamAgents db.TeamAgentStore, users db.UsersStore, chains db.ChainStore) *Server {
	s := &Server{
		db:            database,
		prompts:       prompts,
		swipes:        swipes,
		dashboard:     dashboard,
		eventHandlers: eventHandlers,
		agents:        agents,
		teamAgents:    teamAgents,
		users:         users,
		chains:        chains,
		mux:           http.NewServeMux(),
		ws:            websocket.NewHub(),
	}
	s.routes()
	return s
}

// ListenAndServe starts the HTTP server on the given address.
func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.mux)
}

func (s *Server) routes() {
	// API routes
	// Integration credentials (GitHub PAT, Jira PAT). Distinct from the
	// session-auth routes below — these are per-user-stored credentials
	// for talking to third-party services on the user's behalf, not the
	// user's own login. Lived under /api/auth/* historically; renamed in
	// the post-SKY-251 cleanup so /api/auth/* unambiguously means
	// "session authentication." D9 wires its session middleware to
	// /api/* — including these, since you need to be logged in to
	// manage your integration credentials.
	s.mux.HandleFunc("POST /api/integrations/setup", s.handleIntegrationsSetup)
	s.mux.HandleFunc("GET /api/integrations/status", s.handleIntegrationsStatus)
	// DELETE on the collection = nuke all integration credentials.
	// Targeted clears (Jira only) get explicit subpaths.
	s.mux.HandleFunc("DELETE /api/integrations", s.handleIntegrationsClear)
	s.mux.HandleFunc("DELETE /api/integrations/jira", s.handleIntegrationsDeleteJira)

	// Multi-mode OAuth flow. Handlers 404 themselves when authDeps is
	// nil (local mode), so unconditional mount is safe — the routes
	// are inert until SetAuthDeps wires them.
	s.mux.HandleFunc("GET /api/auth/oauth/{provider}", s.handleOAuthStart)
	s.mux.HandleFunc("GET /api/auth/callback", s.handleOAuthCallback)
	// Logout is the only cookie-authed mutating endpoint in D7. Wrap
	// in the Origin-check middleware so a cross-site form POST can't
	// drive-by-log-the-user-out. D9 will apply the same wrapper to
	// every retrofitted mutating endpoint.
	s.mux.Handle("POST /api/auth/logout", s.withCSRFOriginCheck(http.HandlerFunc(s.handleLogout)))
	// Logout-everywhere: must be authenticated to use it (you can only
	// nuke your own sessions). Wrapped in withSession + the same
	// CSRF guard as /logout.
	s.mux.Handle("POST /api/auth/logout/all", s.withCSRFOriginCheck(s.withSession(http.HandlerFunc(s.handleLogoutAll))))
	// /api/me is the session-protected identity endpoint. withSession
	// passes through in local mode (no authDeps), so a local-mode
	// /api/me hit would reach the handler with nil claims and write
	// 401. The frontend in local mode shouldn't be calling this; D8
	// handles the conditional mount on the SPA side.
	s.mux.Handle("GET /api/me", s.withSession(http.HandlerFunc(s.handleMe)))

	// /auth/v1/* reverse-proxy to gotrue, wired lazily inside
	// SetAuthDeps. The closure here re-reads s.authProxy each
	// request so local-mode (where it stays nil) returns 404
	// rather than panicking, and multi-mode picks up the proxy
	// once SetAuthDeps completes.
	s.mux.Handle("/auth/v1/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authProxy == nil {
			http.NotFound(w, r)
			return
		}
		s.authProxy.ServeHTTP(w, r)
	}))

	s.mux.HandleFunc("GET /api/queue", s.handleQueue)
	s.mux.HandleFunc("GET /api/tasks", s.handleTasks)
	s.mux.HandleFunc("GET /api/tasks/{id}", s.handleTaskGet)
	s.mux.HandleFunc("POST /api/tasks/{id}/swipe", s.handleSwipe)
	s.mux.HandleFunc("POST /api/tasks/{id}/snooze", s.handleSnooze)
	s.mux.HandleFunc("POST /api/tasks/{id}/undo", s.handleUndo)
	s.mux.HandleFunc("POST /api/tasks/{id}/requeue", s.handleRequeue)

	s.mux.HandleFunc("GET /api/agent/runs/{runID}", s.handleAgentStatus)
	s.mux.HandleFunc("GET /api/agent/runs/{runID}/messages", s.handleAgentMessages)
	s.mux.HandleFunc("POST /api/agent/runs/{runID}/cancel", s.handleAgentCancel)
	s.mux.HandleFunc("POST /api/agent/runs/{runID}/takeover", s.handleAgentTakeover)
	s.mux.HandleFunc("POST /api/agent/runs/{runID}/release", s.handleAgentRelease)
	s.mux.HandleFunc("POST /api/agent/runs/{runID}/respond", s.handleAgentRespond)
	s.mux.HandleFunc("GET /api/agent/runs", s.handleAgentRuns)
	s.mux.HandleFunc("GET /api/agent/takeovers/held", s.handleHeldTakeovers)

	// Projects (SKY-215). Pure CRUD over the projects table; the
	// Curator runtime that populates curator_session_id lands in
	// SKY-216 and per-project entity classification in SKY-220.
	s.mux.HandleFunc("POST /api/projects", s.handleProjectCreate)
	s.mux.HandleFunc("GET /api/projects", s.handleProjectList)
	s.mux.HandleFunc("GET /api/projects/{id}", s.handleProjectGet)
	s.mux.HandleFunc("PATCH /api/projects/{id}", s.handleProjectUpdate)
	s.mux.HandleFunc("DELETE /api/projects/{id}", s.handleProjectDelete)
	s.mux.HandleFunc("GET /api/projects/{id}/export/preview", s.handleProjectExportPreview)
	s.mux.HandleFunc("GET /api/projects/{id}/export", s.handleProjectExport)
	s.mux.HandleFunc("POST /api/projects/import", s.handleProjectImport)
	s.mux.HandleFunc("GET /api/projects/{id}/knowledge", s.handleProjectKnowledge)
	s.mux.HandleFunc("POST /api/projects/{id}/knowledge", s.handleProjectKnowledgeUpload)
	s.mux.HandleFunc("GET /api/projects/{id}/knowledge/{path}", s.handleProjectKnowledgeFile)
	s.mux.HandleFunc("DELETE /api/projects/{id}/knowledge/{path}", s.handleProjectKnowledgeDelete)
	// Project-creation backfill popup (SKY-220 PR B).
	s.mux.HandleFunc("GET /api/projects/{id}/backfill-candidates", s.handleBackfillCandidates)
	s.mux.HandleFunc("POST /api/projects/{id}/backfill", s.handleBackfill)
	// Project entities panel (SKY-238).
	s.mux.HandleFunc("GET /api/projects/{id}/entities", s.handleProjectEntities)

	// Curator chat per project (SKY-216). The Curator package owns the
	// long-lived CC session lifecycle; these endpoints are the API
	// the Projects page (SKY-217) will hit.
	s.mux.HandleFunc("POST /api/projects/{id}/curator/messages", s.handleCuratorSend)
	s.mux.HandleFunc("GET /api/projects/{id}/curator/messages", s.handleCuratorHistory)
	s.mux.HandleFunc("DELETE /api/projects/{id}/curator/messages/in-flight", s.handleCuratorCancel)
	s.mux.HandleFunc("POST /api/projects/{id}/curator/reset", s.handleCuratorReset)

	// Websocket
	s.mux.HandleFunc("GET /api/ws", s.ws.HandleWS)

	s.mux.HandleFunc("GET /api/dashboard/stats", s.handleDashboardStats)
	s.mux.HandleFunc("GET /api/dashboard/prs", s.handleDashboardPRs)
	s.mux.HandleFunc("GET /api/dashboard/prs/{number}/status", s.handleDashboardPRStatus)
	s.mux.HandleFunc("POST /api/dashboard/prs/{number}/draft", s.handleDashboardPRDraft)

	s.mux.HandleFunc("GET /api/brief", s.handleBrief)
	s.mux.HandleFunc("GET /api/preferences", s.handlePreferences)

	s.mux.HandleFunc("GET /api/settings", s.handleSettingsGet)
	s.mux.HandleFunc("POST /api/settings", s.handleSettingsPost)

	// SKY-264: deployment shape + team roster for the predicate editor.
	// Both endpoints are fetched fresh on every consumer mount (the FE
	// hooks dedup concurrent in-flight calls within a render but don't
	// hold a persistent cache — current_user.github_username and the
	// roster are both mutable mid-session). Endpoint costs are a single
	// SELECT each, so re-fetching per editor mount is cheap and correct.
	s.mux.HandleFunc("GET /api/config", s.handleConfig)
	s.mux.HandleFunc("GET /api/team/members", s.handleTeamMembers)
	s.mux.HandleFunc("POST /api/skills/import", s.handleSkillsImport)
	s.mux.HandleFunc("GET /api/github/repos", s.handleGitHubRepos)
	s.mux.HandleFunc("POST /api/github/preflight-ssh", s.handleGitHubPreflightSSH)
	s.mux.HandleFunc("GET /api/repos", s.handleRepoProfiles)
	s.mux.HandleFunc("POST /api/repos", s.handleReposSave)
	s.mux.HandleFunc("PATCH /api/repos/{owner}/{repo}", s.handleRepoUpdate)
	s.mux.HandleFunc("GET /api/repos/{owner}/{repo}/branches", s.handleRepoBranches)
	s.mux.HandleFunc("POST /api/jira/connect", s.handleJiraConnect)
	s.mux.HandleFunc("GET /api/jira/statuses", s.handleJiraStatuses)
	s.mux.HandleFunc("GET /api/jira/stock", s.handleJiraStockGet)
	s.mux.HandleFunc("POST /api/jira/stock", s.handleJiraStockPost)

	s.mux.HandleFunc("GET /api/reviews/{id}", s.handleReviewGet)
	s.mux.HandleFunc("PATCH /api/reviews/{id}", s.handleReviewUpdate)
	s.mux.HandleFunc("GET /api/reviews/{id}/diff", s.handleReviewDiff)
	s.mux.HandleFunc("POST /api/reviews/{id}/submit", s.handleReviewSubmit)
	s.mux.HandleFunc("PUT /api/reviews/{id}/comments/{commentId}", s.handleReviewCommentUpdate)
	s.mux.HandleFunc("DELETE /api/reviews/{id}/comments/{commentId}", s.handleReviewCommentDelete)
	s.mux.HandleFunc("GET /api/agent/runs/{runID}/review", s.handleRunReview)

	s.mux.HandleFunc("GET /api/pending-prs/{id}", s.handlePendingPRGet)
	s.mux.HandleFunc("PATCH /api/pending-prs/{id}", s.handlePendingPRUpdate)
	s.mux.HandleFunc("GET /api/pending-prs/{id}/diff", s.handlePendingPRDiff)
	s.mux.HandleFunc("POST /api/pending-prs/{id}/submit", s.handlePendingPRSubmit)
	s.mux.HandleFunc("GET /api/agent/runs/{runID}/pending-pr", s.handleRunPendingPR)

	s.mux.HandleFunc("GET /api/factory/snapshot", s.handleFactorySnapshot)
	s.mux.HandleFunc("POST /api/factory/delegate", s.handleFactoryDelegate)

	s.mux.HandleFunc("GET /api/event-types", s.handleEventTypes)
	s.mux.HandleFunc("GET /api/event-schemas", s.handleEventSchemasList)
	s.mux.HandleFunc("GET /api/event-schemas/{event_type}", s.handleEventSchemaGet)
	// Unified event_handlers endpoints (SKY-259). Replace the former
	// /api/task-rules + /api/triggers split — kind is passed as ?kind=
	// on list, in the body on create, derived on update.
	s.mux.HandleFunc("GET /api/event-handlers", s.handleEventHandlersList)
	s.mux.HandleFunc("POST /api/event-handlers", s.handleEventHandlerCreate)
	s.mux.HandleFunc("PUT /api/event-handlers/reorder", s.handleEventHandlerReorder)
	s.mux.HandleFunc("PATCH /api/event-handlers/{id}", s.handleEventHandlerUpdate)
	s.mux.HandleFunc("PUT /api/event-handlers/{id}", s.handleEventHandlerUpdate)
	s.mux.HandleFunc("DELETE /api/event-handlers/{id}", s.handleEventHandlerDelete)
	s.mux.HandleFunc("POST /api/event-handlers/{id}/toggle", s.handleEventHandlerToggle)
	s.mux.HandleFunc("POST /api/event-handlers/{id}/promote", s.handleEventHandlerPromote)
	s.mux.HandleFunc("GET /api/prompts", s.handlePromptsList)
	s.mux.HandleFunc("POST /api/prompts", s.handlePromptCreate)
	s.mux.HandleFunc("GET /api/prompts/{id}", s.handlePromptGet)
	s.mux.HandleFunc("PUT /api/prompts/{id}", s.handlePromptPut)
	s.mux.HandleFunc("DELETE /api/prompts/{id}", s.handlePromptDelete)
	s.mux.HandleFunc("GET /api/prompts/{id}/stats", s.handlePromptStats)
	s.mux.HandleFunc("GET /api/prompts/{id}/chain-steps", s.handleChainStepsGet)
	s.mux.HandleFunc("PUT /api/prompts/{id}/chain-steps", s.handleChainStepsPut)
	s.mux.HandleFunc("GET /api/chain-runs/{id}", s.handleChainRunGet)
	s.mux.HandleFunc("POST /api/chain-runs/{id}/cancel", s.handleChainRunCancel)

	// Frontend: serve embedded SPA, with fallback to index.html for client-side routing
	s.mux.HandleFunc("/", s.handleFrontend)
}

// handleFrontend serves the embedded React SPA. Non-file requests fall back to index.html
// so that client-side routing works.
func (s *Server) handleFrontend(w http.ResponseWriter, r *http.Request) {
	if s.static == nil {
		http.Error(w, "frontend not built — run: cd frontend && npm run build", http.StatusNotFound)
		return
	}

	path := r.URL.Path
	if path == "/" {
		path = "index.html"
	} else {
		path = strings.TrimPrefix(path, "/")
	}

	// Try to serve the file directly
	if _, err := fs.Stat(s.static, path); err == nil {
		http.ServeFileFS(w, r, s.static, path)
		return
	}

	// Fallback to index.html for SPA client-side routing
	http.ServeFileFS(w, r, s.static, "index.html")
}

// SetStatic sets the embedded frontend filesystem.
func (s *Server) SetStatic(f fs.FS) {
	s.static = f
}

// SetSpawner sets the delegation spawner for agent runs.
func (s *Server) SetSpawner(sp *delegate.Spawner) {
	s.spawner = sp
}

// SetCurator wires the Curator runtime into the server so the
// /api/projects/{id}/curator/* endpoints can dispatch messages and
// the project-delete handler can cancel in-flight chats. Wired
// post-construction (mirrors SetSpawner) so main.go can build the
// Curator after the websocket hub is constructed.
func (s *Server) SetCurator(c *curator.Curator) {
	s.curator = c
}

// SetOnGitHubChanged registers a callback for GitHub config changes (creds, URL, repos).
// This triggers a full restart: invalidate profiles → stop all pollers → re-profile → restart.
func (s *Server) SetOnGitHubChanged(fn func()) {
	s.onGitHubChanged = fn
}

// SetOnJiraChanged registers a callback for Jira config changes.
// This restarts only the Jira poller.
func (s *Server) SetOnJiraChanged(fn func()) {
	s.onJiraChanged = fn
}

// SetScorerTrigger registers a callback to kick the AI scorer. Used by
// flows that create tasks outside the normal poll→event path (e.g.
// carry-over) so scoring starts immediately rather than waiting for the
// next poll cycle.
func (s *Server) SetScorerTrigger(fn func()) {
	s.scorerTrigger = fn
}

// SetLifetimeCounter wires the in-memory distinct-entity counter the
// factory snapshot reads. Hydrated once at startup and maintained by an
// eventbus subscriber; replaces the per-request COUNT(DISTINCT entity_id)
// scan that grew with the events table.
func (s *Server) SetLifetimeCounter(c *db.LifetimeDistinctCounter) {
	s.lifetimeCounter = c
}

// SetGitHubClient sets the GitHub client for review approval submissions.
func (s *Server) SetGitHubClient(client *ghclient.Client) {
	s.ghClient = client
}

// SetJiraClient sets the Jira client used by claim and undo handlers.
// Per-project in-progress rules are looked up via config.Load() at the
// use site (tasks.go) — projects can have different workflows and the
// right rule depends on the ticket's project_key.
func (s *Server) SetJiraClient(client *jira.Client) {
	s.jiraClient = client
}

// MarkJiraRestarted records the moment the Jira poller was restarted. Clears
// the last-poll timestamp so jiraPollReady reports false until a completion
// event arrives. Call this before kicking off a Jira poller restart.
func (s *Server) MarkJiraRestarted() {
	s.jiraPollMu.Lock()
	defer s.jiraPollMu.Unlock()
	s.jiraRestartedAt = time.Now()
	s.jiraLastPollAt = time.Time{}
}

// MarkJiraPollComplete records a successful Jira poll cycle. Call from the
// event-bus subscriber on system:poll:completed when source == "jira".
// startedAt is the wall-clock time the poll cycle started; completions from
// poll goroutines that started before the most recent MarkJiraRestarted are
// ignored so an in-flight pre-restart poll can't incorrectly flip readiness
// back to true.
//
// A zero startedAt means the emitter didn't supply a start time (metadata
// field missing or the event came from a publisher unaware of the race
// guard). Accept those completions so a malformed/future event can't leave
// carry-over stuck on {status:"polling"} indefinitely — race protection
// degrades gracefully rather than silently failing open.
func (s *Server) MarkJiraPollComplete(startedAt time.Time) {
	s.jiraPollMu.Lock()
	defer s.jiraPollMu.Unlock()
	if !startedAt.IsZero() && startedAt.Before(s.jiraRestartedAt) {
		return
	}
	s.jiraLastPollAt = time.Now()
}

// jiraPollReady returns true when the poller has completed at least one cycle
// since the last restart. Used by /api/jira/stock to gate the list response.
func (s *Server) jiraPollReady() bool {
	s.jiraPollMu.RLock()
	defer s.jiraPollMu.RUnlock()
	return !s.jiraLastPollAt.IsZero() && s.jiraLastPollAt.After(s.jiraRestartedAt)
}

// --- Stub handlers (to be implemented) ---

func (s *Server) handleBrief(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not yet implemented"})
}

func (s *Server) handlePreferences(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not yet implemented"})
}

// Prompt handlers are in prompts_handler.go
// Skill import handler is in skills_handler.go

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
