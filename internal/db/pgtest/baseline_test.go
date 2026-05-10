package pgtest

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestBaseline_AppliesCleanly pins the high-level invariants of the
// migration: every expected schema object is present after goose.Up.
func TestBaseline_AppliesCleanly(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	expectedTables := []string{
		"orgs", "teams", "users", "memberships", "sessions", "project_knowledge",
		"org_settings", "team_settings", "user_settings", "jira_project_status_rules",
		"prompts", "projects", "events_catalog", "entities", "entity_links", "events",
		"task_rules", "prompt_triggers", "tasks", "task_events", "runs", "run_artifacts",
		"run_messages", "run_memory", "pending_firings", "run_worktrees", "pending_prs",
		"swipe_events", "poller_state", "repo_profiles", "pending_reviews",
		"pending_review_comments", "preferences", "system_prompt_versions",
		"curator_requests", "curator_messages", "curator_pending_context",
	}
	for _, table := range expectedTables {
		var n int
		err := h.AdminDB.QueryRow(
			`SELECT COUNT(*) FROM pg_tables WHERE schemaname = 'public' AND tablename = $1`, table,
		).Scan(&n)
		if err != nil {
			t.Fatalf("probe table %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("table %s missing after goose.Up", table)
		}
	}

	for _, fn := range []string{"current_user_id", "current_org_id", "user_has_org_access"} {
		var n int
		if err := h.AdminDB.QueryRow(
			`SELECT COUNT(*) FROM pg_proc p JOIN pg_namespace n ON p.pronamespace = n.oid
			 WHERE n.nspname = 'tf' AND p.proname = $1`, fn,
		).Scan(&n); err != nil {
			t.Fatalf("probe function tf.%s: %v", fn, err)
		}
		if n == 0 {
			t.Errorf("tf.%s missing", fn)
		}
	}

	for _, fn := range []string{"vault_put_org_secret", "vault_get_org_secret", "vault_delete_org_secret", "update_project_knowledge"} {
		var n int
		if err := h.AdminDB.QueryRow(
			`SELECT COUNT(*) FROM pg_proc p JOIN pg_namespace n ON p.pronamespace = n.oid
			 WHERE n.nspname = 'public' AND p.proname = $1`, fn,
		).Scan(&n); err != nil {
			t.Fatalf("probe function public.%s: %v", fn, err)
		}
		if n == 0 {
			t.Errorf("public.%s missing", fn)
		}
	}
}

// TestRoles_TfAppShape pins the exact pg_roles attributes for tf_app
// and the authenticator → tf_app grant. Drift here breaks the entire
// RLS posture, so the assertion is per-bit explicit.
func TestRoles_TfAppShape(t *testing.T) {
	h := Shared(t)

	var canLogin, inherit, bypassRLS bool
	err := h.AdminDB.QueryRow(`
		SELECT rolcanlogin, rolinherit, rolbypassrls
		  FROM pg_roles WHERE rolname = 'tf_app'
	`).Scan(&canLogin, &inherit, &bypassRLS)
	if err != nil {
		t.Fatalf("query tf_app: %v", err)
	}
	if canLogin {
		t.Errorf("tf_app.rolcanlogin = true, want false (NOLOGIN)")
	}
	if inherit {
		t.Errorf("tf_app.rolinherit = true, want false (NOINHERIT)")
	}
	if bypassRLS {
		t.Errorf("tf_app.rolbypassrls = true, want false")
	}

	// authenticator must be granted tf_app so SET ROLE works.
	var granted bool
	err = h.AdminDB.QueryRow(`
		SELECT EXISTS (
		  SELECT 1 FROM pg_auth_members am
		    JOIN pg_roles member ON member.oid = am.member
		    JOIN pg_roles role   ON role.oid   = am.roleid
		   WHERE member.rolname = 'authenticator' AND role.rolname = 'tf_app'
		)
	`).Scan(&granted)
	if err != nil {
		t.Fatalf("query grant: %v", err)
	}
	if !granted {
		t.Errorf("authenticator was not granted tf_app — SET LOCAL ROLE tf_app would fail")
	}
}

// TestSeedData asserts events_catalog rowcount equals
// len(domain.AllEventTypes()). Asserting against the slice length (not
// a hardcoded number) makes drift between the Go event registry and
// the SQL seed list surface here, not at runtime.
func TestSeedData(t *testing.T) {
	h := Shared(t)

	var n int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM events_catalog`).Scan(&n); err != nil {
		t.Fatalf("count events_catalog: %v", err)
	}
	want := len(domain.AllEventTypes())
	if n != want {
		t.Errorf("events_catalog rowcount = %d, want %d (len of domain.AllEventTypes)", n, want)
	}

	// Spot-check: a known event ID is present.
	var label string
	if err := h.AdminDB.QueryRow(
		`SELECT label FROM events_catalog WHERE id = 'github:pr:opened'`,
	).Scan(&label); err != nil {
		t.Fatalf("query github:pr:opened: %v", err)
	}
	if label != "PR Opened" {
		t.Errorf("label = %q, want 'PR Opened'", label)
	}
}

// TestRLS_AdminConnectionBypassesRLS pins the harness contract: tests
// run through AdminDB see all rows regardless of RLS policies. If
// this ever fails, it means tf_app or some other role's BYPASSRLS bit
// changed, which would invalidate every RLS test in the suite (false
// passes from a connection that wasn't actually bypassing RLS).
func TestRLS_AdminConnectionBypassesRLS(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, _, _ := seedOrgWithUser(t, h, "alice")
	orgB, _, _ := seedOrgWithUser(t, h, "bob")

	var n int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM orgs WHERE id IN ($1, $2)`, orgA, orgB).Scan(&n); err != nil {
		t.Fatalf("count orgs: %v", err)
	}
	if n != 2 {
		t.Errorf("AdminDB saw %d orgs, want 2 — RLS not actually bypassed", n)
	}
}

