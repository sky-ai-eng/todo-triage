package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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

// TestTaskRuleStore_Postgres_SeedIsolatesAcrossOrgs is the load-bearing
// invariant for the multi-tenant deployment: seeding org A must not
// prevent org B from getting its own copy of the shipped rules. The
// regression this guards against — slug-only UUIDv5 deriving a single
// row id for every org — would silently drop the seed for every org
// after the first because task_rules.id is a global PRIMARY KEY and
// ON CONFLICT DO NOTHING would skip the duplicate.
//
// The fix it pins: ShippedTaskRule.UUIDFor(orgID) mixes orgID into
// the UUID derivation so each tenant gets a distinct row id for the
// same logical rule.
func TestTaskRuleStore_Postgres_SeedIsolatesAcrossOrgs(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA := seedPgOrgForTaskRules(t, h)
	orgB := seedPgOrgForTaskRules(t, h)
	if orgA == orgB {
		t.Fatal("seedPgOrgForTaskRules returned the same orgID twice")
	}

	stores := pgstore.New(h.AdminDB, h.AdminDB)
	ctx := context.Background()

	if err := stores.TaskRules.Seed(ctx, orgA); err != nil {
		t.Fatalf("seed org A: %v", err)
	}
	if err := stores.TaskRules.Seed(ctx, orgB); err != nil {
		t.Fatalf("seed org B (regression case — slug-only UUID collides on PK and skips): %v", err)
	}

	a, err := stores.TaskRules.List(ctx, orgA)
	if err != nil {
		t.Fatalf("list org A: %v", err)
	}
	b, err := stores.TaskRules.List(ctx, orgB)
	if err != nil {
		t.Fatalf("list org B: %v", err)
	}
	wantCount := len(db.ShippedTaskRules)
	if len(a) != wantCount {
		t.Errorf("org A got %d shipped rules, want %d", len(a), wantCount)
	}
	if len(b) != wantCount {
		t.Errorf("org B got %d shipped rules, want %d (regression: slug-only UUID collided across orgs)", len(b), wantCount)
	}

	// Cross-tenant isolation: each org's rows must carry their own
	// row id. The same logical shipped rule (event_type +
	// system source) appears in both lists but with distinct ids.
	aIDs := map[string]struct{}{}
	for _, r := range a {
		aIDs[r.ID] = struct{}{}
	}
	for _, r := range b {
		if _, overlap := aIDs[r.ID]; overlap {
			t.Errorf("row id %s appears in both org A and org B; per-org UUID derivation broken", r.ID)
		}
	}
}

// TestTaskRuleStore_Postgres_SeedRejectsInsideTx pins the contract
// that Seed must not be invoked from a WithTx closure. Escaping to
// the admin pool from inside a caller's tx would silently bypass
// their transaction scope (the inserts would commit even if the
// caller's tx rolled back). The error is the explicit refusal.
func TestTaskRuleStore_Postgres_SeedRejectsInsideTx(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID := seedPgOrgForTaskRules(t, h)

	stores := pgstore.New(h.AdminDB, h.AdminDB)
	err := stores.Tx.WithTx(context.Background(), orgID, mustOwnerUserForOrg(t, h, orgID), func(tx db.TxStores) error {
		return tx.TaskRules.Seed(context.Background(), orgID)
	})
	if err == nil {
		t.Fatal("Seed inside WithTx returned nil; want explicit refusal")
	}
	if !strings.Contains(err.Error(), "must not be called inside WithTx") {
		t.Errorf("error %q does not mention the inside-WithTx contract; tighten the message or this test", err.Error())
	}
}

