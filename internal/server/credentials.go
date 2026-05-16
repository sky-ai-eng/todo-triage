package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

type setupRequest struct {
	GitHubURL string `json:"github_url"`
	GitHubPAT string `json:"github_pat"`
	JiraURL   string `json:"jira_url"`
	JiraPAT   string `json:"jira_pat"`
	// CloneProtocol is the user's choice on the Setup wizard: "ssh"
	// (default) or "https". Empty means "use the existing config
	// value" — important because the wizard runs preflight separately
	// and may post setup multiple times during reconfiguration.
	CloneProtocol string `json:"clone_protocol"`
}

type setupResponse struct {
	GitHub *auth.GitHubUser `json:"github,omitempty"`
	Jira   *auth.JiraUser   `json:"jira,omitempty"`
}

func (s *Server) handleIntegrationsSetup(w http.ResponseWriter, r *http.Request) {
	var req setupRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}

	if req.GitHubURL == "" || req.GitHubPAT == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "GitHub URL and token are required"})
		return
	}

	// Hard-block setup with SSH selected if our preflight against the
	// configured GitHub host can't authenticate. Run BEFORE the PAT
	// check so the user gets the SSH error first rather than entering
	// a valid PAT just to find out their SSH is broken on the next
	// step. The HTTPS path skips this entirely. The probe target is
	// derived from the URL the user just submitted so GHE deployments
	// see hints with their hostname, not "github.com".
	if req.CloneProtocol == "ssh" {
		sshHost := worktree.SSHHostFromBaseURL(req.GitHubURL)
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		err := worktree.PreflightSSH(ctx, sshHost)
		cancel()
		if err != nil {
			log.Printf("[auth] blocked SSH setup against %s: preflight failed: %v", sshHost, err)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error":  fmt.Sprintf("SSH preflight against %s failed — set up your SSH key or pick HTTPS. %s", sshHost, err.Error()),
				"field":  "clone_protocol",
				"stderr": err.Error(),
			})
			return
		}
	}

	resp := setupResponse{}

	// Validate GitHub if provided
	if req.GitHubURL != "" && req.GitHubPAT != "" {
		ghUser, err := auth.ValidateGitHub(req.GitHubURL, req.GitHubPAT)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "GitHub: " + err.Error(),
				"field": "github",
			})
			return
		}
		resp.GitHub = ghUser
	}

	// Validate Jira if provided
	if req.JiraURL != "" && req.JiraPAT != "" {
		jiraUser, err := auth.ValidateJira(req.JiraURL, req.JiraPAT)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "Jira: " + err.Error(),
				"field": "jira",
			})
			return
		}
		resp.Jira = jiraUser
	}

	// Store credentials in keychain (PATs only — github_username lives on
	// the users row per SKY-264, written separately below).
	if err := auth.Store(auth.Credentials{
		GitHubURL: req.GitHubURL,
		GitHubPAT: req.GitHubPAT,
		JiraURL:   req.JiraURL,
		JiraPAT:   req.JiraPAT,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to store credentials: " + err.Error()})
		return
	}

	// Capture the GitHub login on the users row when we validated GitHub.
	// Skip when GitHub wasn't validated (Jira-only setup) — the dashboard /
	// poller short-circuit on empty username and Settings can re-capture
	// later. This write is local-mode-only because it targets the
	// LocalDefaultUserID row; multi-user modes must use a session-derived
	// user ID instead.
	if resp.GitHub != nil && resp.GitHub.Login != "" && runmode.Current() == runmode.ModeLocal {
		if err := s.users.SetGitHubUsername(r.Context(), runmode.LocalDefaultUserID, resp.GitHub.Login); err != nil {
			log.Printf("[setup] failed to persist users.github_username: %v", err)
		}
	}

	// Persist base URLs in config so they survive without keychain access
	cfg, _ := config.Load()
	if req.GitHubURL != "" {
		cfg.GitHub.BaseURL = req.GitHubURL
	}
	if req.JiraURL != "" {
		cfg.Jira.BaseURL = req.JiraURL
	}
	if req.CloneProtocol == "ssh" || req.CloneProtocol == "https" {
		cfg.GitHub.CloneProtocol = req.CloneProtocol
	}
	if err := config.Save(cfg); err != nil {
		log.Printf("[auth] warning: failed to save config: %v", err)
	}

	// Setup always includes GitHub — trigger full restart. Mark Jira restarted
	// synchronously so jiraPollReady flips false before the async callback
	// starts, closing a race where carry-over reads stale snapshots.
	if s.onGitHubChanged != nil {
		s.MarkJiraRestarted()
		go s.onGitHubChanged()
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleIntegrationsStatus(w http.ResponseWriter, r *http.Request) {
	creds, err := auth.Load()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": false,
			"error":      err.Error(),
		})
		return
	}

	repoCount, _ := s.repos.CountConfigured(r.Context(), runmode.LocalDefaultOrgID)

	// GitHub is mandatory — configured requires GitHub creds + at least one repo
	result := map[string]any{
		"configured":   creds.GitHubPAT != "" && creds.GitHubURL != "" && repoCount > 0,
		"github":       creds.GitHubPAT != "",
		"jira":         creds.JiraPAT != "",
		"github_repos": repoCount,
		"env_provided": auth.EnvProvided(),
	}

	if creds.GitHubURL != "" {
		result["github_url"] = creds.GitHubURL
	}
	if creds.JiraURL != "" {
		result["jira_url"] = creds.JiraURL
	}

	writeJSON(w, http.StatusOK, result)
}

// DELETE /api/integrations — clears all integration credentials (GitHub
// + Jira) from the keychain. Used by the Settings "Clear All Tokens"
// flow when the user wants a fresh slate. Granular per-integration
// clears live on subpaths (e.g. DELETE /api/integrations/jira).
func (s *Server) handleIntegrationsClear(w http.ResponseWriter, r *http.Request) {
	if err := auth.Clear(); err != nil {
		internalError(w, "auth", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

// DELETE /api/integrations/jira — clears Jira credentials only,
// preserving GitHub. Counterpart to the collection-level clear at
// DELETE /api/integrations.
func (s *Server) handleIntegrationsDeleteJira(w http.ResponseWriter, r *http.Request) {
	if err := auth.ClearJira(); err != nil {
		internalError(w, "auth", err)
		return
	}
	// Stop the Jira poller and clear the in-memory client so it doesn't
	// keep polling with stale credentials.
	if s.onJiraChanged != nil {
		s.MarkJiraRestarted()
		go s.onJiraChanged()
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}