// TestRLS_CrossOrgIsolation is the core RLS assertion. Two orgs, two
// users, each user can only see their own org's data — even when they
// hand-craft INSERTs/SELECTs with the OTHER org's UUIDs.
func TestRLS_CrossOrgIsolation(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, userA, _ := seedOrgWithUser(t, h, "alice")
	orgB, userB, _ := seedOrgWithUser(t, h, "bob")

	// Seed an entity + task under each org via AdminDB (RLS-bypassed).
	entityA := seedEntity(t, h, orgA, "github", "octo/repo#1")
	entityB := seedEntity(t, h, orgB, "github", "octo/repo#1")
	taskA := seedTask(t, h, orgA, userA, entityA, "github:pr:opened")
	taskB := seedTask(t, h, orgB, userB, entityB, "github:pr:opened")

	// Alice (in orgA) SELECTs from tasks — must see only her task.
	err := h.WithUser(t, userA, orgA, func(tx *sql.Tx) error {
		var ids []string
		rows, err := tx.Query(`SELECT id FROM tasks`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if len(ids) != 1 || ids[0] != taskA {
			t.Errorf("alice saw tasks %v, want [%s]", ids, taskA)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("alice select: %v", err)
	}

	// Alice tries to INSERT a task with orgB's org_id directly — RLS
	// WITH CHECK rejects it (her current_org_id = orgA, but the row
	// being inserted has org_id = orgB).
	primaryEventB := getEventForEntity(t, h, entityB)
	err = h.WithUser(t, userA, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO tasks (org_id, creator_user_id, entity_id, event_type, primary_event_id)
			VALUES ($1, $2, $3, 'github:pr:opened', $4)
		`, orgB, userA, entityB, primaryEventB)
		return err
	})
	if err == nil {
		t.Errorf("alice INSERT into orgB tasks did not fail — RLS WITH CHECK broken")
	} else if !strings.Contains(err.Error(), "row-level security") {
		t.Errorf("expected RLS violation, got: %v", err)
	}

	// Bob in orgB sees only taskB.
	err = h.WithUser(t, userB, orgB, func(tx *sql.Tx) error {
		var ids []string
		rows, err := tx.Query(`SELECT id FROM tasks`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if len(ids) != 1 || ids[0] != taskB {
			t.Errorf("bob saw tasks %v, want [%s]", ids, taskB)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob select: %v", err)
	}
}

// TestRLS_CuratorChatPerUserIsolation — two users in the same org +
// same project can't see each other's curator chats, but both can read
// the shared project_knowledge.
func TestRLS_CuratorChatPerUserIsolation(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	org, alice, _ := seedOrgWithUser(t, h, "alice")
	bob := seedUser(t, h, "bob")
	// Add bob to the same team.
	teamID := getOrgTeam(t, h, org)
	mustExec(t, h.AdminDB,
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`, bob, teamID)

	projectID := seedProject(t, h, org, alice, "demo")
	aliceReq := seedCuratorRequest(t, h, org, alice, projectID, "alice asking")
	bobReq := seedCuratorRequest(t, h, org, bob, projectID, "bob asking")

	// Shared KB row.
	mustExec(t, h.AdminDB,
		`INSERT INTO project_knowledge (org_id, project_id, key, content) VALUES ($1, $2, 'overview', 'shared notes')`,
		org, projectID)

	// Alice sees her request only.
	err := h.WithUser(t, alice, org, func(tx *sql.Tx) error {
		var ids []string
		rows, err := tx.Query(`SELECT id FROM curator_requests`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if len(ids) != 1 || ids[0] != aliceReq {
			t.Errorf("alice saw curator_requests %v, want [%s]", ids, aliceReq)
		}

		// But she CAN see the shared project_knowledge.
		var kbCount int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM project_knowledge`).Scan(&kbCount); err != nil {
			return err
		}
		if kbCount != 1 {
			t.Errorf("alice saw %d project_knowledge rows, want 1", kbCount)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("alice: %v", err)
	}

	// Bob sees his request only.
	err = h.WithUser(t, bob, org, func(tx *sql.Tx) error {
		var ids []string
		rows, err := tx.Query(`SELECT id FROM curator_requests`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if len(ids) != 1 || ids[0] != bobReq {
			t.Errorf("bob saw curator_requests %v, want [%s]", ids, bobReq)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob: %v", err)
	}
}

// TestVault_PutGetDelete — happy-path round-trip on the wrapper. Cross-
// org access is covered by TestVault_ClaimMismatchDenied, which exercises
// the in-function p_org_id = current_org_id() gate added on top of the
// path-prefix isolation.
func TestVault_PutGetDelete(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, userA, _ := seedOrgWithUser(t, h, "alice")

	err := h.WithUser(t, userA, orgA, func(tx *sql.Tx) error {
		var id string
		if err := tx.QueryRow(
			`SELECT vault_put_org_secret($1, $2, $3, NULL)`, orgA, "github_pat", "ghp_alice_secret",
		).Scan(&id); err != nil {
			return err
		}

		var secret sql.NullString
		if err := tx.QueryRow(
			`SELECT vault_get_org_secret($1, $2)`, orgA, "github_pat",
		).Scan(&secret); err != nil {
			return err
		}
		if !secret.Valid || secret.String != "ghp_alice_secret" {
			t.Errorf("get after put = %v, want ghp_alice_secret", secret)
		}

		var deleted bool
		if err := tx.QueryRow(
			`SELECT vault_delete_org_secret($1, $2)`, orgA, "github_pat",
		).Scan(&deleted); err != nil {
			return err
		}
		if !deleted {
			t.Errorf("delete returned false, want true")
		}

		// Subsequent get returns NULL.
		var afterDelete sql.NullString
		if err := tx.QueryRow(
			`SELECT vault_get_org_secret($1, $2)`, orgA, "github_pat",
		).Scan(&afterDelete); err != nil {
			return err
		}
		if afterDelete.Valid {
			t.Errorf("get after delete returned %q, want NULL", afterDelete.String)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("round-trip: %v", err)
	}
}

// TestVault_ClaimMismatchDenied — even tf_app (with EXECUTE) cannot
// pass an arbitrary p_org_id; the wrapper checks p_org_id against
// the JWT-claims org_id and raises 42501 on mismatch. Without this
// in-function gate, a SECURITY DEFINER wrapper would let any caller
// with EXECUTE retrieve secrets for any org UUID they happened to
// know — defeating the org-prefix isolation.
func TestVault_ClaimMismatchDenied(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, userA, _ := seedOrgWithUser(t, h, "alice")
	orgB, _, _ := seedOrgWithUser(t, h, "bob")

	// Alice's session (claims org_id = orgA) tries to put a secret
	// with p_org_id = orgB. Must error.
	err := h.WithUser(t, userA, orgA, func(tx *sql.Tx) error {
		var id string
		return tx.QueryRow(
			`SELECT vault_put_org_secret($1, $2, $3, NULL)`, orgB, "github_pat", "stolen",
		).Scan(&id)
	})
	if err == nil {
		t.Fatalf("vault_put_org_secret with mismatched org did not error — claim check broken")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("err = %v, want SQLSTATE 42501", err)
	}

	// Same for get + delete.
	err = h.WithUser(t, userA, orgA, func(tx *sql.Tx) error {
		var s sql.NullString
		return tx.QueryRow(`SELECT vault_get_org_secret($1, $2)`, orgB, "k").Scan(&s)
	})
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("get cross-org err = %v, want 42501", err)
	}

	err = h.WithUser(t, userA, orgA, func(tx *sql.Tx) error {
		var b bool
		return tx.QueryRow(`SELECT vault_delete_org_secret($1, $2)`, orgB, "k").Scan(&b)
	})
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("delete cross-org err = %v, want 42501", err)
	}
}

// TestRLS_UsersIsolation — public.users is org-scoped via the
// "shares at least one org with caller" policy. Alice in orgA cannot
// see bob in orgB; both can see themselves; co-workers in the same
// org can see each other (so display_name/avatar resolve for task
// authors etc.).
func TestRLS_UsersIsolation(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, _ := seedOrgWithUser(t, h, "alice")
	_, bob, _ := seedOrgWithUser(t, h, "bob") // separate org
	// Co-worker: charlie joins alice's team.
	charlie := seedUser(t, h, "charlie")
	teamA := getOrgTeam(t, h, orgA)
	mustExec(t, h.AdminDB,
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`, charlie, teamA)

	err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		var ids []string
		rows, err := tx.Query(`SELECT id FROM users ORDER BY display_name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		// Alice should see herself + charlie (same org), but NOT bob.
		seen := make(map[string]bool)
		for _, id := range ids {
			seen[id] = true
		}
		if !seen[alice] {
			t.Errorf("alice can't see her own user row")
		}
		if !seen[charlie] {
			t.Errorf("alice can't see charlie's user row (same org)")
		}
		if seen[bob] {
			t.Errorf("alice CAN see bob's user row across orgs — RLS broken")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("alice query: %v", err)
	}

	// Alice can update her own display_name.
	err = h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE users SET display_name = $1 WHERE id = $2`, "alice-renamed", alice)
		return err
	})
	if err != nil {
		t.Fatalf("alice self-update: %v", err)
	}

	// Alice CANNOT update bob's row.
	err = h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE users SET display_name = $1 WHERE id = $2`, "bob-pwned", bob)
		if err != nil {
			return err
		}
		rows, _ := res.RowsAffected()
		if rows != 0 {
			t.Errorf("alice's UPDATE on bob affected %d rows, want 0 (RLS WITH CHECK should reject)", rows)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("alice cross-org update: %v", err)
	}
}

// TestVault_GrantsLockdown — calling vault_get_org_secret as a role
// that wasn't granted EXECUTE on the wrapper raises permission denial.
// Pins the REVOKE-from-PUBLIC + GRANT-only-to-tf_app contract.
func TestVault_GrantsLockdown(t *testing.T) {
	h := Shared(t)

	// authenticator inherits no privileges (NOINHERIT) and tf_app
	// wasn't granted EXECUTE to anon. Open a transaction on AppDB,
	// SET ROLE to 'anon' (which tf_app is NOT granted to), then call
	// the wrapper — must error with permission denial.
	tx, err := h.AppDB.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`SET LOCAL ROLE anon`); err != nil {
		t.Fatalf("SET LOCAL ROLE anon: %v", err)
	}
	_, err = tx.Exec(`SELECT vault_get_org_secret('00000000-0000-0000-0000-000000000000', 'k')`)
	if err == nil {
		t.Fatalf("vault_get_org_secret as 'anon' did not error — grants lockdown broken")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("err = %v, want permission denied (SQLSTATE 42501)", err)
	}
}

// TestProjectKnowledge_OCC — two transactions both try to update
// project_knowledge with expected_version=1. One succeeds, one raises
// SQLSTATE 40001.
func TestProjectKnowledge_OCC(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	org, user, _ := seedOrgWithUser(t, h, "alice")
	projectID := seedProject(t, h, org, user, "demo")
	var pkID string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO project_knowledge (org_id, project_id, key, content)
		VALUES ($1, $2, 'overview', 'v1') RETURNING id
	`, org, projectID).Scan(&pkID); err != nil {
		t.Fatalf("seed KB: %v", err)
	}

	// First update — should succeed, returns version=2.
	var newVer int
	err := h.WithUser(t, user, org, func(tx *sql.Tx) error {
		return tx.QueryRow(
			`SELECT update_project_knowledge($1, $2, $3, $4, NULL)`,
			pkID, 1, "v2", user,
		).Scan(&newVer)
	})
	if err != nil {
		t.Fatalf("first update: %v", err)
	}
	if newVer != 2 {
		t.Errorf("first new_version = %d, want 2", newVer)
	}

	// Second update with stale expected_version=1 — must raise 40001.
	err = h.WithUser(t, user, org, func(tx *sql.Tx) error {
		return tx.QueryRow(
			`SELECT update_project_knowledge($1, $2, $3, $4, NULL)`,
			pkID, 1, "v3", user,
		).Scan(&newVer)
	})
	if err == nil {
		t.Fatalf("stale-version update did not error — OCC broken")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "40001" {
		t.Errorf("err = %v, want SQLSTATE 40001 serialization_failure", err)
	}
}

// --- fixture helpers ---

func seedOrgWithUser(t *testing.T, h *Harness, displayName string) (orgID, userID, teamID string) {
	t.Helper()
	userID = seedUser(t, h, displayName)
	orgID = seedOrg(t, h, displayName+"-org", userID)
	teamID = seedTeam(t, h, orgID, "default")
	mustExec(t, h.AdminDB,
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'owner')`, userID, teamID)
	return
}

func seedUser(t *testing.T, h *Harness, displayName string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(
		`SELECT gen_random_uuid()`,
	).Scan(&id); err != nil {
		t.Fatalf("gen uuid: %v", err)
	}
	h.SeedAuthUser(t, id, displayName+"@test")
	mustExec(t, h.AdminDB,
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`, id, displayName)
	return id
}

func seedOrg(t *testing.T, h *Harness, slug, ownerID string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO orgs (slug, name, owner_user_id) VALUES ($1, $1, $2) RETURNING id
	`, slug, ownerID).Scan(&id); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	return id
}

func seedTeam(t *testing.T, h *Harness, orgID, slug string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO teams (org_id, slug, name) VALUES ($1, $2, $2) RETURNING id
	`, orgID, slug).Scan(&id); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	return id
}

func seedProject(t *testing.T, h *Harness, orgID, creatorID, name string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO projects (org_id, creator_user_id, name) VALUES ($1, $2, $3) RETURNING id
	`, orgID, creatorID, name).Scan(&id); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return id
}

func seedEntity(t *testing.T, h *Harness, orgID, source, sourceID string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO entities (org_id, source, source_id, kind, title)
		VALUES ($1, $2, $3, 'pr', 'test pr') RETURNING id
	`, orgID, source, sourceID).Scan(&id); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	return id
}

