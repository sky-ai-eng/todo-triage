package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestAgentRunStore_Postgres runs the shared conformance suite
// against the Postgres AgentRunStore impl. Each subtest gets a
// fresh org + team + user + prompt + agent seed; the suite drives
// every method through its happy and edge paths.
func TestAgentRunStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	// Wire both pools against AdminDB so the run lifecycle
	// statements run without a JWT-claims tx. Production wiring
	// uses the app pool, but the conformance suite is about
	// behavior, not auth; the cross-org leakage test below
	// exercises the org_id defense-in-depth filter directly.
	stores := pgstore.New(h.AdminDB, h.AdminDB)

	dbtest.RunAgentRunStoreConformance(t, func(t *testing.T) (db.AgentRunStore, string, string, dbtest.AgentRunSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, agentID := seedPgAgentRunOrg(t, h)
		promptID := seedPgAgentRunPrompt(t, h, orgID, userID)
		seeder := newPgAgentRunSeeder(h.AdminDB, orgID, userID, agentID, promptID)
		return stores.AgentRuns, orgID, userID, seeder
	})
}

// seedPgAgentRunOrg builds the auth.user + public.user + org +
// org_membership + default team + agent graph the AgentRunStore
// needs. Mirrors seedPgOrgUserAgent from tasks_test.go.
func seedPgAgentRunOrg(t *testing.T, h *pgtest.Harness) (orgID, userID, agentID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	agentID = uuid.New().String()
	email := fmt.Sprintf("agentrun-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "AgentRun Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "AgentRun Org "+orgID[:8], "ar-"+orgID[:8], userID,
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
		`INSERT INTO agents (id, org_id, display_name) VALUES ($1, $2, 'Conformance Bot')`,
		agentID, orgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return orgID, userID, agentID
}

// seedPgAgentRunPrompt inserts a user-source prompt the conformance
// suite's runs FK into. Stable id `p_agentrun_test` matches the
// constant the shared harness expects.
func seedPgAgentRunPrompt(t *testing.T, h *pgtest.Harness, orgID, userID string) string {
	t.Helper()
	teamID := firstTeamForOrg(t, h, orgID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, kind, allowed_tools, visibility, created_at, updated_at)
		VALUES ('p_agentrun_test', $1, $2, $3, 'AgentRun Test', 'body', 'user', 'leaf', '', 'team', now(), now())
	`, orgID, userID, teamID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return "p_agentrun_test"
}

// newPgAgentRunSeeder builds the FactorySeeder-style callbacks the
// conformance harness uses to stage non-run fixture rows. INSERTs
// carry org_id explicitly so the cross-org leakage test below can
// reuse the same seeder for two orgs in parallel.
func newPgAgentRunSeeder(conn *sql.DB, orgID, userID, agentID, promptID string) dbtest.AgentRunSeeder {
	_ = promptID // referenced via the conformance suite's constant
	return dbtest.AgentRunSeeder{
		Entity: func(t *testing.T, suffix string) string {
			t.Helper()
			id := uuid.New().String()
			sourceID := fmt.Sprintf("agentrun-%s-%s", suffix, id[:8])
			if _, err := conn.Exec(`
				INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
				VALUES ($1, $2, 'github', $3, 'pr', $4, $5, '{}'::jsonb, $6)
			`, id, orgID, sourceID, "Conformance "+suffix, "https://example/"+sourceID, time.Now().UTC()); err != nil {
				t.Fatalf("seed entity %s: %v", suffix, err)
			}
			return id
		},
		Event: func(t *testing.T, entityID, eventType string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
				VALUES ($1, $2, $3, $4, '', '{}'::jsonb, $5)
			`, id, orgID, entityID, eventType, time.Now().UTC()); err != nil {
				t.Fatalf("seed event %s: %v", eventType, err)
			}
			return id
		},
		Task: func(t *testing.T, entityID, eventType, primaryEventID string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id,
				                   status, scoring_status, priority_score, created_at)
				VALUES ($1, $2, $3,
				        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
				        'team', $4, $5, '', $6, 'queued', 'pending', 0.5, $7)
			`, id, orgID, userID, entityID, eventType, primaryEventID, time.Now().UTC()); err != nil {
				t.Fatalf("seed task: %v", err)
			}
			return id
		},
		StampAgentClaim: func(t *testing.T, taskID, agent string) {
			t.Helper()
			if _, err := conn.Exec(
				`UPDATE tasks SET claimed_by_agent_id = $1::uuid, claimed_by_user_id = NULL WHERE id = $2 AND org_id = $3`,
				agent, taskID, orgID,
			); err != nil {
				t.Fatalf("stamp claim: %v", err)
			}
		},
		SetRunMemory: func(t *testing.T, runID, entityID, content string) {
			t.Helper()
			memID := uuid.New().String()
			if content == dbtest.NullMemorySentinel {
				if _, err := conn.Exec(`
					INSERT INTO run_memory (id, org_id, run_id, entity_id, agent_content) VALUES ($1, $2, $3, $4, NULL)
				`, memID, orgID, runID, entityID); err != nil {
					t.Fatalf("seed null memory: %v", err)
				}
				return
			}
			if _, err := conn.Exec(`
				INSERT INTO run_memory (id, org_id, run_id, entity_id, agent_content) VALUES ($1, $2, $3, $4, $5)
			`, memID, orgID, runID, entityID, content); err != nil {
				t.Fatalf("seed memory: %v", err)
			}
		},
		AgentID: agentID,
	}
}

// TestAgentRunStore_Postgres_CrossOrgLeakage pins the defense-in-
// depth guarantee: even with the org_id filter as the only line of
// defense (AdminDB bypasses RLS), org A's queries can't see org B's
// runs. In production the RLS policies add a second layer; this test
// validates the WHERE-clause filter on its own so a regression there
// can't silently rely on RLS to compensate.
func TestAgentRunStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, userA, agentA := seedPgAgentRunOrg(t, h)
	orgB, userB, agentB := seedPgAgentRunOrg(t, h)
	_ = agentA
	_ = agentB
	seedPgAgentRunPromptIn(t, h, "p_xleak_A", orgA, userA)
	seedPgAgentRunPromptIn(t, h, "p_xleak_B", orgB, userB)

	stores := pgstore.New(h.AdminDB, h.AdminDB)
	ctx := context.Background()

	// Seed an entity + task + run in each org via the AdminDB so
	// the FK chain is satisfied.
	mkChain := func(t *testing.T, orgID, userID, promptID, runID string) (taskID string) {
		t.Helper()
		entityID := uuid.New().String()
		eventID := uuid.New().String()
		taskID = uuid.New().String()
		if _, err := h.AdminDB.Exec(`
			INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
			VALUES ($1, $2, 'github', $3, 'pr', 'Cross-org test', '', '{}'::jsonb, now())
		`, entityID, orgID, "xleak-"+orgID[:8]); err != nil {
			t.Fatalf("entity: %v", err)
		}
		if _, err := h.AdminDB.Exec(`
			INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
			VALUES ($1, $2, $3, 'github:pr:opened', '', '{}'::jsonb, now())
		`, eventID, orgID, entityID); err != nil {
			t.Fatalf("event: %v", err)
		}
		if _, err := h.AdminDB.Exec(`
			INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status, priority_score)
			VALUES ($1, $2, $3,
			        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
			        'team', $4, 'github:pr:opened', '', $5, 'queued', 'pending', 0.5)
		`, taskID, orgID, userID, entityID, eventID); err != nil {
			t.Fatalf("task: %v", err)
		}
		if err := stores.AgentRuns.Create(ctx, orgID, domain.AgentRun{
			ID: runID, TaskID: taskID, PromptID: promptID, Status: "running", Model: "m",
			CreatorUserID: userID,
		}); err != nil {
			t.Fatalf("Create run: %v", err)
		}
		return taskID
	}
	runA := uuid.New().String()
	runB := uuid.New().String()
	taskA := mkChain(t, orgA, userA, "p_xleak_A", runA)
	_ = mkChain(t, orgB, userB, "p_xleak_B", runB)

	// Org A's view must NOT see B's run.
	if got, err := stores.AgentRuns.Get(ctx, orgA, runB); err != nil {
		t.Fatalf("Get cross-org: %v", err)
	} else if got != nil {
		t.Errorf("orgA Get returned orgB run %s; defense-in-depth filter leaked", runB)
	}
	if got, err := stores.AgentRuns.Get(ctx, orgB, runA); err != nil {
		t.Fatalf("Get cross-org reverse: %v", err)
	} else if got != nil {
		t.Errorf("orgB Get returned orgA run %s", runA)
	}

	// ListForTask scoped to orgB looking at orgA's task must
	// return nothing.
	if runs, err := stores.AgentRuns.ListForTask(ctx, orgB, taskA); err != nil {
		t.Fatalf("ListForTask cross-org: %v", err)
	} else if len(runs) != 0 {
		t.Errorf("orgB ListForTask(orgA task) returned %d runs; want 0", len(runs))
	}
}

// seedPgAgentRunPromptIn is a small variant that inserts a prompt
// with an explicit id. Used by cross-org leakage which needs two
// prompts in two orgs with distinct ids.
func seedPgAgentRunPromptIn(t *testing.T, h *pgtest.Harness, id, orgID, userID string) {
	t.Helper()
	teamID := firstTeamForOrg(t, h, orgID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, kind, allowed_tools, visibility, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'X-leak Test', 'body', 'user', 'leaf', '', 'team', now(), now())
	`, id, orgID, userID, teamID); err != nil {
		t.Fatalf("seed prompt %s: %v", id, err)
	}
}