// TestTaskRuleStore_Postgres_SeededRowsHaveSystemShape pins the
// schema-honest shape established by migration 202605110001:
// shipped rules have NULL creator_user_id, source='system',
// visibility='org'. The task_rules_system_has_no_creator CHECK
// constraint enforces (source='system' ↔ creator_user_id IS NULL);
// this test makes sure the seeder actually writes rows in that
// shape rather than relying on the constraint to catch a regression
// after the fact.
func TestTaskRuleStore_Postgres_SeededRowsHaveSystemShape(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID := seedPgOrgForTaskRules(t, h)

	stores := pgstore.New(h.AdminDB, h.AdminDB)
	if err := stores.TaskRules.Seed(context.Background(), orgID); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Read raw to inspect creator_user_id + visibility — the
	// TaskRuleStore.List projection drops creator_user_id (it's not
	// on domain.TaskRule), so we hit the table directly.
	rows, err := h.AdminDB.Query(`
		SELECT source, visibility, creator_user_id IS NULL
		FROM task_rules
		WHERE org_id = $1
	`, orgID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	var count int
	for rows.Next() {
		var source, visibility string
		var creatorIsNull bool
		if err := rows.Scan(&source, &visibility, &creatorIsNull); err != nil {
			t.Fatalf("scan: %v", err)
		}
		count++
		if source != "system" {
			t.Errorf("source=%q want system", source)
		}
		if visibility != "org" {
			t.Errorf("visibility=%q want org (so any org member can read/disable)", visibility)
		}
		if !creatorIsNull {
			t.Errorf("creator_user_id NOT NULL on system row; CHECK constraint should have refused or seeder is lying")
		}
	}
	if count != len(db.ShippedTaskRules) {
		t.Errorf("got %d seeded rows, want %d", count, len(db.ShippedTaskRules))
	}
}

// TestTaskRuleStore_Postgres_AdminCanUpdateSystemRow pins the
// admin-only gate on org-visible row writes from migration
// 202605110001. Disabling a TF-shipped default is an org-wide
// decision: an admin makes it and every org member observes it.
// The policy refuses non-admin writers and accepts admins.
func TestTaskRuleStore_Postgres_AdminCanUpdateSystemRow(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	// Owner (alice from seedPgOrgForTaskRules), an explicit admin
	// (carol), and a plain member (bob). Three distinct identities
	// so we can prove the policy gates on role, not on identity-as-
	// owner-of-org.
	orgID := seedPgOrgForTaskRules(t, h)
	bobID := seedPgMember(t, h, orgID, "bob", "member")
	carolID := seedPgMember(t, h, orgID, "carol", "admin")

	adminStores := pgstore.New(h.AdminDB, h.AdminDB)
	if err := adminStores.TaskRules.Seed(context.Background(), orgID); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var ruleID string
	if err := h.AdminDB.QueryRow(`
		SELECT id FROM task_rules
		WHERE org_id = $1 AND source = 'system' AND event_type = $2
	`, orgID, domain.EventGitHubPRCICheckFailed).Scan(&ruleID); err != nil {
		t.Fatalf("find shipped CI rule: %v", err)
	}

	// Non-admin (bob) must be refused. The policy filters the row
	// out of the UPDATE's USING set, so the UPDATE returns 0 rows
	// rather than an error — assert rows-affected.
	err := h.WithUser(t, bobID, orgID, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE task_rules SET enabled = FALSE, updated_at = now()
			  WHERE org_id = $1 AND id = $2`,
			orgID, ruleID,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("non-admin UPDATE matched %d rows; policy should filter org-visible rows away from non-admins", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("non-admin UPDATE assertion: %v", err)
	}
	// Sanity: rule still enabled after bob's failed UPDATE.
	var enabled bool
	if err := h.AdminDB.QueryRow(
		`SELECT enabled FROM task_rules WHERE id = $1`, ruleID,
	).Scan(&enabled); err != nil {
		t.Fatalf("re-read post-bob: %v", err)
	}
	if !enabled {
		t.Fatal("rule disabled after non-admin UPDATE; policy didn't gate")
	}

	// Admin (carol) must succeed.
	err = h.WithUser(t, carolID, orgID, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE task_rules SET enabled = FALSE, updated_at = now()
			  WHERE org_id = $1 AND id = $2`,
			orgID, ruleID,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("admin UPDATE matched %d rows; want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("admin UPDATE on system row: %v", err)
	}
	if err := h.AdminDB.QueryRow(
		`SELECT enabled FROM task_rules WHERE id = $1`, ruleID,
	).Scan(&enabled); err != nil {
		t.Fatalf("re-read post-carol: %v", err)
	}
	if enabled {
		t.Errorf("rule still enabled after admin's UPDATE — silent no-op?")
	}
}

// seedPgMember adds one user with the given role to the given org.
// Returns the user id. Used by the policy tests that need to
// distinguish admin from member identities.
func seedPgMember(t *testing.T, h *pgtest.Harness, orgID, label, role string) string {
	t.Helper()
	userID := uuid.New().String()
	h.SeedAuthUser(t, userID, fmt.Sprintf("%s-%s@test.local", label, userID[:8]))
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, label,
	); err != nil {
		t.Fatalf("seed user %s: %v", label, err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, $3)`,
		orgID, userID, role,
	); err != nil {
		t.Fatalf("seed %s membership: %v", label, err)
	}
	return userID
}

// TestTaskRuleStore_Postgres_UserCannotForgeSystemRow pins the other
// side of the policy split: a tf_app caller cannot smuggle a system
// row through the insert path. The task_rules_insert WITH CHECK
// requires creator_user_id = tf.current_user_id(); a NULL candidate
// fails the equality, so the row is refused regardless of source.
// Additionally the task_rules_system_has_no_creator CHECK constraint
// rejects (source='user' AND creator IS NULL) as well — defense in
// depth.
func TestTaskRuleStore_Postgres_UserCannotForgeSystemRow(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID := seedPgOrgForTaskRules(t, h)
	ownerID := mustOwnerUserForOrg(t, h, orgID)

	err := h.WithUser(t, ownerID, orgID, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO task_rules
				(id, org_id, creator_user_id, event_type, scope_predicate_json,
				 enabled, name, default_priority, sort_order, source, visibility,
				 created_at, updated_at)
			VALUES (gen_random_uuid(), $1, NULL, $2, NULL,
			        TRUE, 'forged', 0.5, 0, 'system', 'org',
			        now(), now())
		`, orgID, domain.EventGitHubPRCICheckFailed)
		return err
	})
	if err == nil {
		t.Fatal("expected RLS / CHECK refusal on tf_app system-row insert; got nil — any caller can forge system rows")
	}
}