func seedTask(t *testing.T, h *Harness, orgID, creatorID, entityID, eventType string) string {
	t.Helper()
	// Insert a corresponding event first since tasks.primary_event_id is NOT NULL.
	var evtID string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO events (org_id, entity_id, event_type) VALUES ($1, $2, $3) RETURNING id
	`, orgID, entityID, eventType).Scan(&evtID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO tasks (org_id, creator_user_id, entity_id, event_type, primary_event_id)
		VALUES ($1, $2, $3, $4, $5) RETURNING id
	`, orgID, creatorID, entityID, eventType, evtID).Scan(&id); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return id
}

func seedCuratorRequest(t *testing.T, h *Harness, orgID, creatorID, projectID, input string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO curator_requests (org_id, creator_user_id, project_id, user_input)
		VALUES ($1, $2, $3, $4) RETURNING id
	`, orgID, creatorID, projectID, input).Scan(&id); err != nil {
		t.Fatalf("seed curator_request: %v", err)
	}
	return id
}

func getOrgTeam(t *testing.T, h *Harness, orgID string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(
		`SELECT id FROM teams WHERE org_id = $1 LIMIT 1`, orgID,
	).Scan(&id); err != nil {
		t.Fatalf("get team for org %s: %v", orgID, err)
	}
	return id
}

func getEventForEntity(t *testing.T, h *Harness, entityID string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(
		`SELECT id FROM events WHERE entity_id = $1 LIMIT 1`, entityID,
	).Scan(&id); err != nil {
		t.Fatalf("get event for entity %s: %v", entityID, err)
	}
	return id
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
