package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestChainStore_Postgres_ReplaceAndListSteps pins the dialect-aware
// SQL: ReplaceSteps must INSERT the org_id column (Postgres-only),
// ListSteps must filter on org_id, and CountStepReferences must count
// distinct chain prompts within the org. The regression this guards
// against is the original chains.go free-function shape that wrote
// ? placeholders against the Postgres tables with no org_id — every
// INSERT would 23502 (NULL constraint) and every WHERE would either
// 42703 (no such column) or leak across tenants.
func TestChainStore_Postgres_ReplaceAndListSteps(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, userID := seedPgOrgForChains(t, h)
	stores := pgstore.New(h.AdminDB, h.AdminDB)
	chains := stores.Chains
	ctx := context.Background()

	chainID := "chain-prompt-" + orgID[:8]
	stepAID := "step-a-" + orgID[:8]
	stepBID := "step-b-" + orgID[:8]
	seedPgPrompt(t, h, orgID, userID, chainID, "chain")
	seedPgPrompt(t, h, orgID, userID, stepAID, "leaf")
	seedPgPrompt(t, h, orgID, userID, stepBID, "leaf")

	if err := chains.ReplaceSteps(ctx, orgID, chainID,
		[]string{stepAID, stepBID}, []string{"brief A", "brief B"}); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}

	steps, err := chains.ListSteps(ctx, orgID, chainID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("len(steps)=%d, want 2", len(steps))
	}
	if steps[0].StepIndex != 0 || steps[0].StepPromptID != stepAID || steps[0].Brief != "brief A" {
		t.Errorf("step 0 = %+v, want index=0 prompt=%s brief='brief A'", steps[0], stepAID)
	}
	if steps[1].StepIndex != 1 || steps[1].StepPromptID != stepBID || steps[1].Brief != "brief B" {
		t.Errorf("step 1 = %+v, want index=1 prompt=%s brief='brief B'", steps[1], stepBID)
	}

	// CountStepReferences should see one chain referencing stepAID.
	n, err := chains.CountStepReferences(ctx, orgID, stepAID)
	if err != nil {
		t.Fatalf("CountStepReferences: %v", err)
	}
	if n != 1 {
		t.Errorf("CountStepReferences = %d, want 1", n)
	}

	// Re-ReplaceSteps with a smaller list — the DELETE+INSERT path
	// inside one tx must collapse to the new list, not append.
	if err := chains.ReplaceSteps(ctx, orgID, chainID, []string{stepBID}, nil); err != nil {
		t.Fatalf("ReplaceSteps (shrink): %v", err)
	}
	steps2, err := chains.ListSteps(ctx, orgID, chainID)
	if err != nil {
		t.Fatalf("ListSteps after shrink: %v", err)
	}
	if len(steps2) != 1 || steps2[0].StepPromptID != stepBID || steps2[0].Brief != "" {
		t.Errorf("after shrink: %+v, want one step with prompt=%s and empty brief", steps2, stepBID)
	}
}

