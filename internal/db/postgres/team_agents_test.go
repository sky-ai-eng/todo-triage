package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestTeamAgentStore_Postgres runs the shared conformance suite. The
// factory pre-seeds (user, org, team, agent) so all FKs satisfy.
func TestTeamAgentStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunTeamAgentStoreConformance(t, func(t *testing.T) (db.TeamAgentStore, string, string, string) {
		t.Helper()
		h.Reset(t)
		orgID := seedPgOrgForAgents(t, h)
		teamID := seedPgTeam(t, h, orgID, "default")
		stores := pgstore.New(h.AdminDB, h.AdminDB)
		agentID, err := stores.Agents.Create(context.Background(), orgID, domain.Agent{DisplayName: "Bot"})
		if err != nil {
			t.Fatalf("seed agent: %v", err)
		}
		return stores.TeamAgents, orgID, teamID, agentID
	})
}

// TestTeamAgentStore_Postgres_NonMemberCannotToggle pins the RLS
// gate: a team member can toggle their own team's bot, but cannot
// toggle a different team's bot in the same org.
func TestTeamAgentStore_Postgres_NonMemberCannotToggle(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID := seedPgOrgForAgents(t, h)
	platformID := seedPgTeam(t, h, orgID, "platform")
	mobileID := seedPgTeam(t, h, orgID, "mobile")

	// dave is a member of the org (so org-access predicate passes for
	// joined queries) but only joins platform; toggling mobile must
	// fail the team_agents_modify RLS predicate.
	daveID := seedPgMember(t, h, orgID, "dave", "member")
	if _, err := h.AdminDB.Exec(
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`,
		daveID, platformID,
	); err != nil {
		t.Fatalf("dave platform membership: %v", err)
	}

	adminStores := pgstore.New(h.AdminDB, h.AdminDB)
	agentID, err := adminStores.Agents.Create(context.Background(), orgID, domain.Agent{DisplayName: "Org Bot"})
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	// Pre-seed team_agents rows for both teams.
	for _, teamID := range []string{platformID, mobileID} {
		if err := adminStores.TeamAgents.AddForTeam(context.Background(), orgID, teamID, agentID); err != nil {
			t.Fatalf("AddForTeam %s: %v", teamID, err)
		}
	}

	// dave toggles platform — allowed (he's a member).
	err = h.WithUser(t, daveID, orgID, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE team_agents SET enabled = FALSE WHERE team_id = $1 AND agent_id = $2`,
			platformID, agentID,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return fmt.Errorf("platform toggle matched %d rows; want 1 (dave is a platform member)", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("dave toggle own team: %v", err)
	}

	// dave toggles mobile — refused (not a member). RLS filters the
	// row out of the UPDATE's USING set; 0 rows affected, no error.
	err = h.WithUser(t, daveID, orgID, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE team_agents SET enabled = FALSE WHERE team_id = $1 AND agent_id = $2`,
			mobileID, agentID,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			return fmt.Errorf("non-member toggle matched %d rows; want 0", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("dave toggle other team: %v", err)
	}

	// Sanity: mobile's team_agents row is still enabled.
	var enabled bool
	if err := h.AdminDB.QueryRow(
		`SELECT enabled FROM team_agents WHERE team_id = $1`, mobileID,
	).Scan(&enabled); err != nil {
		t.Fatalf("re-read mobile: %v", err)
	}
	if !enabled {
		t.Error("mobile bot disabled by non-member; RLS didn't gate")
	}
}

// seedPgTeam inserts a teams row under the given org and returns its id.
func seedPgTeam(t *testing.T, h *pgtest.Harness, orgID, slug string) string {
	t.Helper()
	teamID := uuid.New().String()
	if _, err := h.AdminDB.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES ($1, $2, $3, $4)`,
		teamID, orgID, slug, slug+" team",
	); err != nil {
		t.Fatalf("seed team %s: %v", slug, err)
	}
	return teamID
}
