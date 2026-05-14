package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/config"
)

// --- Unit tests: validateProjectRules --------------------------------------
//
// Every persisted project must be fully configured. Partial saves aren't a
// supported state — the FE prevents them, this handler rejects them, and the
// jpsr_*_populated CHECK constraints catch any path that slips past both.
// These unit tests cover the per-project invariant directly:
//   - Pickup: members required, canonical must be empty (TF never writes).
//   - InProgress/Done: members + canonical required, canonical ∈ members.

func validProject(key string) config.JiraProjectConfig {
	return config.JiraProjectConfig{
		Key:        key,
		Pickup:     config.JiraStatusRule{Members: []string{"To Do"}},
		InProgress: config.JiraStatusRule{Members: []string{"In Progress"}, Canonical: "In Progress"},
		Done:       config.JiraStatusRule{Members: []string{"Done"}, Canonical: "Done"},
	}
}

func TestValidateProjectRules_Valid(t *testing.T) {
	if err := validateProjectRules(validProject("SKY")); err != nil {
		t.Fatalf("fully-configured project should be valid, got: %v", err)
	}
}

func TestValidateProjectRules_PickupEmptyMembers_Rejected(t *testing.T) {
	p := validProject("SKY")
	p.Pickup.Members = nil
	err := validateProjectRules(p)
	if err == nil || !strings.Contains(err.Error(), "pickup members are required") {
		t.Errorf("empty pickup members should be rejected, got: %v", err)
	}
}

func TestValidateProjectRules_PickupCanonicalSet_Rejected(t *testing.T) {
	p := validProject("SKY")
	p.Pickup.Canonical = "To Do"
	err := validateProjectRules(p)
	if err == nil || !strings.Contains(err.Error(), "pickup canonical must be empty") {
		t.Errorf("pickup canonical should be rejected, got: %v", err)
	}
}

func TestValidateProjectRules_InProgressEmptyMembers_Rejected(t *testing.T) {
	p := validProject("SKY")
	p.InProgress.Members = nil
	p.InProgress.Canonical = ""
	err := validateProjectRules(p)
	if err == nil || !strings.Contains(err.Error(), "in_progress members are required") {
		t.Errorf("empty in_progress members should be rejected, got: %v", err)
	}
}

func TestValidateProjectRules_InProgressMissingCanonical_Rejected(t *testing.T) {
	p := validProject("SKY")
	p.InProgress.Canonical = ""
	err := validateProjectRules(p)
	if err == nil || !strings.Contains(err.Error(), "in_progress canonical is required") {
		t.Errorf("missing in_progress canonical should be rejected, got: %v", err)
	}
}

func TestValidateProjectRules_InProgressCanonicalNotInMembers_Rejected(t *testing.T) {
	p := validProject("SKY")
	p.InProgress.Canonical = "Doing" // not in Members
	err := validateProjectRules(p)
	if err == nil || !strings.Contains(err.Error(), "not in members") {
		t.Errorf("canonical-not-in-members should be rejected, got: %v", err)
	}
}

func TestValidateProjectRules_DoneEmptyMembers_Rejected(t *testing.T) {
	p := validProject("SKY")
	p.Done.Members = nil
	p.Done.Canonical = ""
	err := validateProjectRules(p)
	if err == nil || !strings.Contains(err.Error(), "done members are required") {
		t.Errorf("empty done members should be rejected, got: %v", err)
	}
}

// --- Unit tests: project key normalization + regex -------------------------

