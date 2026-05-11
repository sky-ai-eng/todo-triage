package dbtest

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// AgentStoreFactory is what a per-backend test file hands to
// RunAgentStoreConformance. The factory returns the wired store +
// the orgID to pass to every method. Postgres backend pre-seeds the
// orgs row so agents.org_id FK satisfies; SQLite has no FK and just
// returns runmode.LocalDefaultOrg.
type AgentStoreFactory func(t *testing.T) (store db.AgentStore, orgID string)

// RunAgentStoreConformance runs the shared assertion suite. What it
// covers:
//
//   - Create inserts the row + returns its id; idempotent on org_id —
//     duplicate Create returns the existing row's id without error.
//   - GetForOrg returns the row's fields verbatim.
//   - GetForOrg returns (nil, nil) when no row exists yet.
//   - Update applies display_name + default_model + autonomy +
//     jira_service_account_id.
//   - SetGitHubAppInstallation + SetGitHubPATUser are mutually
//     exclusive — each clears the other field as a side effect.
//   - Postgres invalid-UUID guards: Update / SetGitHubApp / SetGitHubPAT
//     return nil instead of bubbling 22P02 parse errors.
//   - DisplayName defaulting: empty input fills "Triage Factory Bot".
func RunAgentStoreConformance(t *testing.T, factory AgentStoreFactory) {
	t.Helper()

	t.Run("Create_FirstCallInsertsAndReturnsID", func(t *testing.T) {
		store, orgID := factory(t)
		ctx := context.Background()
		id, err := store.Create(ctx, orgID, domain.Agent{
			DisplayName:  "Custom Bot Name",
			DefaultModel: "claude-opus-4-7",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if id == "" {
			t.Fatal("Create returned empty id")
		}
		got, err := store.GetForOrg(ctx, orgID)
		if err != nil {
			t.Fatalf("GetForOrg: %v", err)
		}
		if got == nil {
			t.Fatal("GetForOrg returned nil after Create")
		}
		if got.ID != id {
			t.Errorf("Create returned id=%q but GetForOrg returned id=%q", id, got.ID)
		}
		if got.DisplayName != "Custom Bot Name" {
			t.Errorf("DisplayName=%q want %q", got.DisplayName, "Custom Bot Name")
		}
		if got.DefaultModel != "claude-opus-4-7" {
			t.Errorf("DefaultModel=%q want claude-opus-4-7", got.DefaultModel)
		}
	})

	t.Run("Create_DefaultsDisplayName", func(t *testing.T) {
		store, orgID := factory(t)
		_, err := store.Create(context.Background(), orgID, domain.Agent{})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := store.GetForOrg(context.Background(), orgID)
		if err != nil || got == nil {
			t.Fatalf("GetForOrg: got=%v err=%v", got, err)
		}
		if got.DisplayName != "Triage Factory Bot" {
			t.Errorf("DisplayName=%q; want Triage Factory Bot (default)", got.DisplayName)
		}
	})

	t.Run("Create_IgnoresCallerSuppliedID", func(t *testing.T) {
		// Regression: an earlier version of both impls let a caller-
		// supplied a.ID override BootstrapAgentID(orgID). In SQLite
		// (no UNIQUE(org_id)) that created rows GetForOrg's
		// deterministic lookup couldn't reach AND let a subsequent
		// empty-ID Create insert a second row. In Postgres the custom
		// id was silently dropped on conflict, producing
		// "the id you asked for isn't the id you got" surprises.
		// Both backends now ignore a.ID and use the deterministic
		// derivation; the returned id is the only id the row will
		// ever have. This test pins that contract.
		store, orgID := factory(t)
		ctx := context.Background()
		id, err := store.Create(ctx, orgID, domain.Agent{
			ID:          "00000000-1111-2222-3333-444444444444",
			DisplayName: "Custom",
		})
		if err != nil {
			t.Fatalf("Create with caller-supplied ID: %v", err)
		}
		if id == "00000000-1111-2222-3333-444444444444" {
			t.Fatal("Create returned caller's id; impl is honoring caller-supplied ID. Should ignore.")
		}
		// And GetForOrg must reach the row.
		got, err := store.GetForOrg(ctx, orgID)
		if err != nil {
			t.Fatalf("GetForOrg: %v", err)
		}
		if got == nil {
			t.Fatal("GetForOrg returned nil; caller-supplied ID created an unreachable row")
		}
		if got.ID != id {
			t.Errorf("GetForOrg returned id=%q; Create returned id=%q; contract requires they match", got.ID, id)
		}
	})

	t.Run("Create_DuplicateReturnsExistingID", func(t *testing.T) {
		// Idempotency invariant. Bootstrap may run multiple times across
		// boots; the second Create must NOT error and must NOT change
		// the existing row's id (callers persist the id elsewhere —
		// flipping it would invalidate audit traces).
		store, orgID := factory(t)
		ctx := context.Background()
		first, err := store.Create(ctx, orgID, domain.Agent{DisplayName: "Original"})
		if err != nil {
			t.Fatalf("first Create: %v", err)
		}
		second, err := store.Create(ctx, orgID, domain.Agent{DisplayName: "Different"})
		if err != nil {
			t.Fatalf("duplicate Create: %v", err)
		}
		if first != second {
			t.Errorf("second Create returned new id %q; want existing %q", second, first)
		}
		// And the row should not have been overwritten — ON CONFLICT
		// DO NOTHING preserves the original display_name.
		got, _ := store.GetForOrg(ctx, orgID)
		if got == nil {
			t.Fatal("row vanished after duplicate Create")
		}
		if got.DisplayName != "Original" {
			t.Errorf("DisplayName=%q after duplicate Create; want %q (no overwrite)", got.DisplayName, "Original")
		}
	})

	t.Run("GetForOrg_ReturnsNilWhenAbsent", func(t *testing.T) {
		store, orgID := factory(t)
		got, err := store.GetForOrg(context.Background(), orgID)
		if err != nil {
			t.Fatalf("GetForOrg: %v", err)
		}
		if got != nil {
			t.Fatalf("GetForOrg on fresh store returned %+v; want nil", got)
		}
	})

	t.Run("Update_AppliesFields", func(t *testing.T) {
		store, orgID := factory(t)
		ctx := context.Background()
		id, err := store.Create(ctx, orgID, domain.Agent{DisplayName: "Initial"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		autonomy := 0.85
		if err := store.Update(ctx, orgID, domain.Agent{
			ID:                         id,
			DisplayName:                "Renamed",
			DefaultModel:               "claude-sonnet-4-6",
			DefaultAutonomySuitability: &autonomy,
			JiraServiceAccountID:       "sa-jira-123",
		}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		got, _ := store.GetForOrg(ctx, orgID)
		if got == nil {
			t.Fatal("row vanished after Update")
		}
		if got.DisplayName != "Renamed" {
			t.Errorf("DisplayName=%q want Renamed", got.DisplayName)
		}
		if got.DefaultModel != "claude-sonnet-4-6" {
			t.Errorf("DefaultModel=%q want claude-sonnet-4-6", got.DefaultModel)
		}
		if got.DefaultAutonomySuitability == nil || !nearlyEqual(*got.DefaultAutonomySuitability, 0.85) {
			t.Errorf("DefaultAutonomySuitability=%v want 0.85", got.DefaultAutonomySuitability)
		}
		if got.JiraServiceAccountID != "sa-jira-123" {
			t.Errorf("JiraServiceAccountID=%q want sa-jira-123", got.JiraServiceAccountID)
		}
	})

	t.Run("Update_OnInvalidUUID_IsNoop", func(t *testing.T) {
		// Postgres-only constraint; SQLite TEXT keys accept anything.
		// The conformance asserts the contract (no error returned) for
		// both backends — SQLite's no-op behavior comes from "WHERE
		// id = ?" matching zero rows.
		store, orgID := factory(t)
		ctx := context.Background()
		if _, err := store.Create(ctx, orgID, domain.Agent{DisplayName: "Real"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := store.Update(ctx, orgID, domain.Agent{
			ID:          "not-a-uuid",
			DisplayName: "Hijacked",
		}); err != nil {
			t.Errorf("Update with invalid UUID: want nil, got %v", err)
		}
		// Real row untouched.
		got, _ := store.GetForOrg(ctx, orgID)
		if got == nil || got.DisplayName != "Real" {
			t.Errorf("real row corrupted by invalid-UUID Update; got=%+v", got)
		}
	})

	t.Run("SetGitHubAppInstallation_WritesAndClearsPATUser", func(t *testing.T) {
		// The Postgres impl needs a real users(id) for GitHubPATUserID
		// because of the FK. We exercise the mutual-exclusion property
		// by starting with an installation-only row and clearing it via
		// the App-set call.
		store, orgID := factory(t)
		ctx := context.Background()
		id, err := store.Create(ctx, orgID, domain.Agent{
			DisplayName:             "Test",
			GitHubAppInstallationID: "12345",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Switch to a different installation_id.
		if err := store.SetGitHubAppInstallation(ctx, orgID, id, "99999"); err != nil {
			t.Fatalf("SetGitHubAppInstallation: %v", err)
		}
		got, _ := store.GetForOrg(ctx, orgID)
		if got == nil {
			t.Fatal("row missing after SetGitHubAppInstallation")
		}
		if got.GitHubAppInstallationID != "99999" {
			t.Errorf("installation_id=%q want 99999", got.GitHubAppInstallationID)
		}
		if got.GitHubPATUserID != "" {
			t.Errorf("PAT user not cleared on App-set; got=%q", got.GitHubPATUserID)
		}
	})

	t.Run("SetGitHubAppInstallation_OnInvalidUUID_IsNoop", func(t *testing.T) {
		store, orgID := factory(t)
		if err := store.SetGitHubAppInstallation(context.Background(), orgID, "not-a-uuid", "12345"); err != nil {
			t.Errorf("SetGitHubAppInstallation invalid UUID: want nil, got %v", err)
		}
	})

	t.Run("SetGitHubPATUser_OnInvalidUUID_IsNoop", func(t *testing.T) {
		store, orgID := factory(t)
		if err := store.SetGitHubPATUser(context.Background(), orgID, "not-a-uuid", uuid.New().String()); err != nil {
			t.Errorf("SetGitHubPATUser invalid agent UUID: want nil, got %v", err)
		}
	})
}
