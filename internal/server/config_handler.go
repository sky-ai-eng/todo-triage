package server

import (
	"log"
	"net/http"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// configResponse is the FE-facing shape exposed by GET /api/config.
//
// Two consumers, two purposes:
//
//   - AuthGate (SKY-252 D8) reads deployment_mode at boot to choose
//     between the local keychain-capture flow and the multi-mode cookie
//     auth flow. This call is unauthenticated — it has to work before
//     login.
//   - IdentityListField (SKY-264) reads team_size + current_user to pick
//     the predicate editor variant (toggle vs multi-select).
//
// In multi mode the call is still unauthenticated by route, but does a
// soft session peek: if the caller's sid cookie is valid, current_user
// fields are populated from JWT claims. team_size stays at 1 as a
// placeholder until D9 wires org-scoped team rosters — the predicate
// editor will degrade to single-user variant in multi mode, which is
// the conservative default.
//
// Don't conflate with /api/settings (user-mutable preferences) or
// /api/team/members (mutable team roster).
type configResponse struct {
	DeploymentMode string             `json:"deployment_mode"`
	TeamSize       int                `json:"team_size"`
	CurrentUser    configResponseUser `json:"current_user"`
}

type configResponseUser struct {
	ID              string  `json:"id"`
	GitHubUsername  *string `json:"github_username"`   // null when not yet captured
	JiraAccountID   *string `json:"jira_account_id"`   // null when Jira not yet connected
	JiraDisplayName *string `json:"jira_display_name"` // null when Jira not yet connected
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	mode := runmode.Current()

	if mode == runmode.ModeLocal {
		username, _ := s.users.GetGitHubUsername(r.Context(), runmode.LocalDefaultUserID)
		var gh *string
		if username != "" {
			gh = &username
		}
		jiraAccount, jiraName, _ := s.users.GetJiraIdentity(r.Context(), runmode.LocalDefaultUserID)
		var jiraAccountPtr, jiraNamePtr *string
		if jiraAccount != "" {
			jiraAccountPtr = &jiraAccount
		}
		if jiraName != "" {
			jiraNamePtr = &jiraName
		}
		writeJSON(w, http.StatusOK, configResponse{
			DeploymentMode: string(mode),
			TeamSize:       1,
			CurrentUser: configResponseUser{
				ID:              runmode.LocalDefaultUserID,
				GitHubUsername:  gh,
				JiraAccountID:   jiraAccountPtr,
				JiraDisplayName: jiraNamePtr,
			},
		})
		return
	}

	// Multi mode. The handler is unauthenticated by route so AuthGate
	// can read deployment_mode before login. Soft session peek
	// populates current_user when the caller already has a valid sid
	// cookie — failures degrade silently to empty current_user rather
	// than returning 401.
	resp := configResponse{
		DeploymentMode: string(mode),
		TeamSize:       0,
		CurrentUser:    configResponseUser{},
	}
	if user, ok := s.softPeekUser(r); ok {
		resp.CurrentUser = user
		resp.TeamSize = 1
	}
	writeJSON(w, http.StatusOK, resp)
}

// softPeekUser attempts to resolve the current user from the sid cookie
// without surfacing 401s. Returns (user, true) when the cookie is
// present, the session is active, and the JWT verifies; returns
// (_, false) on any failure. Distinct from withSession in that failures
// are silent — /api/config must answer "what mode are we in?" for
// unauthenticated boot.
func (s *Server) softPeekUser(r *http.Request) (configResponseUser, bool) {
	if s.authDeps == nil {
		return configResponseUser{}, false
	}
	cookie, err := r.Cookie(s.sidCookieName())
	if err != nil {
		return configResponseUser{}, false
	}
	sid, err := uuid.Parse(cookie.Value)
	if err != nil {
		return configResponseUser{}, false
	}
	sess, err := s.authDeps.sessions.Lookup(r.Context(), sid)
	if err != nil {
		log.Printf("[config] soft session lookup: %v", err)
		return configResponseUser{}, false
	}
	if sess == nil {
		return configResponseUser{}, false
	}
	claims, err := s.authDeps.verifier.Verify(sess.JWT)
	if err != nil {
		return configResponseUser{}, false
	}
	var gh *string
	if claims.UserMetadata != nil {
		if v, _ := claims.UserMetadata["user_name"].(string); v != "" {
			gh = &v
		} else if v, _ := claims.UserMetadata["preferred_username"].(string); v != "" {
			gh = &v
		}
	}
	return configResponseUser{
		ID:             claims.Subject,
		GitHubUsername: gh,
		// Jira identity in multi mode is per-org and not surfaced
		// here yet; D9 retrofits this once org context lands.
		JiraAccountID:   nil,
		JiraDisplayName: nil,
	}, true
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
	JiraAccountID  *string `json:"jira_account_id"` // null when member hasn't connected Jira
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
	jiraAccount, _, _ := s.users.GetJiraIdentity(r.Context(), runmode.LocalDefaultUserID)
	var login, jiraID *string
	if username != "" {
		login = &username
	}
	if jiraAccount != "" {
		jiraID = &jiraAccount
	}
	writeJSON(w, http.StatusOK, teamMembersResponse{
		Members: []teamMemberRow{
			{
				UserID:         runmode.LocalDefaultUserID,
				DisplayName:    displayName,
				GitHubUsername: login,
				JiraAccountID:  jiraID,
				IsCurrentUser:  true,
			},
		},
	})
}
