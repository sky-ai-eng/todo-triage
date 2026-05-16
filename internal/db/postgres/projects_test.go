package postgres_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestProjectStore_Postgres runs the shared conformance suite against
// the Postgres ProjectStore impl. Both pools wire against AdminDB
// (BYPASSRLS) so behavior tests stay independent of the auth path.
// creator_user_id under admin pool resolves via the org-owner
// fallback half of the COALESCE (no JWT claims set → tf.current_user_id()
// is NULL); production multi-mode under WithTx hits the first branch.
func TestProjectStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB)
	dbtest.RunProjectStoreConformance(t, func(t *testing.T) (db.ProjectStore, string, string) {
		t.Helper()
		h.Reset(t)
		orgID, _, _ := seedPgProjectOrg(t, h)
		teamID := firstTeamForOrg(t, h, orgID)
		return stores.Projects, orgID, teamID
	})
}

// TestProjectStore_Postgres_CrossOrgLeakage pins the defense-in-depth
// org_id filter on every read + mutation path.
func TestProjectStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB)
	ctx := context.Background()

	orgA, _, _ := seedPgProjectOrg(t, h)
	teamA := firstTeamForOrg(t, h, orgA)
	orgB, _, _ := seedPgProjectOrg(t, h)

	id, err := stores.Projects.Create(ctx, orgA, teamA, domain.Project{
		Name: "orgA project", Description: "secret",
	})
	if err != nil {
		t.Fatalf("Create orgA: %v", err)
	}

	if got, err := stores.Projects.Get(ctx, orgB, id); err != nil {
		t.Fatalf("Get cross-org: %v", err)
	} else if got != nil {
		t.Errorf("orgB Get returned orgA project %s", id)
	}

	if got, err := stores.Projects.List(ctx, orgB); err != nil {
		t.Fatalf("List cross-org: %v", err)
	} else if len(got) != 0 {
		t.Errorf("orgB List returned %d rows, want 0", len(got))
	}

	if err := stores.Projects.Update(ctx, orgB, domain.Project{ID: id, Name: "hack"}); err == nil {
		t.Errorf("orgB Update on orgA project should error")
	}
	if err := stores.Projects.Delete(ctx, orgB, id); err == nil {
		t.Errorf("orgB Delete on orgA project should error")
	}
	if got, _ := stores.Projects.Get(ctx, orgA, id); got == nil || got.Name != "orgA project" {
		t.Errorf("orgA's row was clobbered by cross-org mutation: got=%+v", got)
	}
}

// TestProjectStore_Postgres_CreateRefusesTeamSentinel pins the team
// guard: passing runmode.LocalDefaultTeamID (the SQLite-only sentinel)
// returns a clear error instead of silently attaching the project to
// any team. Projects are user-driven writes; the human picks the
// team at the Create UI (SKY-294), and the store refuses to make one
// up. Once D9 retrofits handler claims, the caller threads a real
// team from request context.
func TestProjectStore_Postgres_CreateRefusesTeamSentinel(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB)
	ctx := context.Background()

	orgID, _, _ := seedPgProjectOrg(t, h)

	_, err := stores.Projects.Create(ctx, orgID,
		runmode.LocalDefaultTeamID,
		domain.Project{Name: "no-team"})
	if err == nil {
		t.Fatal("Create with team sentinel should error; want explicit team_id requirement")
	}
}

func seedPgProjectOrg(t *testing.T, h *pgtest.Harness) (orgID, userID, agentID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	agentID = uuid.New().String()
	email := fmt.Sprintf("project-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Project Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Project Org "+orgID[:8], "proj-"+orgID[:8], userID,
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID,
	); err != nil {
		t.Fatalf("seed org_membership: %v", err)
	}
	seedPgDefaultTeam(t, h, orgID, userID)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO agents (id, org_id, display_name) VALUES ($1, $2, 'Project Bot')`,
		agentID, orgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return orgID, userID, agentID
}
