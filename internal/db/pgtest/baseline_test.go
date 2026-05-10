package pgtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
		"orgs", "teams", "users", "memberships", "org_memberships", "sessions", "project_knowledge",
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

	for _, fn := range []string{
		"current_user_id", "current_org_id",
		"user_has_org_access", "user_is_org_admin", "user_is_team_admin",
		"user_owns_org", "user_is_org_admin_via_team",
	} {
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
	// Add bob to the same team (org member + team member).
	teamID := getOrgTeam(t, h, org)
	addOrgMember(t, h, bob, org, teamID, "member", "member")

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
	addOrgMember(t, h, charlie, orgA, teamA, "member", "member")

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

// TestRLS_UsersVisibleWithoutTeamMembership catches a regression
// where users_select joined through memberships+teams instead of
// org_memberships. A founder who has just created their org has an
// org_memberships row but may not yet have any memberships row;
// the old policy made them invisible to org-mates added afterward.
func TestRLS_UsersVisibleWithoutTeamMembership(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	// Founder: org owner with org_memberships row, NO memberships row.
	founder := seedUser(t, h, "founder")
	orgID := seedOrg(t, h, "founder-org", founder)
	teamID := seedTeam(t, h, orgID, "default")
	mustExec(t, h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'owner')`, founder, orgID)
	// (deliberately no memberships row for founder)

	// Joiner: org member via org_memberships AND on the team.
	joiner := seedUser(t, h, "joiner")
	addOrgMember(t, h, joiner, orgID, teamID, "member", "member")

	// Joiner must be able to see the founder via the users table
	// even though founder isn't on any team.
	err := h.WithUser(t, joiner, orgID, func(tx *sql.Tx) error {
		var name string
		if err := tx.QueryRow(`SELECT display_name FROM users WHERE id = $1`, founder).Scan(&name); err != nil {
			return err
		}
		if name != "founder" {
			t.Errorf("got display_name = %q, want 'founder'", name)
		}
		return nil
	})
	if err != nil {
		t.Errorf("joiner can't see team-less founder: %v — users_select must use org_memberships, not memberships+teams", err)
	}
}

// TestRLS_TeamSettingsIsTeamMemberOnly catches the team_settings
// regression where SELECT was open to all org members. A member of
// team A should not see team B's settings even within the same org.
func TestRLS_TeamSettingsIsTeamMemberOnly(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := seedOrgWithUser(t, h, "alice")
	// teamB is a second team in the same org; bob is on teamB only.
	teamB := seedTeam(t, h, orgA, "team-b")
	bob := seedUser(t, h, "bob")
	addOrgMember(t, h, bob, orgA, teamB, "member", "admin")

	// Alice writes team_settings for HER team (teamA).
	if _, err := h.AdminDB.Exec(`INSERT INTO team_settings (team_id) VALUES ($1)`, teamA); err != nil {
		t.Fatalf("seed team_settings: %v", err)
	}

	// Bob (member of teamB, same org as teamA) must NOT see teamA's settings.
	err := h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM team_settings WHERE team_id = $1`, teamA).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("bob (teamB) saw %d team_settings rows for teamA, want 0 — gate must require team membership, not just org membership", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob query: %v", err)
	}

	// Sanity: alice (on teamA) CAN see her own team's settings.
	err = h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM team_settings WHERE team_id = $1`, teamA).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("alice saw %d rows for her own team's settings, want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("alice sanity: %v", err)
	}
}

// TestVault_GrantsLockdown — pins both the catalog state (anon /
// authenticated / service_role lack EXECUTE on the wrappers) AND the
// behavior (calling as one of those roles raises 42501).
//
// Why both: the wrappers internally raise 42501 on cross-org claim
// mismatch (TestVault_ClaimMismatchDenied). If anon retained EXECUTE
// via a Supabase auto-grant we missed, behavior alone (42501 raised)
// wouldn't distinguish "permission denied at function entry" from
// "claim check inside the function rejected." The has_function_privilege
// assertion locks down the actual ACL state regardless of behavior.
func TestVault_GrantsLockdown(t *testing.T) {
	h := Shared(t)

	// 1. Catalog-level pin: every restricted role lacks EXECUTE on
	//    every wrapper. has_function_privilege checks the actual ACL.
	//    (PUBLIC isn't a real role, so it's not a valid arg to
	//    has_function_privilege; we cover the PUBLIC revoke by
	//    asserting proacl has no empty-grantee entry below.)
	wrappers := []string{
		"vault_put_org_secret(uuid,text,text,text)",
		"vault_get_org_secret(uuid,text)",
		"vault_delete_org_secret(uuid,text)",
	}
	deniedRoles := []string{"anon", "authenticated", "service_role"}
	for _, fn := range wrappers {
		for _, role := range deniedRoles {
			var has bool
			if err := h.AdminDB.QueryRow(
				`SELECT has_function_privilege($1, $2, 'EXECUTE')`, role, fn,
			).Scan(&has); err != nil {
				t.Fatalf("has_function_privilege(%s, %s): %v", role, fn, err)
			}
			if has {
				t.Errorf("%s has EXECUTE on %s — lockdown broken", role, fn)
			}
		}
		// Assert no PUBLIC grant. In pg_proc.proacl, PUBLIC grants
		// render as entries with an empty grantee, e.g. "=X/owner".
		// If any such entry exists, PUBLIC has EXECUTE.
		var publicGranted bool
		if err := h.AdminDB.QueryRow(`
			SELECT EXISTS (
			  SELECT 1 FROM (
			    SELECT unnest(proacl)::text AS aclitem
			      FROM pg_proc
			     WHERE oid = ('public.' || $1)::regprocedure
			  ) a
			  WHERE a.aclitem LIKE '=%'
			)
		`, fn).Scan(&publicGranted); err != nil {
			t.Fatalf("probe PUBLIC grant on %s: %v", fn, err)
		}
		if publicGranted {
			t.Errorf("PUBLIC has a grant entry on %s — lockdown broken", fn)
		}
		// Sanity: tf_app DOES have EXECUTE (the only role that should).
		var tfHas bool
		if err := h.AdminDB.QueryRow(
			`SELECT has_function_privilege('tf_app', $1, 'EXECUTE')`, fn,
		).Scan(&tfHas); err != nil {
			t.Fatalf("has_function_privilege(tf_app, %s): %v", fn, err)
		}
		if !tfHas {
			t.Errorf("tf_app LACKS EXECUTE on %s — break the call path", fn)
		}
	}

	// 2. Behavior-level pin: calling as a denied role produces a
	//    PostgreSQL permission-denied error specifically (not the
	//    function's own 42501 on claim mismatch). The two are
	//    distinguishable via the error message.
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
		t.Fatalf("err = %v, want SQLSTATE 42501", err)
	}
	// "permission denied for function" comes from the PG executor when
	// the role lacks EXECUTE, BEFORE the body runs. "cross-org Vault
	// access denied" comes from inside the function. Pinning the
	// message distinguishes catalog-level denial from in-function
	// authorization.
	if !strings.Contains(pgErr.Message, "permission denied for function") {
		t.Errorf("err message = %q, want 'permission denied for function ...' (catalog-level denial, not in-function check)", pgErr.Message)
	}
}

// TestProjectKnowledge_OCC — two transactions both try to update
// project_knowledge with expected_version=1. One succeeds, one raises
// SQLSTATE 40001. Also asserts last_updated_by is derived from the
// JWT-claims user, not accepted from the caller.
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
			`SELECT update_project_knowledge($1, $2, $3, NULL)`,
			pkID, 1, "v2",
		).Scan(&newVer)
	})
	if err != nil {
		t.Fatalf("first update: %v", err)
	}
	if newVer != 2 {
		t.Errorf("first new_version = %d, want 2", newVer)
	}

	// last_updated_by must equal the JWT-claims user, not anything
	// the caller could pass.
	var lastBy string
	if err := h.AdminDB.QueryRow(
		`SELECT last_updated_by FROM project_knowledge WHERE id = $1`, pkID,
	).Scan(&lastBy); err != nil {
		t.Fatalf("read last_updated_by: %v", err)
	}
	if lastBy != user {
		t.Errorf("last_updated_by = %s, want %s (derived from current_user_id)", lastBy, user)
	}

	// Second update with stale expected_version=1 — must raise 40001.
	err = h.WithUser(t, user, org, func(tx *sql.Tx) error {
		return tx.QueryRow(
			`SELECT update_project_knowledge($1, $2, $3, NULL)`,
			pkID, 1, "v3",
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

// TestProjectKnowledge_RunValidation — passing a p_updated_by_run that
// belongs to another user fails because runs RLS hides it. Without
// this gate, a caller could attribute their KB write to someone else's
// run, polluting audit chronology.
func TestProjectKnowledge_RunValidation(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, _ := seedOrgWithUser(t, h, "alice")
	_, bob, _ := seedOrgWithUser(t, h, "bob")

	projectID := seedProject(t, h, orgA, alice, "demo")
	var pkID string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO project_knowledge (org_id, project_id, key, content)
		VALUES ($1, $2, 'overview', 'v1') RETURNING id
	`, orgA, projectID).Scan(&pkID); err != nil {
		t.Fatalf("seed KB: %v", err)
	}

	// Seed bob's run in his own org. Use AdminDB so we don't have to
	// build the full task/event/prompt chain.
	bobOrg := seedOrg(t, h, "bob-other-org", bob)
	bobTeam := seedTeam(t, h, bobOrg, "default")
	addOrgMember(t, h, bob, bobOrg, bobTeam, "owner", "admin")
	bobEntity := seedEntity(t, h, bobOrg, "github", "octo/repo#99")
	bobTask := seedTask(t, h, bobOrg, bob, bobEntity, "github:pr:opened")
	bobPrompt := seedPrompt(t, h, bobOrg, bob, "p1")
	var bobRun string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO runs (org_id, creator_user_id, task_id, prompt_id)
		VALUES ($1, $2, $3, $4) RETURNING id
	`, bobOrg, bob, bobTask, bobPrompt).Scan(&bobRun); err != nil {
		t.Fatalf("seed bob run: %v", err)
	}

	// Alice tries to attribute her KB update to bob's run. RLS on
	// runs hides bob's row from alice's session, so the EXISTS check
	// inside the function fails.
	err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		var v int
		return tx.QueryRow(
			`SELECT update_project_knowledge($1, $2, $3, $4)`,
			pkID, 1, "v2-with-stolen-run", bobRun,
		).Scan(&v)
	})
	if err == nil {
		t.Fatalf("update with stolen run did not error — run validation broken")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("err = %v, want SQLSTATE 42501", err)
	}
}

// TestRLS_TeamVisibilityIsTeamScoped — a row with visibility='team'
// must only be visible to members of that specific team_id, not to
// every org member. Covers all four tables that use this pattern:
// prompts, projects, task_rules, prompt_triggers.
//
// Subtle bug this guards against: in the EXISTS subquery,
// `m.team_id = team_id` is ambiguous — SQL name resolution binds the
// unqualified `team_id` to memberships.team_id (innermost scope),
// making the predicate `m.team_id = m.team_id` which is always true
// for any membership row the EXISTS scans. The correct form
// qualifies the outer table explicitly: `m.team_id = <outer>.team_id`.
// All four policies had this footgun; this test exercises each.
func TestRLS_TeamVisibilityIsTeamScoped(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := seedOrgWithUser(t, h, "alice")
	bob := seedUser(t, h, "bob")
	carol := seedUser(t, h, "carol")
	// Bob joins teamA; carol joins a NEW team in the same org. Both
	// are org-level 'member' (only alice as founder is org-level admin).
	addOrgMember(t, h, bob, orgA, teamA, "member", "member")
	teamB := seedTeam(t, h, orgA, "team-b")
	addOrgMember(t, h, carol, orgA, teamB, "member", "member")

	// Seed one team-scoped row in each of the four tables.
	prompt := seedPrompt(t, h, orgA, alice, "p1")
	var teamPromptID, teamProjectID, teamRuleID, teamTriggerID string

	if err := h.AdminDB.QueryRow(`
		INSERT INTO prompts (org_id, creator_user_id, team_id, visibility, name, body)
		VALUES ($1, $2, $3, 'team', 'team-prompt', '') RETURNING id
	`, orgA, alice, teamA).Scan(&teamPromptID); err != nil {
		t.Fatalf("seed team prompt: %v", err)
	}
	if err := h.AdminDB.QueryRow(`
		INSERT INTO projects (org_id, creator_user_id, team_id, visibility, name)
		VALUES ($1, $2, $3, 'team', 'team-proj') RETURNING id
	`, orgA, alice, teamA).Scan(&teamProjectID); err != nil {
		t.Fatalf("seed team project: %v", err)
	}
	if err := h.AdminDB.QueryRow(`
		INSERT INTO task_rules (org_id, creator_user_id, team_id, visibility, event_type, name)
		VALUES ($1, $2, $3, 'team', 'github:pr:opened', 'team-rule') RETURNING id
	`, orgA, alice, teamA).Scan(&teamRuleID); err != nil {
		t.Fatalf("seed team rule: %v", err)
	}
	if err := h.AdminDB.QueryRow(`
		INSERT INTO prompt_triggers (org_id, creator_user_id, team_id, visibility, prompt_id, event_type)
		VALUES ($1, $2, $3, 'team', $4, 'github:pr:opened') RETURNING id
	`, orgA, alice, teamA, prompt).Scan(&teamTriggerID); err != nil {
		t.Fatalf("seed team trigger: %v", err)
	}

	// Bob (in teamA) should see all four.
	err := h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		for _, c := range []struct {
			label, query, id string
		}{
			{"prompts", `SELECT 1 FROM prompts WHERE id = $1`, teamPromptID},
			{"projects", `SELECT 1 FROM projects WHERE id = $1`, teamProjectID},
			{"task_rules", `SELECT 1 FROM task_rules WHERE id = $1`, teamRuleID},
			{"prompt_triggers", `SELECT 1 FROM prompt_triggers WHERE id = $1`, teamTriggerID},
		} {
			var n int
			if err := tx.QueryRow(c.query, c.id).Scan(&n); err != nil {
				t.Errorf("bob can't see teamA-scoped %s row: %v", c.label, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob query: %v", err)
	}

	// Carol (in teamB, same org, DIFFERENT team) must NOT see any of
	// the four. The unqualified-team_id bug would let her see all of
	// them.
	err = h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		for _, c := range []struct {
			label, query, id string
		}{
			{"prompts", `SELECT COUNT(*) FROM prompts WHERE id = $1`, teamPromptID},
			{"projects", `SELECT COUNT(*) FROM projects WHERE id = $1`, teamProjectID},
			{"task_rules", `SELECT COUNT(*) FROM task_rules WHERE id = $1`, teamRuleID},
			{"prompt_triggers", `SELECT COUNT(*) FROM prompt_triggers WHERE id = $1`, teamTriggerID},
		} {
			var n int
			if err := tx.QueryRow(c.query, c.id).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("carol (different team) saw teamA-scoped %s row — outer-table-qualified team_id check broken", c.label)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("carol query: %v", err)
	}
}

// TestRLS_RevokedMembership — if a user's session still carries
// org_id claims after the underlying membership is gone, RLS must
// still gate them. Without `tf.user_has_org_access(org_id)` in the
// USING clause, alice could read tasks she created in orgA even
// after being kicked out of orgA. The check is on top of the
// claims match (current_org_id), giving us two independent gates.
func TestRLS_RevokedMembership(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, _ := seedOrgWithUser(t, h, "alice")
	entityA := seedEntity(t, h, orgA, "github", "octo/repo#1")
	taskA := seedTask(t, h, orgA, alice, entityA, "github:pr:opened")

	// Sanity: alice can see her task pre-revocation.
	err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		var id string
		return tx.QueryRow(`SELECT id FROM tasks WHERE id = $1`, taskA).Scan(&id)
	})
	if err != nil {
		t.Fatalf("alice pre-revocation: %v", err)
	}

	// Revoke alice's membership at BOTH levels (admin path; tests as
	// supabase_admin). Removing the team membership alone is no
	// longer enough — user_has_org_access queries org_memberships in
	// the two-axis model.
	mustExec(t, h.AdminDB, `DELETE FROM org_memberships WHERE user_id = $1`, alice)
	mustExec(t, h.AdminDB, `DELETE FROM memberships WHERE user_id = $1`, alice)

	// Alice's session still carries claims {sub: alice, org_id: orgA}
	// but org_memberships row is gone → tf.user_has_org_access(orgA) now
	// returns false. Task SELECT must return 0 rows.
	err = h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT id FROM tasks`)
		if err != nil {
			return err
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			count++
		}
		if count != 0 {
			t.Errorf("alice saw %d tasks after membership revoked, want 0", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("alice post-revocation: %v", err)
	}
}