func TestNormalizeJiraProjectKey(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"sky", "SKY"},
		{"  SKY  ", "SKY"},
		{"Mixed_Case", "MIXED_CASE"},
		{"", ""},
	} {
		if got := normalizeJiraProjectKey(tc.in); got != tc.want {
			t.Errorf("normalizeJiraProjectKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestJiraProjectKeyRe(t *testing.T) {
	valid := []string{"SKY", "SKY1", "A", "PROJ_2024", "X_Y_Z"}
	invalid := []string{"", "1SKY", "_SKY", "sky", "SKY-X", "SKY.X", "SK Y"}
	for _, k := range valid {
		if !jiraProjectKeyRe.MatchString(k) {
			t.Errorf("expected %q to match jiraProjectKeyRe", k)
		}
	}
	for _, k := range invalid {
		if jiraProjectKeyRe.MatchString(k) {
			t.Errorf("expected %q to NOT match jiraProjectKeyRe", k)
		}
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

// settingsPostBodyWithProject builds a request that exercises validation
// of a single project's rules. The SKY-272 wire shape collapses Pickup,
// InProgress, and Done into the per-project array.
func settingsPostBodyWithProject(key string, pickup, inProgress, done config.JiraStatusRule) map[string]any {
	return map[string]any{
		"github_enabled": true,
		"jira_enabled":   true,
		"jira_projects": []map[string]any{
			{
				"key":         key,
				"pickup":      pickup,
				"in_progress": inProgress,
				"done":        done,
			},
		},
	}
}

func validInProgress() config.JiraStatusRule {
	return config.JiraStatusRule{Members: []string{"In Progress"}, Canonical: "In Progress"}
}

func validDone() config.JiraStatusRule {
	return config.JiraStatusRule{Members: []string{"Done"}, Canonical: "Done"}
}

func validPickup() config.JiraStatusRule {
	return config.JiraStatusRule{Members: []string{"To Do"}}
}

func TestSettingsPost_PickupCanonical_Rejected(t *testing.T) {
	s := newTestServer(t)
	body := settingsPostBodyWithProject("SKY",
		config.JiraStatusRule{Members: []string{"To Do"}, Canonical: "To Do"}, // invalid pickup
		validInProgress(),
		validDone(),
	)
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
	body := settingsPostBodyWithProject("SKY",
		validPickup(),
		config.JiraStatusRule{Members: []string{"In Progress"}, Canonical: "Doing"}, // invalid
		validDone(),
	)
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
	body := settingsPostBodyWithProject("SKY",
		validPickup(),
		config.JiraStatusRule{Members: []string{"In Progress"}}, // invalid: missing canonical
		validDone(),
	)
	rec := doJSON(t, s, "POST", "/api/settings", body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp["error"], "canonical is required") {
		t.Errorf("error should mention canonical required, got: %q", resp["error"])
	}
}

func TestSettingsPost_DoneCanonicalNotInMembers_Rejected(t *testing.T) {
	s := newTestServer(t)
	body := settingsPostBodyWithProject("SKY",
		validPickup(),
		validInProgress(),
		config.JiraStatusRule{Members: []string{"Resolved", "Verified"}, Canonical: "Done"}, // invalid
	)
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

// TestSettingsPost_PerProjectRules_RoundTrip verifies the core SKY-272
// contract: two projects in the same team can carry different rules,
// and Save → Load preserves each project's rules independently. Exercises
// the config layer directly (the HTTP handler's keychain write isn't
// available in the test env).
func TestSettingsPost_PerProjectRules_RoundTrip(t *testing.T) {
	_ = newTestServer(t) // sets up config.Init against an in-memory DB
	cfg := config.Default()
	cfg.Jira.Projects = []config.JiraProjectConfig{
		{
			Key:        "SKY",
			Pickup:     config.JiraStatusRule{Members: []string{"Backlog", "Selected"}},
			InProgress: config.JiraStatusRule{Members: []string{"In Progress"}, Canonical: "In Progress"},
			Done:       config.JiraStatusRule{Members: []string{"Done"}, Canonical: "Done"},
		},
		{
			Key:        "OPS",
			Pickup:     config.JiraStatusRule{Members: []string{"New", "Triage"}},
			InProgress: config.JiraStatusRule{Members: []string{"Active"}, Canonical: "Active"},
			Done:       config.JiraStatusRule{Members: []string{"Resolved", "Verified"}, Canonical: "Resolved"},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
	got, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if r := got.Jira.RuleForProject("SKY"); r == nil || r.InProgress.Canonical != "In Progress" || !r.Pickup.Contains("Backlog") {
		t.Errorf("SKY rules round-trip: %+v", r)
	}
	if r := got.Jira.RuleForProject("OPS"); r == nil || r.Done.Canonical != "Resolved" || !r.Pickup.Contains("Triage") {
		t.Errorf("OPS rules round-trip: %+v", r)
	}

	// Edit only SKY's rules; OPS must stay untouched.
	cfg = got
	for i, p := range cfg.Jira.Projects {
		if p.Key == "SKY" {
			cfg.Jira.Projects[i].Pickup = config.JiraStatusRule{Members: []string{"Ready"}}
		}
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("config.Save (edit SKY): %v", err)
	}
	got, err = config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if r := got.Jira.RuleForProject("SKY"); r == nil || !r.Pickup.Contains("Ready") || r.Pickup.Contains("Backlog") {
		t.Errorf("SKY edit didn't apply: %+v", r)
	}
	if r := got.Jira.RuleForProject("OPS"); r == nil || !r.Pickup.Contains("Triage") || r.Done.Canonical != "Resolved" {
		t.Errorf("OPS untouched check failed: %+v", r)
	}

	// Drop SKY from the config — the rules row for SKY should vanish
	// while OPS persists.
	kept := make([]config.JiraProjectConfig, 0, len(cfg.Jira.Projects))
	for _, p := range cfg.Jira.Projects {
		if p.Key != "SKY" {
			kept = append(kept, p)
		}
	}
	cfg.Jira.Projects = kept
	if err := config.Save(cfg); err != nil {
		t.Fatalf("config.Save (drop SKY): %v", err)
	}
	got, err = config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if r := got.Jira.RuleForProject("SKY"); r != nil {
		t.Errorf("SKY rules should be gone after drop, got: %+v", r)
	}
	if r := got.Jira.RuleForProject("OPS"); r == nil || r.Done.Canonical != "Resolved" {
		t.Errorf("OPS rules should persist after dropping SKY: %+v", r)
	}
}

// TestSettingsPost_DuplicateProjectKey_Rejected verifies that the
// handler rejects two entries with the same key — the rules table
// keys on (team_id, project_key) and a duplicate would silently
// last-write-win.
func TestSettingsPost_DuplicateProjectKey_Rejected(t *testing.T) {
	s := newTestServer(t)
	body := map[string]any{
		"github_enabled": true,
		"jira_enabled":   true,
		"jira_projects": []map[string]any{
			{"key": "SKY", "pickup": validPickup(), "in_progress": validInProgress(), "done": validDone()},
			{"key": "SKY", "pickup": validPickup(), "in_progress": validInProgress(), "done": validDone()},
		},
	}
	rec := doJSON(t, s, "POST", "/api/settings", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on duplicate project key, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp["error"], "duplicate project key") {
		t.Errorf("error should mention duplicate, got: %q", resp["error"])
	}
}
