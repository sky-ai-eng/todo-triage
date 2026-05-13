package server

import (
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// configResponse is the FE-facing shape exposed by GET /api/config. It
// tells the SPA which form variants to render in the rule/trigger editor
// (toggle vs multi-select handle picker) and what the current user's
// captured GitHub identity is.
//
// One-shot read at FE boot — the deployment shape doesn't change during
// a session. Don't conflate with /api/settings (user-mutable
// preferences) or /api/team/members (mutable team roster).
type configResponse struct {
	DeploymentMode string             `json:"deployment_mode"`
	TeamSize       int                `json:"team_size"`
	CurrentUser    configResponseUser `json:"current_user"`
}

type configResponseUser struct {
	ID             string  `json:"id"`
	GitHubUsername *string `json:"github_username"` // null when not yet captured
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	mode := string(runmode.Current())

	// Local mode: synthetic single-user team. Team size is always 1.
	// Multi mode will compute team size from memberships once the team-
	// scoped session middleware lands (SKY-251). For now multi-mode
	// callers never reach this code path (main.go fatal's on TF_MODE=multi
	// because the rest of the multi wiring isn't in place yet).
	username, _ := s.users.GetGitHubUsername(r.Context(), runmode.LocalDefaultUserID)
	var gh *string
	if username != "" {
		gh = &username
	}
	writeJSON(w, http.StatusOK, configResponse{
		DeploymentMode: mode,
		TeamSize:       1,
		CurrentUser: configResponseUser{
			ID:             runmode.LocalDefaultUserID,
			GitHubUsername: gh,
		},
	})
}

// teamMembersResponse is the roster shown to Variant B (multi-person team)
// of the predicate editor. Each row carries display_name + github_username
// + is_current_user so the FE can pre-rank the dropdown and highlight
// "you" among teammates.
type teamMembersResponse struct {
	Members []teamMemberRow `json:"members"`
}

type teamMemberRow struct {
	UserID         string  `json:"user_id"`
	DisplayName    string  `json:"display_name"`
	GitHubUsername *string `json:"github_username"` // null when member hasn't captured identity
	IsCurrentUser  bool    `json:"is_current_user"`
}

func (s *Server) handleTeamMembers(w http.ResponseWriter, r *http.Request) {
	// Local mode: single-entry array containing the synthetic user.
	// Multi mode would query memberships for the session user's active
	// team — gated behind SKY-251's middleware which doesn't exist yet.
	username, _ := s.users.GetGitHubUsername(r.Context(), runmode.LocalDefaultUserID)
	displayName, _ := s.users.GetDisplayName(r.Context(), runmode.LocalDefaultUserID)
	var login *string
	if username != "" {
		login = &username
	}
	writeJSON(w, http.StatusOK, teamMembersResponse{
		Members: []teamMemberRow{
			{
				UserID:         runmode.LocalDefaultUserID,
				DisplayName:    displayName,
				GitHubUsername: login,
				IsCurrentUser:  true,
			},
		},
	})
}