// TestRLS_OrgAdminGate — non-admin members can read orgs/teams but
// can't UPDATE the org row (rename, flip sso_enforced, etc.) or
// CREATE/DELETE teams. Catches the original "any member could
// mutate org-wide attributes" privilege escalation.
func TestRLS_OrgAdminGate(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	// Alice is the org owner; bob is a plain org+team member.
	orgA, alice, teamA := seedOrgWithUser(t, h, "alice")
	bob := seedUser(t, h, "bob")
	addOrgMember(t, h, bob, orgA, teamA, "member", "member")

	// Alice (owner) can rename the org.
	err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE orgs SET name = 'renamed' WHERE id = $1`, orgA)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			t.Errorf("alice (owner) UPDATE affected %d rows, want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("alice rename: %v", err)
	}

	// Bob (member) CANNOT rename the org. RLS UPDATE policy filters
	// the row out; UPDATE affects 0 rows.
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE orgs SET name = 'bob-takeover' WHERE id = $1`, orgA)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("bob (member) UPDATE affected %d rows, want 0 (admin gate)", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob rename attempt: %v", err)
	}

	// Bob CANNOT create a new team — INSERT WITH CHECK fails.
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO teams (org_id, slug, name) VALUES ($1, 'bob-team', 'Bob Team')`, orgA,
		)
		return err
	})
	if err == nil {
		t.Errorf("bob (member) created a new team — admin gate broken")
	} else if !strings.Contains(err.Error(), "row-level security") {
		t.Errorf("expected RLS violation on team INSERT, got: %v", err)
	}

	// Bob CANNOT delete the existing team.
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM teams WHERE id = $1`, teamA)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("bob (member) DELETE on team affected %d rows, want 0", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob team delete: %v", err)
	}

	// Bob (member) CAN still SELECT the org + team — read access
	// is still org-wide.
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		var name string
		if err := tx.QueryRow(`SELECT name FROM orgs WHERE id = $1`, orgA).Scan(&name); err != nil {
			t.Errorf("bob SELECT org: %v", err)
		}
		if err := tx.QueryRow(`SELECT name FROM teams WHERE id = $1`, teamA).Scan(&name); err != nil {
			t.Errorf("bob SELECT team: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob read-only access: %v", err)
	}
}

