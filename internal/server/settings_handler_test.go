package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/config"
)

// --- Unit tests: validateStatusRule ----------------------------------------
//
// These cover the invariants directly, independent of the HTTP layer:
//   - Pickup (hasWriteTarget=false) MUST have empty canonical.
//   - InProgress/Done (hasWriteTarget=true) MUST have canonical ∈ members
//     whenever members is non-empty.
//   - Empty rules (no members, no canonical) are always OK — "cleared" state.

func TestValidateStatusRule_Pickup_EmptyOK(t *testing.T) {
	if err := validateStatusRule("pickup", config.JiraStatusRule{}, false); err != nil {
		t.Fatalf("empty pickup rule should be valid, got: %v", err)
	}
}

func TestValidateStatusRule_Pickup_MembersOnlyOK(t *testing.T) {
	r := config.JiraStatusRule{Members: []string{"To Do", "Backlog"}}
	if err := validateStatusRule("pickup", r, false); err != nil {
		t.Fatalf("pickup with members only should be valid, got: %v", err)
	}
}

func TestValidateStatusRule_Pickup_CanonicalSet_Rejected(t *testing.T) {
	// Canonical set AT ALL on a read-only rule is invalid — even if it
	// happens to be in members, storing it would let stale config drift
	// through and mislead future readers.
	r := config.JiraStatusRule{Members: []string{"To Do"}, Canonical: "To Do"}
	err := validateStatusRule("pickup", r, false)
	if err == nil {
		t.Fatal("pickup with canonical should be rejected")
	}
	if !strings.Contains(err.Error(), "canonical must be empty") {
		t.Errorf("error message should mention empty canonical, got: %v", err)
	}
}

func TestValidateStatusRule_WriteTarget_EmptyOK(t *testing.T) {
	// Empty rule (no members, no canonical) is a valid "cleared" state for
	// InProgress/Done too — the Ready() gate handles "required for startup";
	// validateStatusRule only enforces the shape invariants.
	if err := validateStatusRule("in_progress", config.JiraStatusRule{}, true); err != nil {
		t.Fatalf("empty in_progress rule should be shape-valid, got: %v", err)
	}
	if err := validateStatusRule("done", config.JiraStatusRule{}, true); err != nil {
		t.Fatalf("empty done rule should be shape-valid, got: %v", err)
	}
}

func TestValidateStatusRule_WriteTarget_Valid(t *testing.T) {
	r := config.JiraStatusRule{
		Members:   []string{"In Progress", "In Review"},
		Canonical: "In Progress",
	}
	if err := validateStatusRule("in_progress", r, true); err != nil {
		t.Fatalf("valid write-target rule should be accepted, got: %v", err)
	}
}

func TestValidateStatusRule_WriteTarget_MembersWithoutCanonical_Rejected(t *testing.T) {
	// If the user listed members but forgot to pick a canonical, we can't
	// actually write — transitions would have nowhere to go. Reject rather
	// than silently degrade at claim time.
	r := config.JiraStatusRule{Members: []string{"In Progress"}}
	err := validateStatusRule("in_progress", r, true)
	if err == nil {
		t.Fatal("in_progress with members but no canonical should be rejected")
	}
	if !strings.Contains(err.Error(), "canonical status is required") {
		t.Errorf("error message should mention canonical required, got: %v", err)
	}
}

func TestValidateStatusRule_WriteTarget_CanonicalNotInMembers_Rejected(t *testing.T) {
	// Canonical must itself be a member — otherwise TF would write a status
	// that doesn't match the user's definition of "in progress," and the
	// next read would immediately flip the ticket back out of the rule.
	r := config.JiraStatusRule{
		Members:   []string{"In Progress"},
		Canonical: "Doing",
	}
	err := validateStatusRule("in_progress", r, true)
	if err == nil {
		t.Fatal("canonical outside members should be rejected")
	}
	if !strings.Contains(err.Error(), "not in members") {
		t.Errorf("error message should mention canonical not in members, got: %v", err)
	}
}

// --- Handler tests: POST /api/settings rejects invalid rules ---------------
//
// These confirm the wire-up — validation errors on any of the three rules
// propagate to a 400 before any persistence fires. Happy-path round-trip
// isn't tested here because it'd write to the real keychain/config.yaml;
// those invariants are covered by the unit tests above.
//
// All of these bodies set *_enabled: true with empty URL/PAT so the handler
// doesn't take the "disabled" branch (which clears credentials via
// auth.ClearGitHub / auth.ClearJira — real keychain writes). Validation
// short-circuits before any persistence on the rejection path.

func settingsPostBody(jiraField string, rule config.JiraStatusRule) map[string]any {
	return map[string]any{
		"github_enabled": true,
		"jira_enabled":   true,
		jiraField:        rule,
	}
}

func TestSettingsPost_PickupCanonical_Rejected(t *testing.T) {
	s := newTestServer(t)
	body := settingsPostBody("jira_pickup", config.JiraStatusRule{
		Members:   []string{"To Do"},
		Canonical: "To Do", // invalid: pickup has no write target
	})
	rec := doJSON(t, s, "POST", "/api/settings", body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp["error"], "canonical must be empty") {
		t.Errorf("error should mention pickup canonical invariant, got: %q", resp["error"])
	}
}

func TestSettingsPost_InProgressCanonicalNotInMembers_Rejected(t *testing.T) {
	s := newTestServer(t)
	body := settingsPostBody("jira_in_progress", config.JiraStatusRule{
		Members:   []string{"In Progress"},
		Canonical: "Doing", // invalid: not a member
	})
	rec := doJSON(t, s, "POST", "/api/settings", body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp["error"], "not in members") {
		t.Errorf("error should mention canonical not in members, got: %q", resp["error"])
	}
}

func TestSettingsPost_InProgressMembersWithoutCanonical_Rejected(t *testing.T) {
	s := newTestServer(t)
	body := settingsPostBody("jira_in_progress", config.JiraStatusRule{
		Members: []string{"In Progress"}, // invalid: missing canonical
	})
	rec := doJSON(t, s, "POST", "/api/settings", body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp["error"], "canonical status is required") {
		t.Errorf("error should mention canonical required, got: %q", resp["error"])
	}
}

func TestSettingsPost_DoneCanonicalNotInMembers_Rejected(t *testing.T) {
	s := newTestServer(t)
	body := settingsPostBody("jira_done", config.JiraStatusRule{
		Members:   []string{"Resolved", "Verified"},
		Canonical: "Done", // invalid: not a member
	})
	rec := doJSON(t, s, "POST", "/api/settings", body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp["error"], "not in members") {
		t.Errorf("error should mention canonical not in members, got: %q", resp["error"])
	}
}
