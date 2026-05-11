package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/db"
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

func (s *Server) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	var req setupRequest
	limitBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
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

	// Store credentials in keychain (include username if we validated GitHub)
	ghUsername := ""
	if resp.GitHub != nil {
		ghUsername = resp.GitHub.Login
	}
	if err := auth.Store(auth.Credentials{
		GitHubURL:      req.GitHubURL,
		GitHubPAT:      req.GitHubPAT,
		GitHubUsername: ghUsername,
		JiraURL:        req.JiraURL,
		JiraPAT:        req.JiraPAT,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to store credentials: " + err.Error()})
		return
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

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	creds, err := auth.Load()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": false,
			"error":      err.Error(),
		})
		return
	}

	repoCount, _ := db.CountConfiguredRepos(s.db)

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

func (s *Server) handleAuthDelete(w http.ResponseWriter, r *http.Request) {
	if err := auth.Clear(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

// DELETE /api/auth/jira — clears Jira credentials only, preserving GitHub.
func (s *Server) handleAuthDeleteJira(w http.ResponseWriter, r *http.Request) {
	if err := auth.ClearJira(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