// TestRLS_SettingsAdminOnly — non-admin members can read org_settings
// + jira_project_status_rules but can't write them.
func TestRLS_SettingsAdminOnly(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := seedOrgWithUser(t, h, "alice")
	bob := seedUser(t, h, "bob")
	addOrgMember(t, h, bob, orgA, teamA, "member", "member")

	// Alice (owner) creates an org_settings row.
	err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO org_settings (org_id) VALUES ($1)`, orgA)
		return err
	})
	if err != nil {
		t.Fatalf("alice (owner) INSERT org_settings: %v", err)
	}

	// Bob can SELECT (org member).
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		var poll string
		return tx.QueryRow(
			`SELECT github_poll_interval::text FROM org_settings WHERE org_id = $1`, orgA,
		).Scan(&poll)
	})
	if err != nil {
		t.Errorf("bob SELECT org_settings: %v", err)
	}

	// Bob cannot UPDATE — UPDATE policy admin-gated; filters row out.
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE org_settings SET github_poll_interval = '1 minute' WHERE org_id = $1`, orgA,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("bob UPDATE org_settings affected %d rows, want 0", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob update attempt: %v", err)
	}

	// Bob cannot INSERT a jira rule — WITH CHECK fails.
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO jira_project_status_rules (org_id, project_key)
			VALUES ($1, 'SKY')
		`, orgA)
		return err
	})
	if err == nil {
		t.Errorf("bob INSERT jira rule succeeded — admin gate broken")
	} else if !strings.Contains(err.Error(), "row-level security") {
		t.Errorf("expected RLS violation, got: %v", err)
	}

	// Alice (owner) can INSERT a jira rule.
	err = h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO jira_project_status_rules (org_id, project_key)
			VALUES ($1, 'SKY')
		`, orgA)
		return err
	})
	if err != nil {
		t.Fatalf("alice INSERT jira rule: %v", err)
	}
}

