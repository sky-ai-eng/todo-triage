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

// TestTriggerStore_Postgres runs the shared TriggerStore conformance
// suite against the Postgres impl. Wired against AdminDB so writes
// satisfy the prompt_triggers RLS without needing JWT claims plumbed
// on every subtest (same pattern Swipe / Prompt / TaskRule conformance
// tests use).
func TestTriggerStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunTriggerStoreConformance(t, func(t *testing.T) (db.TriggerStore, string, dbtest.PromptSeederForTriggers) {
		t.Helper()
		h.Reset(t)
		orgID := seedPgOrgForTriggers(t, h)
		stores := pgstore.New(h.AdminDB, h.AdminDB)
		seedPrompts := func(t *testing.T) {
			t.Helper()
			seedPgPromptsForTriggers(t, stores.Prompts, orgID)
		}
		return stores.Triggers, orgID, seedPrompts
	})
}

// TestTriggerStore_Postgres_SeededRowsHaveSystemShape pins the
// schema-honest shape established by migration 202605120001:
// shipped triggers have NULL creator_user_id, source='system',
// visibility='org', enabled=false. The
// prompt_triggers_system_has_no_creator CHECK enforces (source='system'
// ↔ creator IS NULL); this test makes sure the seeder writes rows in
// that shape rather than relying on the constraint to catch a
// regression after the fact.
func TestTriggerStore_Postgres_SeededRowsHaveSystemShape(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID := seedPgOrgForTriggers(t, h)

	stores := pgstore.New(h.AdminDB, h.AdminDB)
	seedPgPromptsForTriggers(t, stores.Prompts, orgID)
	if err := stores.Triggers.Seed(context.Background(), orgID); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rows, err := h.AdminDB.Query(`
		SELECT source, visibility, enabled, creator_user_id IS NULL
		FROM prompt_triggers
		WHERE org_id = $1
	`, orgID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	var count int
	for rows.Next() {
		var source, visibility string
		var enabled, creatorIsNull bool
		if err := rows.Scan(&source, &visibility, &enabled, &creatorIsNull); err != nil {
			t.Fatalf("scan: %v", err)
		}
		count++
		if source != "system" {
			t.Errorf("source=%q want system", source)
		}
		if visibility != "org" {
			t.Errorf("visibility=%q want org", visibility)
		}
		if enabled {
			t.Errorf("shipped trigger enabled=true; convention is ship disabled")
		}
		if !creatorIsNull {
			t.Errorf("creator_user_id NOT NULL on system row; CHECK constraint should have refused")
		}
	}
	if count != len(db.ShippedPromptTriggers) {
		t.Errorf("got %d seeded rows, want %d", count, len(db.ShippedPromptTriggers))
	}
}

// TestTriggerStore_Postgres_SeedIsolatesAcrossOrgs pins the per-org
// UUIDv5 derivation: seeding two orgs must not collide on the global
// PK. Same regression class as TaskRuleStore + the same fix.
func TestTriggerStore_Postgres_SeedIsolatesAcrossOrgs(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA := seedPgOrgForTriggers(t, h)
	orgB := seedPgOrgForTriggers(t, h)
	if orgA == orgB {
		t.Fatal("seedPgOrgForTriggers returned same orgID twice")
	}

	stores := pgstore.New(h.AdminDB, h.AdminDB)
	seedPgPromptsForTriggers(t, stores.Prompts, orgA)
	seedPgPromptsForTriggers(t, stores.Prompts, orgB)
	ctx := context.Background()

	if err := stores.Triggers.Seed(ctx, orgA); err != nil {
		t.Fatalf("seed org A: %v", err)
	}
	if err := stores.Triggers.Seed(ctx, orgB); err != nil {
		t.Fatalf("seed org B (regression case): %v", err)
	}

	a, _ := stores.Triggers.List(ctx, orgA)
	b, _ := stores.Triggers.List(ctx, orgB)
	want := len(db.ShippedPromptTriggers)
	if len(a) != want {
		t.Errorf("org A got %d triggers, want %d", len(a), want)
	}
	if len(b) != want {
		t.Errorf("org B got %d triggers, want %d (regression: slug-only UUID collided)", len(b), want)
	}
	aIDs := map[string]struct{}{}
	for _, row := range a {
		aIDs[row.ID] = struct{}{}
	}
	for _, row := range b {
		if _, overlap := aIDs[row.ID]; overlap {
			t.Errorf("row id %s appears in both orgs; per-org UUID broken", row.ID)
		}
	}
}

// TestTriggerStore_Postgres_SeedRejectsInsideTx pins the contract.
func TestTriggerStore_Postgres_SeedRejectsInsideTx(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID := seedPgOrgForTriggers(t, h)
	ownerID := mustOwnerUserForOrg(t, h, orgID)

	stores := pgstore.New(h.AdminDB, h.AdminDB)
	err := stores.Tx.WithTx(context.Background(), orgID, ownerID, func(tx db.TxStores) error {
		return tx.Triggers.Seed(context.Background(), orgID)
	})
	if err == nil {
		t.Fatal("Seed inside WithTx returned nil; want explicit refusal")
	}
}

func seedPgOrgForTriggers(t *testing.T, h *pgtest.Harness) string {
	t.Helper()
	orgID := uuid.New().String()
	userID := uuid.New().String()
	email := fmt.Sprintf("trigger-conf-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Trigger Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Trigger Conformance Org "+orgID[:8], "trigger-"+orgID[:8], userID,
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

// seedPgPromptsForTriggers ensures the prompts referenced by every
// shipped trigger exist in the given org. Prompt rows are required
// because prompt_triggers (prompt_id, org_id) FKs them.
func seedPgPromptsForTriggers(t *testing.T, prompts db.PromptStore, orgID string) {
	t.Helper()
	ctx := context.Background()
	for _, p := range []domain.Prompt{
		{ID: "system-ci-fix", Name: "CI Fix", Body: "x", Source: "system"},
		{ID: "system-conflict-resolution", Name: "Conflict", Body: "x", Source: "system"},
		{ID: "system-jira-implement", Name: "Jira", Body: "x", Source: "system"},
		{ID: "system-pr-review", Name: "PR Review", Body: "x", Source: "system"},
		{ID: "system-fix-review-feedback", Name: "Fix Review", Body: "x", Source: "system"},
	} {
		if err := prompts.SeedOrUpdate(ctx, orgID, p); err != nil {
			t.Fatalf("seed prompt %s: %v", p.ID, err)
		}
	}
}
