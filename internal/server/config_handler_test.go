package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestHandleConfig_LocalDefaults reports deployment_mode=local, team_size=1,
// and the synthetic local user.
func TestHandleConfig_LocalDefaults(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodGet, "/api/config", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp configResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.DeploymentMode != string(runmode.ModeLocal) {
		t.Errorf("deployment_mode: got %q want %q", resp.DeploymentMode, runmode.ModeLocal)
	}
	if resp.TeamSize != 1 {
		t.Errorf("team_size: got %d want 1", resp.TeamSize)
	}
	if resp.CurrentUser.ID != runmode.LocalDefaultUserID {
		t.Errorf("current_user.id: got %q want %q", resp.CurrentUser.ID, runmode.LocalDefaultUserID)
	}
	if resp.CurrentUser.GitHubUsername != nil {
		t.Errorf("current_user.github_username: expected null (no identity captured), got %q", *resp.CurrentUser.GitHubUsername)
	}
}

// TestHandleConfig_GitHubUsernamePopulated returns the username after
// SetLocalUserGitHubUsername populates the users row — exercises the
// path the SPA hits after a user saves a PAT.
func TestHandleConfig_GitHubUsernamePopulated(t *testing.T) {
	s := newTestServer(t)
	if err := s.users.SetGitHubUsername(t.Context(), runmode.LocalDefaultUserID, "AidanAllchin"); err != nil {
		t.Fatalf("seed github_username: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/config", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp configResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.CurrentUser.GitHubUsername == nil {
		t.Fatal("expected current_user.github_username populated, got null")
	}
	if *resp.CurrentUser.GitHubUsername != "AidanAllchin" {
		t.Errorf("github_username: got %q want %q", *resp.CurrentUser.GitHubUsername, "AidanAllchin")
	}
}

// TestHandleTeamMembers_LocalSingleEntry returns one member (the synthetic
// local user) with is_current_user=true.
func TestHandleTeamMembers_LocalSingleEntry(t *testing.T) {
	s := newTestServer(t)
	if err := s.users.SetGitHubUsername(t.Context(), runmode.LocalDefaultUserID, "AidanAllchin"); err != nil {
		t.Fatalf("seed github_username: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/team/members", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp teamMembersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Members) != 1 {
		t.Fatalf("expected 1 member, got %d", len(resp.Members))
	}
	m := resp.Members[0]
	if m.UserID != runmode.LocalDefaultUserID {
		t.Errorf("member.user_id: got %q want %q", m.UserID, runmode.LocalDefaultUserID)
	}
	if !m.IsCurrentUser {
		t.Error("local user should be marked is_current_user")
	}
	if m.GitHubUsername == nil || *m.GitHubUsername != "AidanAllchin" {
		t.Errorf("github_username: got %v want AidanAllchin", m.GitHubUsername)
	}
}

// TestHandleTeamMembers_NoIdentityCaptured handles the pre-GitHub-config
// state: the row exists, github_username is NULL, the endpoint still
// returns successfully so the FE can render the editor (just with the
// Variant-A toggle disabled).
func TestHandleTeamMembers_NoIdentityCaptured(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodGet, "/api/team/members", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp teamMembersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Members) != 1 {
		t.Fatalf("expected 1 member, got %d", len(resp.Members))
	}
	if resp.Members[0].GitHubUsername != nil {
		t.Errorf("expected null github_username (not yet captured), got %q", *resp.Members[0].GitHubUsername)
	}
}