// TestRLS_OrgBootstrap — a logged-in user can create a brand-new org
// (where they will be owner), the first team, and their own initial
// membership row, all from within a single tf_app transaction.
// Without an INSERT policy on orgs and bootstrap-aware INSERT policy
// on memberships, the entire signup flow would fail and force
// service_role / supabase_admin code paths.
//
// Pre-bootstrap, the user has no membership anywhere, so claims
// include sub but org_id is unset — the user is "logged in but no
// active tenant context".
func TestRLS_OrgBootstrap(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	// Seed a fresh auth.users + public.users row but no memberships.
	dave := seedUser(t, h, "dave")
	alice := seedUser(t, h, "alice") // for the negative cross-owner test

	// Phase 1: dave creates an org where HE is owner. Each negative
	// assertion gets its own tx so a Postgres tx-abort doesn't
	// invalidate the prior successful INSERT.
	orgID := withDaveTx(t, h, dave, func(tx *sql.Tx) string {
		var id string
		if err := tx.QueryRow(`
			INSERT INTO orgs (slug, name, owner_user_id) VALUES ('dave-org', 'Dave Org', $1) RETURNING id
		`, dave).Scan(&id); err != nil {
			t.Fatalf("dave INSERT org: %v", err)
		}
		return id
	})

	// Phase 2 (negative): dave CANNOT create an org owned by alice.
	withDaveTx(t, h, dave, func(tx *sql.Tx) struct{} {
		_, err := tx.Exec(`
			INSERT INTO orgs (slug, name, owner_user_id) VALUES ('alice-stolen', 'Stolen', $1)
		`, alice)
		if err == nil {
			t.Errorf("dave created an org owned by alice — orgs_insert policy too loose")
		} else if !strings.Contains(err.Error(), "row-level security") {
			t.Errorf("expected RLS violation, got: %v", err)
		}
		return struct{}{}
	})

	// Phase 3 (negative): dave CANNOT yet create a team — teams_insert
	// requires user_is_org_admin, which queries org_memberships, and
	// dave has no row there yet.
	withDaveTx(t, h, dave, func(tx *sql.Tx) struct{} {
		_, err := tx.Exec(`
			INSERT INTO teams (org_id, slug, name) VALUES ($1, 'default', 'Default')
		`, orgID)
		if err == nil {
			t.Errorf("dave (no org_memberships yet) created a team — admin gate broken")
		}
		return struct{}{}
	})

	// Phase 4: dave self-inserts his org_memberships row as 'owner'.
	// The org_memberships_insert bootstrap branch (tf.user_owns_org)
	// permits this — he founded the org per orgs.owner_user_id.
	withDaveTx(t, h, dave, func(tx *sql.Tx) struct{} {
		if _, err := tx.Exec(`
			INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'owner')
		`, dave, orgID); err != nil {
			t.Fatalf("dave self-insert org_memberships (bootstrap): %v", err)
		}
		return struct{}{}
	})

	// Phase 5: dave is now an org admin (via org_memberships). He can
	// create teams and self-insert team memberships, all through
	// tf_app — no supabase_admin needed.
	var teamID string
	if err := h.WithUser(t, dave, orgID, func(tx *sql.Tx) error {
		if err := tx.QueryRow(`
			INSERT INTO teams (org_id, slug, name) VALUES ($1, 'default', 'Default') RETURNING id
		`, orgID).Scan(&teamID); err != nil {
			return fmt.Errorf("INSERT team: %w", err)
		}
		if _, err := tx.Exec(`
			INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')
		`, dave, teamID); err != nil {
			return fmt.Errorf("INSERT memberships: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("dave team + memberships bootstrap: %v", err)
	}
}

// withDaveTx runs fn in a fresh tf_app tx with no active org context
// (org_id claim = ""). Used by TestRLS_OrgBootstrap to model the
// post-signup, pre-first-org state where the user is logged in but
// hasn't joined any org yet.
func withDaveTx[T any](t *testing.T, h *Harness, userID string, fn func(tx *sql.Tx) T) T {
	t.Helper()
	ctx := context.Background()
	tx, err := h.AppDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort cleanup
	if _, err := tx.Exec(`SET LOCAL ROLE tf_app`); err != nil {
		t.Fatalf("SET LOCAL ROLE: %v", err)
	}
	claims := fmt.Sprintf(`{"sub":"%s","org_id":""}`, userID)
	if _, err := tx.Exec(`SELECT set_config('request.jwt.claims', $1, true)`, claims); err != nil {
		t.Fatalf("set_config: %v", err)
	}
	result := fn(tx)
	// Commit so subsequent calls see what fn wrote (matters for the
	// positive-INSERT branch; negative branches abort the tx via the
	// failed statement and commit silently no-ops).
	_ = tx.Commit()
	return result
}

// TestRLS_MembershipManagement — exercises the four write policies on
// memberships. Admin can add/promote/remove; non-admin can only
// self-leave.
func TestRLS_MembershipManagement(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := seedOrgWithUser(t, h, "alice")
	bob := seedUser(t, h, "bob")
	carol := seedUser(t, h, "carol")
	// Bob and carol are org members but not yet on any team. This
	// test asserts the team-membership write policies; the org-
	// membership ones are exercised by TestRLS_OrgBootstrap.
	mustExec(t, h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, bob, orgA)
	mustExec(t, h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, carol, orgA)

	// 1. Alice (team admin / org owner) can add bob to the team.
	err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`,
			bob, teamA,
		)
		return err
	})
	if err != nil {
		t.Fatalf("alice (admin) INSERT bob: %v", err)
	}

	// 2. Bob (now a plain member, not admin) cannot add carol.
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`,
			carol, teamA,
		)
		return err
	})
	if err == nil {
		t.Errorf("bob (non-admin) INSERT carol succeeded — admin gate broken")
	}

	// 3. Bob cannot promote himself.
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE memberships SET role = 'admin' WHERE user_id = $1 AND team_id = $2`,
			bob, teamA,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("bob self-promotion affected %d rows, want 0", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob self-promotion attempt: %v", err)
	}

	// 4. Bob CAN self-leave (DELETE his own membership).
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`DELETE FROM memberships WHERE user_id = $1 AND team_id = $2`,
			bob, teamA,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			t.Errorf("bob self-leave affected %d rows, want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob self-leave: %v", err)
	}
}

// TestFK_CrossOrgRejected pins the composite-FK defense-in-depth.
// Even via AdminDB (RLS bypassed!) a row cannot be inserted that
// FK-references a parent in a different org. This catches bugs in
// app code or compromised internal calls that RLS alone wouldn't.
func TestFK_CrossOrgRejected(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, _ := seedOrgWithUser(t, h, "alice")
	orgB, bob, _ := seedOrgWithUser(t, h, "bob")

	entityB := seedEntity(t, h, orgB, "github", "octo/repo#1")

	// Try to INSERT a task in orgA referencing entityB (which is in
	// orgB). Composite FK (entity_id, org_id) → entities(id, org_id)
	// rejects: there's no entities row with (entityB, orgA).
	// AdminDB bypasses RLS but FKs are enforced regardless of role.
	_, err := h.AdminDB.Exec(`
		INSERT INTO tasks (org_id, creator_user_id, entity_id, event_type, primary_event_id)
		VALUES ($1, $2, $3, 'github:pr:opened', gen_random_uuid())
	`, orgA, alice, entityB)
	if err == nil {
		t.Fatalf("cross-org task INSERT succeeded — composite FK broken")
	}
	if !strings.Contains(err.Error(), "foreign key constraint") {
		t.Errorf("err = %v, want foreign key violation", err)
	}

	// Same shape: try to INSERT an event in orgA referencing
	// entityB. Composite FK rejects.
	_, err = h.AdminDB.Exec(`
		INSERT INTO events (org_id, entity_id, event_type) VALUES ($1, $2, 'github:pr:opened')
	`, orgA, entityB)
	if err == nil {
		t.Fatalf("cross-org event INSERT succeeded — composite FK broken")
	}

	// And a project in orgA referencing a prompt in orgB.
	bobPrompt := seedPrompt(t, h, orgB, bob, "bob-prompt")
	_, err = h.AdminDB.Exec(`
		INSERT INTO projects (org_id, creator_user_id, name, spec_authorship_prompt_id)
		VALUES ($1, $2, 'p', $3)
	`, orgA, alice, bobPrompt)
	if err == nil {
		t.Fatalf("cross-org project→prompt INSERT succeeded — composite FK broken")
	}
}

// TestRLS_ChildTablesInheritParentVisibility — denormalized child
// rows (task_events, run_artifacts, run_messages, run_memory,
// run_worktrees, pending_prs, pending_firings) must NOT be visible
// to org members who can't see the parent task/run. Earlier policies
// gated only on org_id, leaking metadata across users in the same org.
// EXISTS-on-parent inherits the parent table's RLS.
func TestRLS_ChildTablesInheritParentVisibility(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := seedOrgWithUser(t, h, "alice")
	bob := seedUser(t, h, "bob")
	addOrgMember(t, h, bob, orgA, teamA, "member", "member")

	// Alice creates a task + a run + child rows. All in orgA.
	entityA := seedEntity(t, h, orgA, "github", "octo/repo#1")
	taskID := seedTask(t, h, orgA, alice, entityA, "github:pr:opened")
	prompt := seedPrompt(t, h, orgA, alice, "p1")
	var runID string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO runs (org_id, creator_user_id, task_id, prompt_id) VALUES ($1, $2, $3, $4) RETURNING id
	`, orgA, alice, taskID, prompt).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	// Seed one child row per parent kind we care about.
	mustExec(t, h.AdminDB, `INSERT INTO task_events (org_id, task_id, event_id, kind)
		SELECT $1, $2, e.id, 'closed' FROM events e WHERE e.entity_id = $3 LIMIT 1`,
		orgA, taskID, entityA)
	mustExec(t, h.AdminDB, `INSERT INTO run_artifacts (org_id, run_id, kind) VALUES ($1, $2, 'pr')`,
		orgA, runID)
	mustExec(t, h.AdminDB, `INSERT INTO run_messages (org_id, run_id, role, content) VALUES ($1, $2, 'assistant', 'hi')`,
		orgA, runID)
	mustExec(t, h.AdminDB, `INSERT INTO run_memory (org_id, run_id, entity_id, agent_content) VALUES ($1, $2, $3, 'note')`,
		orgA, runID, entityA)
	mustExec(t, h.AdminDB, `INSERT INTO run_worktrees (org_id, run_id, repo_id, path, feature_branch) VALUES ($1, $2, 'octo/repo', '/tmp/x', 'feat/x')`,
		orgA, runID)
	mustExec(t, h.AdminDB, `INSERT INTO pending_prs (org_id, run_id, owner, repo, head_branch, head_sha, base_branch, title) VALUES ($1, $2, 'octo', 'repo', 'feat/x', 'sha', 'main', 'PR')`,
		orgA, runID)

	// Alice sees all her child rows.
	err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		for _, table := range []string{"task_events", "run_artifacts", "run_messages", "run_memory", "run_worktrees", "pending_prs"} {
			var n int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
				return err
			}
			if n != 1 {
				t.Errorf("alice saw %d %s rows, want 1", n, table)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("alice query: %v", err)
	}

	// Bob (same org, NOT the run/task creator) must see ZERO of these
	// child rows — tasks_select and runs_select gate on creator, so
	// the EXISTS-on-parent in each child policy returns false for him.
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		for _, table := range []string{"task_events", "run_artifacts", "run_messages", "run_memory", "run_worktrees", "pending_prs"} {
			var n int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("bob saw %d %s rows, want 0 — child policy leaked across users", n, table)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob query: %v", err)
	}
}

