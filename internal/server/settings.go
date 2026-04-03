package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/sky-ai-eng/todo-tinder/internal/auth"
	"github.com/sky-ai-eng/todo-tinder/internal/config"
)

// settingsResponse combines config values with auth status so the frontend
// can render everything on one page.
type settingsResponse struct {
	GitHub   githubSettings `json:"github"`
	Jira     jiraSettings   `json:"jira"`
	Server   serverSettings `json:"server"`
	AI       aiSettings     `json:"ai"`
}

type githubSettings struct {
	Enabled      bool   `json:"enabled"`
	BaseURL      string `json:"base_url"`
	HasToken     bool   `json:"has_token"`
	PollInterval string `json:"poll_interval"`
}

type jiraSettings struct {
	Enabled      bool     `json:"enabled"`
	BaseURL      string   `json:"base_url"`
	HasToken     bool     `json:"has_token"`
	PollInterval string   `json:"poll_interval"`
	Projects     []string `json:"projects"`
}

type serverSettings struct {
	Port int `json:"port"`
}

type aiSettings struct {
	Model                    string `json:"model"`
	ReprioritizeThreshold    int    `json:"reprioritize_threshold"`
	PreferenceUpdateInterval int    `json:"preference_update_interval"`
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load config: " + err.Error()})
		return
	}
	creds, _ := auth.Load() // auth errors are non-fatal (keychain may be empty)

	resp := settingsResponse{
		GitHub: githubSettings{
			Enabled:      creds.GitHubPAT != "",
			BaseURL:      cfg.GitHub.BaseURL,
			HasToken:     creds.GitHubPAT != "",
			PollInterval: cfg.GitHub.PollInterval.String(),
		},
		Jira: jiraSettings{
			Enabled:      creds.JiraPAT != "",
			BaseURL:      cfg.Jira.BaseURL,
			HasToken:     creds.JiraPAT != "",
			PollInterval: cfg.Jira.PollInterval.String(),
			Projects:     cfg.Jira.Projects,
		},
		Server: serverSettings{
			Port: cfg.Server.Port,
		},
		AI: aiSettings{
			Model:                    cfg.AI.Model,
			ReprioritizeThreshold:    cfg.AI.ReprioritizeThreshold,
			PreferenceUpdateInterval: cfg.AI.PreferenceUpdateInterval,
		},
	}

	if resp.Jira.Projects == nil {
		resp.Jira.Projects = []string{}
	}

	writeJSON(w, http.StatusOK, resp)
}

type settingsUpdateRequest struct {
	// Connections — only validate/store if token is non-empty
	GitHubEnabled bool   `json:"github_enabled"`
	GitHubURL     string `json:"github_url"`
	GitHubPAT     string `json:"github_pat"` // empty means "keep existing"
	JiraEnabled   bool   `json:"jira_enabled"`
	JiraURL       string `json:"jira_url"`
	JiraPAT       string `json:"jira_pat"` // empty means "keep existing"

	// Config
	GitHubPollInterval string   `json:"github_poll_interval"`
	JiraPollInterval   string   `json:"jira_poll_interval"`
	JiraProjects       []string `json:"jira_projects"`
	AIModel            string   `json:"ai_model"`
	ServerPort         int      `json:"server_port"`
}

func (s *Server) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	var req settingsUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	// Load existing state
	cfg, err := config.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load config: " + err.Error()})
		return
	}
	creds, _ := auth.Load() // auth errors are non-fatal

	// --- Handle GitHub ---
	if req.GitHubEnabled {
		if req.GitHubURL != "" {
			cfg.GitHub.BaseURL = req.GitHubURL
			creds.GitHubURL = req.GitHubURL
		}
		// New token provided — validate it
		if req.GitHubPAT != "" {
			url := req.GitHubURL
			if url == "" {
				url = creds.GitHubURL
			}
			if url == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "GitHub URL is required"})
				return
			}
			_, err := auth.ValidateGitHub(url, req.GitHubPAT)
			if err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
					"error": "GitHub: " + err.Error(),
					"field": "github",
				})
				return
			}
			creds.GitHubPAT = req.GitHubPAT
		}
	} else {
		// Disabled — clear GitHub credentials
		creds.GitHubURL = ""
		creds.GitHubPAT = ""
		cfg.GitHub.BaseURL = ""
	}

	// --- Handle Jira ---
	if req.JiraEnabled {
		if req.JiraURL != "" {
			cfg.Jira.BaseURL = req.JiraURL
			creds.JiraURL = req.JiraURL
		}
		if req.JiraPAT != "" {
			url := req.JiraURL
			if url == "" {
				url = creds.JiraURL
			}
			if url == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Jira URL is required"})
				return
			}
			_, err := auth.ValidateJira(url, req.JiraPAT)
			if err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
					"error": "Jira: " + err.Error(),
					"field": "jira",
				})
				return
			}
			creds.JiraPAT = req.JiraPAT
		}
	} else {
		creds.JiraURL = ""
		creds.JiraPAT = ""
		cfg.Jira.BaseURL = ""
	}

	// --- Update config fields ---
	if req.GitHubPollInterval != "" {
		if d, err := time.ParseDuration(req.GitHubPollInterval); err == nil && d >= 10*time.Second {
			cfg.GitHub.PollInterval = d
		}
	}
	if req.JiraPollInterval != "" {
		if d, err := time.ParseDuration(req.JiraPollInterval); err == nil && d >= 10*time.Second {
			cfg.Jira.PollInterval = d
		}
	}
	if req.JiraProjects != nil {
		cfg.Jira.Projects = req.JiraProjects
	}
	if req.AIModel != "" {
		cfg.AI.Model = req.AIModel
	}
	if req.ServerPort > 0 {
		cfg.Server.Port = req.ServerPort
	}

	// Persist
	if err := auth.Store(creds); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to store credentials: " + err.Error()})
		return
	}
	if err := config.Save(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save config: " + err.Error()})
		return
	}

	// Restart pollers and spawner with new credentials/config
	if s.onCredentialsChanged != nil {
		go s.onCredentialsChanged()
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}
