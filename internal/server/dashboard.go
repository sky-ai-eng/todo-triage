package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/todo-tinder/internal/auth"
	"github.com/sky-ai-eng/todo-tinder/internal/config"
	ghclient "github.com/sky-ai-eng/todo-tinder/internal/github"
)

func (s *Server) handleDashboardStats(w http.ResponseWriter, r *http.Request) {
	creds, err := auth.Load()
	if err != nil || creds.GitHubPAT == "" {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	cfg, _ := config.Load()
	baseURL := cfg.GitHub.BaseURL
	if baseURL == "" {
		baseURL = creds.GitHubURL
	}

	ghUser, err := auth.ValidateGitHub(baseURL, creds.GitHubPAT)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	client := ghclient.NewClient(baseURL, creds.GitHubPAT)
	stats, err := client.GetDashboardStats(ghUser.Login)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleDashboardPRs(w http.ResponseWriter, r *http.Request) {
	creds, err := auth.Load()
	if err != nil || creds.GitHubPAT == "" {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	cfg, _ := config.Load()
	baseURL := cfg.GitHub.BaseURL
	if baseURL == "" {
		baseURL = creds.GitHubURL
	}

	// Validate to get username
	ghUser, err := auth.ValidateGitHub(baseURL, creds.GitHubPAT)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "GitHub auth failed: " + err.Error()})
		return
	}

	client := ghclient.NewClient(baseURL, creds.GitHubPAT)
	prs, err := client.SearchUserPRs(ghUser.Login)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, prs)
}

func (s *Server) handleDashboardPRStatus(w http.ResponseWriter, r *http.Request) {
	numberStr := r.PathValue("number")
	number, err := strconv.Atoi(numberStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid PR number"})
		return
	}

	repoParam := r.URL.Query().Get("repo")
	if repoParam == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo query parameter required (owner/repo)"})
		return
	}
	parts := strings.SplitN(repoParam, "/", 2)
	if len(parts) != 2 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo must be owner/repo format"})
		return
	}

	creds, err := auth.Load()
	if err != nil || creds.GitHubPAT == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub not configured"})
		return
	}
	cfg, _ := config.Load()
	baseURL := cfg.GitHub.BaseURL
	if baseURL == "" {
		baseURL = creds.GitHubURL
	}

	client := ghclient.NewClient(baseURL, creds.GitHubPAT)
	status, err := client.GetPRStatus(parts[0], parts[1], number)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleDashboardPRDraft(w http.ResponseWriter, r *http.Request) {
	numberStr := r.PathValue("number")
	number, err := strconv.Atoi(numberStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid PR number"})
		return
	}

	repoParam := r.URL.Query().Get("repo")
	parts := strings.SplitN(repoParam, "/", 2)
	if len(parts) != 2 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo must be owner/repo"})
		return
	}

	var body struct {
		Draft bool `json:"draft"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}

	creds, _ := auth.Load()
	cfg, _ := config.Load()
	baseURL := cfg.GitHub.BaseURL
	if baseURL == "" {
		baseURL = creds.GitHubURL
	}
	client := ghclient.NewClient(baseURL, creds.GitHubPAT)

	if body.Draft {
		err = client.ConvertPRToDraft(parts[0], parts[1], number)
	} else {
		err = client.MarkPRReady(parts[0], parts[1], number)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"draft": body.Draft})
}
