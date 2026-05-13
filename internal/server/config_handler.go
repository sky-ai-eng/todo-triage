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
	// Until SKY-251 plumbs the team-scoped session middleware, the only
	// supported runtime is local mode. Refusing in multi mode is safer
	// than returning a response stuffed with local-sentinel values that
	// would silently mislead the SPA into rendering the wrong editor
	// variant.
	if runmode.Current() != runmode.ModeLocal {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "/api/config is not yet wired for multi mode (see SKY-251)",
		})
		return
	}

	username, _ := s.users.GetGitHubUsername(r.Context(), runmode.LocalDefaultUserID)
	var gh *string
	if username != "" {
		gh = &username
	}
	writeJSON(w, http.StatusOK, configResponse{
		DeploymentMode: string(runmode.Current()),
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
	// Multi mode would query memberships for the session user's active
	// team — gated behind SKY-251's middleware which doesn't exist yet.
	// Refuse rather than return a synthetic local roster that would
	// mislead the FE's "you" highlighting.
	if runmode.Current() != runmode.ModeLocal {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "/api/team/members is not yet wired for multi mode (see SKY-251)",
		})
		return
	}

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
