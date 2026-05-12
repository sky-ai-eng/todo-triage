package postgres_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
)

// seedPgMember inserts a fresh users + org_memberships row pair for
// org-policy tests that need a distinct caller identity. role is one
// of 'member' / 'admin' / 'owner'. Returns the user id. Lifted out of
// the per-store test files in SKY-259 (the predecessor task_rules /
// prompt_triggers test files defined this helper inline) so it's
// available to every postgres-package test.
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

// mustOwnerUserForOrg returns the founder/owner user_id stamped on an
// org row at bootstrap. Tests that need a valid claims principal but
// don't want to fabricate a member use this — the owner always
// exists and passes every policy that checks org access.
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