// TestGooseDBVersionLockdown — tf_app must not have any privileges
// on the goose migration tracking table. The bulk
// `GRANT ... ON ALL TABLES IN SCHEMA public TO tf_app` accidentally
// covered goose_db_version (created in public by goose itself); the
// migration's REVOKE locks it back down so the application role can't
// fake migration state.
func TestGooseDBVersionLockdown(t *testing.T) {
	h := Shared(t)

	for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
		var has bool
		if err := h.AdminDB.QueryRow(
			`SELECT has_table_privilege('tf_app', 'public.goose_db_version', $1)`, priv,
		).Scan(&has); err != nil {
			t.Fatalf("has_table_privilege(tf_app, goose_db_version, %s): %v", priv, err)
		}
		if has {
			t.Errorf("tf_app has %s on goose_db_version — migration state tampering vector", priv)
		}
	}
	// Sequence too.
	var seqHas bool
	if err := h.AdminDB.QueryRow(
		`SELECT has_sequence_privilege('tf_app', 'public.goose_db_version_id_seq', 'USAGE')`,
	).Scan(&seqHas); err != nil {
		t.Fatalf("has_sequence_privilege: %v", err)
	}
	if seqHas {
		t.Errorf("tf_app has USAGE on goose_db_version_id_seq")
	}
}

