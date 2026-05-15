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
)

// TestEntityStore_Postgres runs the shared conformance suite against
// the Postgres EntityStore impl. Wires both pools against AdminDB
// (BYPASSRLS) so the suite reads behavior in isolation from the
// auth path; the dedicated cross-org leakage + RLS subtests below
// exercise the org_id filter through the app pool.
func TestEntityStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB)

	dbtest.RunEntityStoreConformance(t, func(t *testing.T) (db.EntityStore, string, dbtest.EntitySeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID := seedPgEntityOrg(t, h)
		seed := newPgEntitySeeder(h.AdminDB, orgID, userID)
		return stores.Entities, orgID, seed
	})
}

// TestEntityStore_Postgres_CrossOrgLeakage verifies the org_id
// defense-in-depth filter on every read path. RLS would normally
// catch a wrong-org read too, but the conformance harness runs
// against AdminDB (BYPASSRLS); this test asserts the WHERE clauses
// themselves reject cross-org access without relying on RLS.
func TestEntityStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AdminDB)

	orgA, userA := seedPgEntityOrg(t, h)
	orgB, _ := seedPgEntityOrg(t, h)
	pidA := seedPgEntityProject(t, h, orgA, userA, "A-proj")

	ctx := context.Background()
	ent, _, err := stores.Entities.FindOrCreate(ctx, orgA, "github", "owner/repo#cross", "pr", "T", "")
	if err != nil {
		t.Fatalf("seed entity in orgA: %v", err)
	}
	if err := stores.Entities.AssignProject(ctx, orgA, ent.ID, &pidA, "rationale"); err != nil {
		t.Fatalf("assign in orgA: %v", err)
	}
	if err := stores.Entities.UpdateDescription(ctx, orgA, ent.ID, "describes A"); err != nil {
		t.Fatalf("describe in orgA: %v", err)
	}

	// Get(orgB, ent.ID) must return nil — even though the id exists, it
	// belongs to orgA.
	gotB, err := stores.Entities.Get(ctx, orgB, ent.ID)
	if err != nil {
		t.Fatalf("Get(orgB): %v", err)
	}
	if gotB != nil {
		t.Errorf("cross-org Get leaked entity: %+v", gotB)
	}

	// GetBySource cross-org returns nil.
	gotSrc, err := stores.Entities.GetBySource(ctx, orgB, "github", "owner/repo#cross")
	if err != nil {
		t.Fatalf("GetBySource(orgB): %v", err)
	}
	if gotSrc != nil {
		t.Errorf("cross-org GetBySource leaked entity: %+v", gotSrc)
	}

	// Descriptions cross-org skips the row.
	descs, err := stores.Entities.Descriptions(ctx, orgB, []string{ent.ID})
	if err != nil {
		t.Fatalf("Descriptions(orgB): %v", err)
	}
	if _, ok := descs[ent.ID]; ok {
		t.Errorf("cross-org Descriptions leaked entity %s", ent.ID)
	}

	// ListActive in orgB must not contain the orgA entity.
	activeB, err := stores.Entities.ListActive(ctx, orgB, "github")
	if err != nil {
		t.Fatalf("ListActive(orgB): %v", err)
	}
	for _, e := range activeB {
		if e.ID == ent.ID {
			t.Errorf("cross-org ListActive leaked entity %s", ent.ID)
		}
	}

	// AssignProject cross-org returns sql.ErrNoRows (entity exists in
	// orgA but not orgB; the WHERE filter rejects the UPDATE, then
	// the existence probe also misses on org_id=$orgB).
	if err := stores.Entities.AssignProject(ctx, orgB, ent.ID, &pidA, "r"); err != sql.ErrNoRows {
		t.Errorf("cross-org AssignProject err = %v, want sql.ErrNoRows", err)
	}
}

func seedPgEntityOrg(t *testing.T, h *pgtest.Harness) (orgID, userID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	email := fmt.Sprintf("entity-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Entity Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Entity Org "+orgID[:8], "ent-"+orgID[:8], userID,
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
	return orgID, userID
}

func seedPgEntityProject(t *testing.T, h *pgtest.Harness, orgID, userID, name string) string {
	t.Helper()
	id := uuid.New().String()
	teamID := firstTeamForOrg(t, h, orgID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO projects (id, org_id, creator_user_id, team_id, name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, now(), now())
	`, id, orgID, userID, teamID, name); err != nil {
		t.Fatalf("seed project %s: %v", name, err)
	}
	return id
}

func newPgEntitySeeder(conn *sql.DB, orgID, userID string) dbtest.EntitySeeder {
	return dbtest.EntitySeeder{
		Project: func(t *testing.T, name string) string {
			t.Helper()
			id := uuid.New().String()
			// team_id must satisfy the projects_team_visibility_requires_team
			// CHECK — default visibility is 'team' so we need a real team
			// row. Use the org's default team seeded by seedPgEntityOrg.
			var teamID string
			if err := conn.QueryRow(
				`SELECT id FROM teams WHERE org_id = $1 ORDER BY created_at ASC LIMIT 1`,
				orgID,
			).Scan(&teamID); err != nil {
				t.Fatalf("lookup default team for org %s: %v", orgID, err)
			}
			if _, err := conn.Exec(`
				INSERT INTO projects (id, org_id, creator_user_id, team_id, name, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, now(), now())
			`, id, orgID, userID, teamID, name); err != nil {
				t.Fatalf("seed project %s: %v", name, err)
			}
			return id
		},
	}
}