// TestChainStore_Postgres_RunLifecycle exercises CreateRun → GetRun →
// MarkRunStatus → GetLatestVerdict on a real Postgres tx. Covers the
// UUID/TEXT column split (chain_runs.id UUID, chain_prompt_id TEXT)
// and the now()-based verdict timestamp ordering.
func TestChainStore_Postgres_RunLifecycle(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, userID := seedPgOrgForChains(t, h)
	stores := pgstore.New(h.AdminDB, h.AdminDB)
	chains := stores.Chains
	ctx := context.Background()

	chainPromptID := "chain-rl-" + orgID[:8]
	stepPromptID := "step-rl-" + orgID[:8]
	seedPgPrompt(t, h, orgID, userID, chainPromptID, "chain")
	seedPgPrompt(t, h, orgID, userID, stepPromptID, "leaf")
	taskID := seedPgTask(t, h, orgID, userID)

	chainRunID, err := chains.CreateRun(ctx, orgID, domain.ChainRun{
		ChainPromptID: chainPromptID,
		TaskID:        taskID,
		TriggerType:   domain.ChainTriggerManual,
		WorktreePath:  "/tmp/wt-pg-chain",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := uuid.Parse(chainRunID); err != nil {
		t.Errorf("CreateRun returned non-UUID id %q", chainRunID)
	}

	cr, err := chains.GetRun(ctx, orgID, chainRunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if cr == nil {
		t.Fatal("GetRun returned nil for a row we just inserted")
	}
	if cr.Status != domain.ChainRunStatusRunning {
		t.Errorf("status = %q, want running", cr.Status)
	}

	// Seed a step run and write two verdicts; latest wins.
	stepRunID := seedPgRun(t, h, orgID, userID, taskID, stepPromptID, chainRunID, 0)
	advanceJSON, _ := json.Marshal(domain.ChainVerdict{Outcome: domain.ChainVerdictAdvance, Reason: "ok"})
	finalJSON, _ := json.Marshal(domain.ChainVerdict{Outcome: domain.ChainVerdictFinal, Reason: "done"})
	if err := chains.InsertVerdict(ctx, orgID, stepRunID, string(advanceJSON)); err != nil {
		t.Fatalf("InsertVerdict advance: %v", err)
	}
	if err := chains.InsertVerdict(ctx, orgID, stepRunID, string(finalJSON)); err != nil {
		t.Fatalf("InsertVerdict final: %v", err)
	}

	latest, err := chains.GetLatestVerdict(ctx, orgID, stepRunID)
	if err != nil {
		t.Fatalf("GetLatestVerdict: %v", err)
	}
	if latest == nil || latest.Outcome != domain.ChainVerdictFinal {
		t.Errorf("latest = %+v, want final", latest)
	}

	mapped, err := chains.LatestVerdictsForRuns(ctx, orgID, []string{stepRunID})
	if err != nil {
		t.Fatalf("LatestVerdictsForRuns: %v", err)
	}
	if v := mapped[stepRunID]; v == nil || v.Outcome != domain.ChainVerdictFinal {
		t.Errorf("mapped[stepRunID] = %+v, want final", v)
	}

	// Mark the chain completed; second attempt should be no-op.
	changed, err := chains.MarkRunStatus(ctx, orgID, chainRunID, domain.ChainRunStatusCompleted, "", nil)
	if err != nil {
		t.Fatalf("MarkRunStatus: %v", err)
	}
	if !changed {
		t.Error("expected changed=true for running → completed")
	}
	changed2, err := chains.MarkRunStatus(ctx, orgID, chainRunID, domain.ChainRunStatusAborted, "late", nil)
	if err != nil {
		t.Fatalf("MarkRunStatus second: %v", err)
	}
	if changed2 {
		t.Error("expected changed=false on terminal row (race guard)")
	}

	// GetRunForRun resolves step → chain.
	cr2, idx, err := chains.GetRunForRun(ctx, orgID, stepRunID)
	if err != nil {
		t.Fatalf("GetRunForRun: %v", err)
	}
	if cr2 == nil || cr2.ID != chainRunID {
		t.Errorf("GetRunForRun chain = %+v, want id=%s", cr2, chainRunID)
	}
	if idx == nil || *idx != 0 {
		t.Errorf("GetRunForRun stepIdx = %v, want 0", idx)
	}
}

// seedPgOrgForChains creates the (org, user, membership) triplet
// chain row writes need to satisfy creator_user_id FK resolution.
// Returns both ids — chain tests also need the userID directly for
// seeding tasks / runs whose RLS predicate gates on
// creator_user_id = tf.current_user_id().
func seedPgOrgForChains(t *testing.T, h *pgtest.Harness) (orgID, userID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	email := fmt.Sprintf("chains-conf-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Chains Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Chains Conformance Org "+orgID[:8], "chains-"+orgID[:8], userID,
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID,
	); err != nil {
		t.Fatalf("seed org_membership: %v", err)
	}
	return orgID, userID
}

// seedPgPrompt inserts a prompt row at the admin pool so chain step
// FKs resolve. Postgres prompts.id is TEXT (slug-shaped) — same
// surface SQLite uses.
func seedPgPrompt(t *testing.T, h *pgtest.Harness, orgID, userID, id, kind string) {
	t.Helper()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, creator_user_id, name, body, source, kind, allowed_tools, visibility, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'body', 'user', $5, '[]'::jsonb, 'private', now(), now())
	`, id, orgID, userID, id, kind); err != nil {
		t.Fatalf("seed prompt %s: %v", id, err)
	}
}

// seedPgTask inserts a minimal task row so chain_runs.task_id FK is
// satisfied. Tests don't drive the task lifecycle; the row only needs
// to exist in the right org with the right creator.
func seedPgTask(t *testing.T, h *pgtest.Harness, orgID, userID string) string {
	t.Helper()
	entityID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Chains Test Entity', 'https://example/x', '{}'::jsonb, now())
	`, entityID, orgID, "chains-test-"+entityID[:8]); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	eventID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, $4, '', '{}'::jsonb, now())
	`, eventID, orgID, entityID, domain.EventGitHubPRCICheckFailed); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	teamID := seedPgDefaultTeam(t, h, orgID, userID)
	taskID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, entity_id, event_type, dedup_key, primary_event_id, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, '', $7, 'queued', now())
	`, taskID, orgID, userID, teamID, entityID, domain.EventGitHubPRCICheckFailed, eventID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return taskID
}

// seedPgRun inserts a runs row linked to a chain_run for verdict /
// step-to-chain resolution tests.
func seedPgRun(t *testing.T, h *pgtest.Harness, orgID, userID, taskID, promptID, chainRunID string, stepIdx int) string {
	t.Helper()
	runID := uuid.New().String()
	teamID := firstTeamForOrg(t, h, orgID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO runs (id, org_id, creator_user_id, team_id, task_id, prompt_id, status, model, started_at, chain_run_id, chain_step_index)
		VALUES ($1, $2, $3, $4, $5, $6, 'initializing', 'claude-sonnet-4-6', now(), $7, $8)
	`, runID, orgID, userID, teamID, taskID, promptID, chainRunID, stepIdx); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return runID
}
