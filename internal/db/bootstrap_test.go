package db_test

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestBootstrapLocalAgent_FreshInstall pins the v1 install path:
// after migrations, BootstrapLocalAgent inserts exactly one agents
// row + one team_agents row in the synthetic local org/team.
func TestBootstrapLocalAgent_FreshInstall(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()

	if err := db.BootstrapLocalAgent(ctx, stores); err != nil {
		t.Fatalf("BootstrapLocalAgent: %v", err)
	}

	agent, err := stores.Agents.GetForOrg(ctx, runmode.LocalDefaultOrg)
	if err != nil {
		t.Fatalf("GetForOrg: %v", err)
	}
	if agent == nil {
		t.Fatal("no agents row after bootstrap")
	}
	if agent.DisplayName != "Triage Factory Bot" {
		t.Errorf("DisplayName=%q want Triage Factory Bot", agent.DisplayName)
	}

	ta, err := stores.TeamAgents.GetForTeam(ctx, runmode.LocalDefaultOrg, db.LocalDefaultTeamID, agent.ID)
	if err != nil {
		t.Fatalf("GetForTeam: %v", err)
	}
	if ta == nil {
		t.Fatal("no team_agents row after bootstrap")
	}
	if !ta.Enabled {
		t.Errorf("team_agents.enabled=false; want true (DEFAULT TRUE)")
	}
}

// TestBootstrapLocalAgent_Idempotent pins the v1.10.1 → current
// upgrade path: BootstrapLocalAgent runs every boot, second + Nth
// calls are no-ops (row count stays at 1).
func TestBootstrapLocalAgent_Idempotent(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()

	for i := 0; i < 3; i++ {
		if err := db.BootstrapLocalAgent(ctx, stores); err != nil {
			t.Fatalf("BootstrapLocalAgent call %d: %v", i, err)
		}
	}

	var agentCount int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM agents`).Scan(&agentCount); err != nil {
		t.Fatalf("count agents: %v", err)
	}
	if agentCount != 1 {
		t.Errorf("agents row count=%d after 3 bootstrap calls; want 1", agentCount)
	}
	var teamAgentCount int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM team_agents`).Scan(&teamAgentCount); err != nil {
		t.Fatalf("count team_agents: %v", err)
	}
	if teamAgentCount != 1 {
		t.Errorf("team_agents row count=%d after 3 bootstrap calls; want 1", teamAgentCount)
	}
}

// TestBootstrapLocalAgent_PreservesUserDisable pins the load-bearing
// invariant for migrating users: if a user has disabled the bot for
// their team, re-running bootstrap on the next boot must NOT flip
// Enabled back to TRUE.
func TestBootstrapLocalAgent_PreservesUserDisable(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()

	if err := db.BootstrapLocalAgent(ctx, stores); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	agent, _ := stores.Agents.GetForOrg(ctx, runmode.LocalDefaultOrg)
	if agent == nil {
		t.Fatal("no agent after bootstrap")
	}
	// User disables the bot.
	if err := stores.TeamAgents.SetEnabled(ctx, runmode.LocalDefaultOrg, db.LocalDefaultTeamID, agent.ID, false); err != nil {
		t.Fatalf("SetEnabled false: %v", err)
	}
	// Simulate a restart.
	if err := db.BootstrapLocalAgent(ctx, stores); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	ta, _ := stores.TeamAgents.GetForTeam(ctx, runmode.LocalDefaultOrg, db.LocalDefaultTeamID, agent.ID)
	if ta == nil {
		t.Fatal("team_agents row vanished")
	}
	if ta.Enabled {
		t.Fatal("bootstrap re-enabled the bot; user's disable would leak away across boots")
	}
}

// TestBootstrapTeamAgent_ErrorsWhenOrgHasNoAgent pins the sequencing
// guard: a caller that wires team-create before org-create gets a
// clear error rather than a silent "team has no bot" outcome.
func TestBootstrapTeamAgent_ErrorsWhenOrgHasNoAgent(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()

	err := db.BootstrapTeamAgent(ctx, stores, runmode.LocalDefaultOrg, "some-team")
	if err == nil {
		t.Fatal("BootstrapTeamAgent without prior org bootstrap returned nil; want explicit error")
	}
	if !strings.Contains(err.Error(), "no agent") {
		t.Errorf("error %q does not mention the missing-agent sequencing bug", err.Error())
	}
}

// openInMemorySQLite gives the bootstrap tests their own SQLite
// fixture (the *_test.go files in package db can't import internal
// helpers from package sqlite_test).
func openInMemorySQLite(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return conn
}
