package postgres_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestTaskRuleStore_Postgres runs the shared TaskRuleStore conformance
// suite against the Postgres impl. Wired against AdminDB so
// creator_user_id resolution can fall back to org.owner_user_id
// without needing JWT claims plumbed on every subtest (same pattern
// SwipeStore + PromptStore use).
func TestTaskRuleStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunTaskRuleStoreConformance(t, func(t *testing.T) (db.TaskRuleStore, string) {
		t.Helper()
		h.Reset(t)
		orgID := seedPgOrgForTaskRules(t, h)
		stores := pgstore.New(h.AdminDB, h.AdminDB)
		return stores.TaskRules, orgID
	})
}

// seedPgOrgForTaskRules creates the minimum (user, org, membership)
// triplet TaskRule writes need to satisfy creator_user_id FK
// resolution. Returns the org ID — the conformance suite doesn't
// need to know the user ID because every write resolves it via
// the COALESCE-to-org-owner fallback.
func seedPgOrgForTaskRules(t *testing.T, h *pgtest.Harness) string {
	t.Helper()
	orgID := uuid.New().String()
	userID := uuid.New().String()
	email := fmt.Sprintf("taskrule-conf-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "TaskRule Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "TaskRule Conformance Org "+orgID[:8], "taskrule-"+orgID[:8], userID,
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID,
	); err != nil {
		t.Fatalf("seed org_membership: %v", err)
	}
	return orgID
}