// TestRLS_TeamAdminNotOrgAdmin pins the two-axis role model: a team
// admin who is only an org member (not an org admin) can manage their
// own team but CANNOT mutate org-wide attributes. This is the
// scenario the previous one-axis model silently allowed — anyone
// promoted to admin of any subteam got org-wide write access.
func TestRLS_TeamAdminNotOrgAdmin(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	// Alice founds the org. Carol is added as a regular org member,
	// then made admin of a subteam (mobile-team).
	orgA, alice, teamMain := seedOrgWithUser(t, h, "alice")
	carol := seedUser(t, h, "carol")
	teamMobile := seedTeam(t, h, orgA, "mobile-team")
	addOrgMember(t, h, carol, orgA, teamMobile, "member", "admin")
	_ = teamMain // unused; alice's team

	// Carol CAN manage her team's settings.
	err := h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO team_settings (team_id, jira_projects)
			VALUES ($1, ARRAY['SKY','MOB'])
		`, teamMobile)
		return err
	})
	if err != nil {
		t.Errorf("carol (team admin) INSERT team_settings on her own team: %v", err)
	}

	// Carol CANNOT rename the org (org admin only).
	err = h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE orgs SET name = 'carol-takeover' WHERE id = $1`, orgA)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("team-admin-only carol UPDATE'd orgs.name (%d rows) — two-axis broken", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("carol rename attempt: %v", err)
	}

	// Carol CANNOT flip sso_enforced.
	err = h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE orgs SET sso_enforced = TRUE WHERE id = $1`, orgA)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("team-admin-only carol flipped sso_enforced (%d rows) — two-axis broken", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("carol sso_enforced attempt: %v", err)
	}

	// Carol CANNOT write org_settings.
	err = h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO org_settings (org_id) VALUES ($1)`, orgA)
		return err
	})
	if err != nil {
		t.Fatalf("alice seed org_settings: %v", err)
	}
	err = h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE org_settings SET github_poll_interval = '1 minute' WHERE org_id = $1`, orgA,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("team-admin-only carol UPDATE'd org_settings (%d rows)", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("carol org_settings attempt: %v", err)
	}

	// Carol CANNOT create a new team in the org.
	err = h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO teams (org_id, slug, name) VALUES ($1, 'sneaky', 'Sneaky')`, orgA,
		)
		return err
	})
	if err == nil {
		t.Errorf("team-admin-only carol created a new team — should require org admin")
	}

	// Sanity: alice (org owner) can do all of the above.
	err = h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE orgs SET name = 'renamed-by-alice' WHERE id = $1`, orgA)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			t.Errorf("alice (org owner) rename affected %d rows, want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("alice sanity rename: %v", err)
	}
}

// --- fixture helpers ---

// addOrgMember adds a user to an org with the given org_role and the
// given team_role on the named team. Modeled on the production
// "admin invites a user" flow (D7's auth middleware will materialize
// both rows together).
func addOrgMember(t *testing.T, h *Harness, userID, orgID, teamID, orgRole, teamRole string) {
	t.Helper()
	mustExec(t, h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, $3)`, userID, orgID, orgRole)
	mustExec(t, h.AdminDB,
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, $3)`, userID, teamID, teamRole)
}

func seedOrgWithUser(t *testing.T, h *Harness, displayName string) (orgID, userID, teamID string) {
	t.Helper()
	userID = seedUser(t, h, displayName)
	orgID = seedOrg(t, h, displayName+"-org", userID)
	teamID = seedTeam(t, h, orgID, "default")
	// Two-axis roles (matches GitHub/GitLab/Linear): the founder is
	// org-level 'owner' (via org_memberships) AND team-level 'admin'
	// (via memberships). Membership_role no longer has 'owner';
	// owning is an org concept now.
	mustExec(t, h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'owner')`, userID, orgID)
	mustExec(t, h.AdminDB,
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')`, userID, teamID)
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

func seedPrompt(t *testing.T, h *Harness, orgID, creatorID, name string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO prompts (org_id, creator_user_id, name, body) VALUES ($1, $2, $3, '') RETURNING id
	`, orgID, creatorID, name).Scan(&id); err != nil {
		t.Fatalf("seed prompt: %v", err)
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