// TestTaskRuleStore_Postgres_SeedRunsWithoutClaims pins the load-
// bearing invariant for #4: Seed at boot time has no JWT claims and
// must still succeed. The fix routes Seed to the admin pool
// (BYPASSRLS); this test stages a fresh org and seeds without ever
// calling WithTx — the equivalent of the main.go boot path.
func TestTaskRuleStore_Postgres_SeedRunsWithoutClaims(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID := seedPgOrgForTaskRules(t, h)

	stores := pgstore.New(h.AdminDB, h.AdminDB)
	if err := stores.TaskRules.Seed(context.Background(), orgID); err != nil {
		t.Fatalf("Seed without JWT claims (boot-path equivalent): %v", err)
	}
	rules, err := stores.TaskRules.List(context.Background(), orgID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rules) != len(db.ShippedTaskRules) {
		t.Errorf("got %d rules, want %d after claims-less seed", len(rules), len(db.ShippedTaskRules))
	}
	for _, r := range rules {
		if r.Source != domain.TaskRuleSourceSystem {
			t.Errorf("rule %s source=%q want system", r.ID, r.Source)
		}
	}
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

// mustOwnerUserForOrg returns the owner_user_id for the given org so
// WithTx can seat real JWT claims. Used by SeedRejectsInsideTx to
// build a syntactically valid WithTx invocation — the claims
// themselves are irrelevant once Seed refuses on inTx.
func mustOwnerUserForOrg(t *testing.T, h *pgtest.Harness, orgID string) string {
	t.Helper()
	var userID string
	if err := h.AdminDB.QueryRow(
		`SELECT owner_user_id FROM orgs WHERE id = $1`, orgID,
	).Scan(&userID); err != nil {
		t.Fatalf("read owner_user_id for org %s: %v", orgID, err)
	}
	return userID
}
