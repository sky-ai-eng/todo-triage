package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// newTestServer spins up an in-memory SQLite with the full schema +
// events catalog seed, registers all HTTP routes, and returns the Server.
// Each test gets its own DB so there's no cross-contamination.
//
// Pre-SKY-259 this helper lived in task_rules_handler_test.go; after the
// unification it sits in a dedicated test_helpers_test.go so any
// handler-level test can use it without depending on a specific feature's
// test file.
func newTestServer(t *testing.T) *Server {
	t.Helper()

	database, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })

	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	// Settings handlers go through config.Load/Save, which require an
	// initialized package handle. We deliberately skip MigrateLegacyYAML
	// so tests don't read or delete the developer's real config.yaml.
	if err := config.Init(database); err != nil {
		t.Fatalf("config init: %v", err)
	}
	// SKY-261 B+: swipe-delegate and factory_delegate both call
	// Agents.GetForOrg to stamp claim. Without an agents row, those
	// paths return 500 with "no agent bootstrapped." Seed the local
	// sentinel agent row so handler tests reach the actual logic
	// under test rather than short-circuiting on agent bootstrap.
	if _, err := database.Exec(
		`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed local agent: %v", err)
	}
	// SKY-261 C work: handlers now re-check team_agents.enabled before
	// stamping the bot claim (the spec's bot-disabled-team handling).
	// Production seeds this via BootstrapLocalAgent; tests need the
	// same row or every delegate gesture 409s.
	if _, err := database.Exec(
		`INSERT OR IGNORE INTO team_agents (team_id, agent_id, enabled) VALUES (?, ?, 1)`,
		runmode.LocalDefaultTeamID, runmode.LocalDefaultAgentID,
	); err != nil {
		t.Fatalf("seed local team_agents: %v", err)
	}
	stores := sqlitestore.New(database)
	return New(database, stores.Prompts, stores.Swipes, stores.Dashboard, stores.EventHandlers, stores.Agents, stores.TeamAgents)
}

// doJSON performs a JSON request against the server's mux and returns
// the response. Body may be nil.
func doJSON(t *testing.T, s *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var reqBody *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reqBody = bytes.NewReader(b)
	} else {
		reqBody = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, path, reqBody)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// seedConfiguredRepo inserts a minimal repo_profiles row so tests that
// pin repos pass the validatePinnedRepos existence check. The Curator's
// repo-materialization eventually wants more (clone_url, default_branch),
// but for HTTP-handler tests this is the smallest seed that satisfies
// the validation contract.
func seedConfiguredRepo(t *testing.T, s *Server, owner, repo string) {
	t.Helper()
	if err := db.UpsertRepoProfile(s.db, domain.RepoProfile{
		ID:            owner + "/" + repo,
		Owner:         owner,
		Repo:          repo,
		DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("seed configured repo %s/%s: %v", owner, repo, err)
	}
}
