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
)

// TestRepoStore_Postgres runs the shared conformance suite against
// the Postgres RepoStore impl. Wires both pools against AdminDB
// (BYPASSRLS) so behavior tests stay independent of the auth path;
// the cross-org leakage test below exercises the org_id filter
// directly.
func TestRepoStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB)

	dbtest.RunRepoStoreConformance(t, func(t *testing.T) (db.RepoStore, string) {
		t.Helper()
		h.Reset(t)
		orgID, _, _ := seedPgRepoOrg(t, h)
		return stores.Repos, orgID
	})
}

// TestRepoStore_Postgres_CrossOrgLeakage pins the defense-in-depth
// org_id filter on every read + mutation path. RLS via
// repo_profiles_all also enforces this, but the org_id = $N clause
// in each query is the belt to RLS's suspenders.
func TestRepoStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB)

	orgA, _, _ := seedPgRepoOrg(t, h)
	orgB, _, _ := seedPgRepoOrg(t, h)
	ctx := context.Background()

	// Seed a repo into orgA only.
	if err := stores.Repos.Upsert(ctx, orgA, domain.RepoProfile{
		ID: "octo/widget", Owner: "octo", Repo: "widget",
		Description: "orgA widget", ProfileText: "orgA body",
		DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("Upsert orgA: %v", err)
	}

	// Get(orgB, octo/widget) must return nil despite the row existing.
	if got, err := stores.Repos.Get(ctx, orgB, "octo/widget"); err != nil {
		t.Fatalf("Get cross-org: %v", err)
	} else if got != nil {
		t.Errorf("orgB Get returned orgA repo %s", got.ID)
	}

	// List cross-org must return empty.
	got, err := stores.Repos.List(ctx, orgB)
	if err != nil {
		t.Fatalf("List cross-org: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("orgB List returned %d rows, want 0", len(got))
	}

	// ListWithContent cross-org must also return empty.
	gotContent, err := stores.Repos.ListWithContent(ctx, orgB)
	if err != nil {
		t.Fatalf("ListWithContent cross-org: %v", err)
	}
	if len(gotContent) != 0 {
		t.Errorf("orgB ListWithContent returned %d rows, want 0", len(gotContent))
	}

	// CountConfigured cross-org must report 0.
	if n, _ := stores.Repos.CountConfigured(ctx, orgB); n != 0 {
		t.Errorf("orgB CountConfigured = %d, want 0", n)
	}

	// UpdateBaseBranch cross-org must not touch orgA's row.
	if err := stores.Repos.UpdateBaseBranch(ctx, orgB, "octo/widget", "hack"); err != nil {
		t.Fatalf("UpdateBaseBranch cross-org: %v", err)
	}
	if got, _ := stores.Repos.Get(ctx, orgA, "octo/widget"); got.BaseBranch != "" {
		t.Errorf("orgA's BaseBranch was mutated by orgB UpdateBaseBranch: got %q", got.BaseBranch)
	}

	// UpdateCloneStatus cross-org must not touch orgA's row.
	if err := stores.Repos.UpdateCloneStatus(ctx, orgB, "octo", "widget", "failed", "hack", "other"); err != nil {
		t.Fatalf("UpdateCloneStatus cross-org: %v", err)
	}
	if got, _ := stores.Repos.Get(ctx, orgA, "octo/widget"); got.CloneStatus == "failed" {
		t.Errorf("orgA's CloneStatus was mutated by orgB UpdateCloneStatus: got %q", got.CloneStatus)
	}

	// SetConfigured cross-org must not delete orgA's row.
	if err := stores.Repos.SetConfigured(ctx, orgB, []string{"another/repo"}); err != nil {
		t.Fatalf("SetConfigured cross-org: %v", err)
	}
	if got, _ := stores.Repos.Get(ctx, orgA, "octo/widget"); got == nil {
		t.Errorf("orgA's repo was deleted by orgB SetConfigured")
	}
}

func seedPgRepoOrg(t *testing.T, h *pgtest.Harness) (orgID, userID, agentID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	agentID = uuid.New().String()
	email := fmt.Sprintf("repo-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Repo Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Repo Org "+orgID[:8], "repo-"+orgID[:8], userID,
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
		`INSERT INTO agents (id, org_id, display_name) VALUES ($1, $2, 'Repo Bot')`,
		agentID, orgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return orgID, userID, agentID
}
