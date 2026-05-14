package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// jiraProjectKeyRe matches Jira's own project-key rule: a leading
// uppercase letter followed by uppercase letters, digits, or
// underscores. Keys arriving through the API are uppercased before
// matching so users typing "sky" land on the same canonical form as
// Jira's wire-side "SKY-123".
var jiraProjectKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// normalizeJiraProjectKey trims whitespace and uppercases. Used at
// the HTTP boundary in handleSettingsPost (the write path) and in
// validateTrackerKeys (the read/compare path) so lookups match
// regardless of how the user typed the key.
func normalizeJiraProjectKey(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// validateProjectRules enforces the per-project invariant that every
// persisted project carries fully-populated Pickup/InProgress/Done
// rules. The jpsr_*_populated CHECK constraints in the baseline are
// the DB-level mirror; this is the user-facing gate that surfaces a
// readable error instead of a constraint violation.
//
// Pickup: members required, canonical must be empty (TF never writes
// to pickup). InProgress/Done: members + canonical required, and the
// canonical must itself be a member (PG CHECK can't subquery, so this
// check stays in Go).
func validateProjectRules(p config.JiraProjectConfig) error {
	if len(p.Pickup.Members) == 0 {
		return fmt.Errorf("project %s: pickup members are required", p.Key)
	}
	if p.Pickup.Canonical != "" {
		return fmt.Errorf("project %s: pickup canonical must be empty — TF never writes tickets back to pickup", p.Key)
	}
	for _, r := range []struct {
		name string
		rule config.JiraStatusRule
	}{
		{"in_progress", p.InProgress},
		{"done", p.Done},
	} {
		if len(r.rule.Members) == 0 {
			return fmt.Errorf("project %s: %s members are required", p.Key, r.name)
		}
		if r.rule.Canonical == "" {
			return fmt.Errorf("project %s: %s canonical is required", p.Key, r.name)
		}
		if !slices.Contains(r.rule.Members, r.rule.Canonical) {
			return fmt.Errorf("project %s: %s canonical %q is not in members", p.Key, r.name, r.rule.Canonical)
		}
	}
	return nil
}

// normalizeMembers returns a sorted, deduplicated copy of members so rules can
// be compared using set semantics without mutating the original slice.
func normalizeMembers(members []string) []string {
	normalized := slices.Clone(members)
	slices.Sort(normalized)
	return slices.Compact(normalized)
}

// ruleEqual compares two status rules by value. Used by change detection to
// decide whether a Jira poller restart is needed. Nil-safe on the Members slice.
func ruleEqual(a, b config.JiraStatusRule) bool {
	return a.Canonical == b.Canonical &&
		slices.Equal(normalizeMembers(a.Members), normalizeMembers(b.Members))
}

// cloneJiraProjects returns a deep copy so the pre-change snapshot
// stays stable while the handler mutates cfg.Jira.Projects. The
// per-project Members slices share backing arrays with the originals
// otherwise, which silently makes the "did this change?" diff a
// no-op against itself.
func cloneJiraProjects(in []config.JiraProjectConfig) []config.JiraProjectConfig {
	out := make([]config.JiraProjectConfig, len(in))
	for i, p := range in {
		out[i] = config.JiraProjectConfig{
			Key: p.Key,
			Pickup: config.JiraStatusRule{
				Members:   slices.Clone(p.Pickup.Members),
				Canonical: p.Pickup.Canonical,
			},
			InProgress: config.JiraStatusRule{
				Members:   slices.Clone(p.InProgress.Members),
				Canonical: p.InProgress.Canonical,
			},
			Done: config.JiraStatusRule{
				Members:   slices.Clone(p.Done.Members),
				Canonical: p.Done.Canonical,
			},
		}
	}
	return out
}

// jiraProjectsEqual compares two per-project lists by value, treating
// order as significant (the user-facing UI keeps projects in the order
// they were added; reordering counts as a change worth restarting the
// poller for). Rules are compared with set-equality on Members.
func jiraProjectsEqual(a, b []config.JiraProjectConfig) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key {
			return false
		}
		if !ruleEqual(a[i].Pickup, b[i].Pickup) ||
			!ruleEqual(a[i].InProgress, b[i].InProgress) ||
			!ruleEqual(a[i].Done, b[i].Done) {
			return false
		}
	}
	return true
}

// defaultedCloneProtocol normalizes a stored CloneProtocol value for the
// API surface using the same effective semantics as backend clone URL
// selection: only the literal value "ssh" selects SSH; empty, "https",
// and any other invalid/stale value are treated as HTTPS. Clients should
// always see one of the two known forms.
func defaultedCloneProtocol(stored string) string {
	if stored == "ssh" {
		return "ssh"
	}
	return "https"
}

// settingsResponse combines config values with auth status so the frontend
// can render everything on one page.
type settingsResponse struct {
	GitHub githubSettings `json:"github"`
	Jira   jiraSettings   `json:"jira"`
	Server serverSettings `json:"server"`
	AI     aiSettings     `json:"ai"`
}

type githubSettings struct {
	Enabled       bool   `json:"enabled"`
	BaseURL       string `json:"base_url"`
	HasToken      bool   `json:"has_token"`
	PollInterval  string `json:"poll_interval"`
	CloneProtocol string `json:"clone_protocol"` // "ssh" | "https"
}

type jiraSettings struct {
	Enabled      bool                  `json:"enabled"`
	BaseURL      string                `json:"base_url"`
	HasToken     bool                  `json:"has_token"`
	PollInterval string                `json:"poll_interval"`
	Projects     []jiraProjectSettings `json:"projects"`
}

// jiraProjectSettings is the per-project wire shape. Mirrors
// config.JiraProjectConfig but with explicit empty-slice
// initialization so the JSON response always carries members:[] rather
// than members:null.
type jiraProjectSettings struct {
	Key        string                `json:"key"`
	Pickup     config.JiraStatusRule `json:"pickup"`
	InProgress config.JiraStatusRule `json:"in_progress"`
	Done       config.JiraStatusRule `json:"done"`
}

type serverSettings struct {
	Port int `json:"port"`
}

type aiSettings struct {
	Model                    string `json:"model"`
	ReprioritizeThreshold    int    `json:"reprioritize_threshold"`
	PreferenceUpdateInterval int    `json:"preference_update_interval"`
	AutoDelegateEnabled      bool   `json:"auto_delegate_enabled"`
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
			Enabled:       creds.GitHubPAT != "",
			BaseURL:       creds.GitHubURL,
			HasToken:      creds.GitHubPAT != "",
			PollInterval:  cfg.GitHub.PollInterval.String(),
			CloneProtocol: defaultedCloneProtocol(cfg.GitHub.CloneProtocol),
		},
		Jira: jiraSettings{
			Enabled:      creds.JiraPAT != "",
			BaseURL:      creds.JiraURL,
			HasToken:     creds.JiraPAT != "",
			PollInterval: cfg.Jira.PollInterval.String(),
			Projects:     toJiraProjectSettings(cfg.Jira.Projects),
		},
		Server: serverSettings{
			Port: cfg.Server.Port,
		},
		AI: aiSettings{
			Model:                    cfg.AI.Model,
			ReprioritizeThreshold:    cfg.AI.ReprioritizeThreshold,
			PreferenceUpdateInterval: cfg.AI.PreferenceUpdateInterval,
			AutoDelegateEnabled:      cfg.AI.AutoDelegateEnabled,
		},
	}

	if resp.Jira.Projects == nil {
		resp.Jira.Projects = []jiraProjectSettings{}
	}

	writeJSON(w, http.StatusOK, resp)
}

// toJiraProjectSettings converts the persisted Config view into the
// wire shape, normalizing nil Members slices to empty slices so the
// JSON response is friendly to FE consumers (no `members:null`).
func toJiraProjectSettings(in []config.JiraProjectConfig) []jiraProjectSettings {
	out := make([]jiraProjectSettings, 0, len(in))
	for _, p := range in {
		out = append(out, jiraProjectSettings{
			Key:        p.Key,
			Pickup:     normalizeRule(p.Pickup),
			InProgress: normalizeRule(p.InProgress),
			Done:       normalizeRule(p.Done),
		})
	}
	return out
}

// normalizeRule replaces a nil Members slice with an empty one so the
// JSON encoding is `[]` rather than `null`. Canonical and other fields
// pass through unchanged.
func normalizeRule(r config.JiraStatusRule) config.JiraStatusRule {
	if r.Members == nil {
		r.Members = []string{}
	}
	return r
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
	GitHubPollInterval  string `json:"github_poll_interval"`
	GitHubCloneProtocol string `json:"github_clone_protocol"` // "ssh" | "https" | "" (don't touch)
	JiraPollInterval    string `json:"jira_poll_interval"`
	// JiraProjects is a pointer so the request can distinguish "don't
	// touch" (nil) from "wipe to empty" ([]). When non-nil, the slice
	// is the full new project list — each entry carries its own
	// Pickup/InProgress/Done rules. SKY-272 collapsed the previous
	// global rule fields into this per-project shape.
	JiraProjects   *[]jiraProjectSettings `json:"jira_projects,omitempty"`
	AIModel        string                 `json:"ai_model"`
	AIAutoDelegate *bool                  `json:"ai_auto_delegate_enabled"` // pointer to distinguish absent from false
	ServerPort     int                    `json:"server_port"`
}

func (s *Server) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	var req settingsUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	// Load existing state — snapshot for change detection
	cfg, err := config.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load config: " + err.Error()})
		return
	}
	creds, _ := auth.Load() // auth errors are non-fatal

	// Snapshot pre-change values for diffing
	prevGHURL := creds.GitHubURL
	prevGHPAT := creds.GitHubPAT
	prevGHPollInterval := cfg.GitHub.PollInterval
	prevGHCloneProtocol := cfg.GitHub.CloneProtocol
	prevJiraURL := creds.JiraURL
	prevJiraPAT := creds.JiraPAT
	prevJiraProjects := cloneJiraProjects(cfg.Jira.Projects)
	prevJiraPollInterval := cfg.Jira.PollInterval

	// --- Handle GitHub ---
	//
	// The GitHub login lives on users.github_username (not the keychain).
	// This handler writes the column directly when we validate a new PAT
	// or backfill an empty row.
	if req.GitHubEnabled {
		if req.GitHubURL != "" {
			cfg.GitHub.BaseURL = req.GitHubURL
			creds.GitHubURL = req.GitHubURL
		}
		// New token provided — validate it.
		if req.GitHubPAT != "" {
			url := req.GitHubURL
			if url == "" {
				url = creds.GitHubURL
			}
			if url == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "GitHub URL is required"})
				return
			}
			ghUser, err := auth.ValidateGitHub(url, req.GitHubPAT)
			if err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
					"error": "GitHub: " + err.Error(),
					"field": "github",
				})
				return
			}
			creds.GitHubPAT = req.GitHubPAT
			// Username persistence targets the LocalDefaultUserID row —
			// only safe in local mode. Multi mode must derive the
			// authenticated user ID from the session (SKY-251).
			if runmode.Current() == runmode.ModeLocal {
				if err := s.users.SetGitHubUsername(r.Context(), runmode.LocalDefaultUserID, ghUser.Login); err != nil {
					log.Printf("[settings] failed to persist users.github_username: %v", err)
				}
			}
		}
		// Backfill username on the users row when we have a PAT but the row
		// is empty (e.g. user saves a PAT for the first time without changing
		// it through the validation branch above). Skip on DB error — a
		// transient read failure shouldn't fan out into a GitHub API call;
		// the next Settings save retries naturally. Local mode only — same
		// reasoning as the validation-branch write above.
		if creds.GitHubPAT != "" && runmode.Current() == runmode.ModeLocal {
			stored, err := s.users.GetGitHubUsername(r.Context(), runmode.LocalDefaultUserID)
			if err != nil {
				log.Printf("[settings] failed to read users.github_username for backfill: %v (skipping backfill this save)", err)
			} else if stored == "" {
				url := creds.GitHubURL
				if url == "" {
					url = cfg.GitHub.BaseURL
				}
				if url != "" {
					if ghUser, err := auth.ValidateGitHub(url, creds.GitHubPAT); err == nil {
						if err := s.users.SetGitHubUsername(r.Context(), runmode.LocalDefaultUserID, ghUser.Login); err != nil {
							log.Printf("[settings] failed to backfill users.github_username: %v", err)
						}
					}
				}
			}
		}
	} else {
		// Disabled — clear GitHub credentials, keychain entries, and tracked data
		// Disconnect is a soft gesture — clear credentials and stop polling,
		// but keep entities/tasks/runs/memory intact. Reconnecting the same
		// account resumes where we left off. Full wipe is a separate
		// destructive action (not implemented in v1).
		creds.GitHubURL = ""
		creds.GitHubPAT = ""
		cfg.GitHub.BaseURL = ""
		if err := auth.ClearGitHub(); err != nil {
			log.Printf("[settings] failed to clear GitHub keychain entry: %v", err)
		}
		// Also clear the captured login on the users row so a downstream
		// "are we connected to GitHub" check via DB stays in sync with the
		// keychain reality (PAT gone → username should be gone too).
		// Local mode only — multi mode must clear the session user's row.
		if runmode.Current() == runmode.ModeLocal {
			if err := s.users.SetGitHubUsername(r.Context(), runmode.LocalDefaultUserID, ""); err != nil {
				log.Printf("[settings] failed to clear users.github_username: %v", err)
			}
		}
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
			jiraUser, err := auth.ValidateJira(url, req.JiraPAT)
			if err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
					"error": "Jira: " + err.Error(),
					"field": "jira",
				})
				return
			}
			creds.JiraPAT = req.JiraPAT
			creds.JiraDisplayName = jiraUser.DisplayName
		}
	} else {
		// Soft disconnect — keep entities/tasks/runs/memory intact.
		creds.JiraURL = ""
		creds.JiraPAT = ""
		creds.JiraDisplayName = ""
		cfg.Jira.BaseURL = ""
		if err := auth.ClearJira(); err != nil {
			log.Printf("[settings] failed to clear Jira keychain entry: %v", err)
		}
	}

	// --- Update config fields ---
	if req.GitHubPollInterval != "" {
		if d, err := time.ParseDuration(req.GitHubPollInterval); err == nil && d >= 10*time.Second {
			cfg.GitHub.PollInterval = d
		}
	}
	// Empty string means "don't touch" so the toggle UX (which always
	// sends one of "ssh" / "https") flips the value while older clients
	// that omit the field leave it alone. Unrecognized values are
	// rejected rather than silently coerced — the frontend should never
	// send anything other than the two known values.
	if req.GitHubCloneProtocol != "" {
		if req.GitHubCloneProtocol != "ssh" && req.GitHubCloneProtocol != "https" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "github_clone_protocol must be 'ssh' or 'https'"})
			return
		}
		cfg.GitHub.CloneProtocol = req.GitHubCloneProtocol
	}
	if req.JiraPollInterval != "" {
		if d, err := time.ParseDuration(req.JiraPollInterval); err == nil && d >= 10*time.Second {
			cfg.Jira.PollInterval = d
		}
	}
	// JiraProjects carries the full per-project array. Validation runs
	// over each entry's rules before any mutation — one bad rule rejects
	// the whole request so cfg never lands in a partial state. Keys are
	// normalized (trim + uppercase) and regex-validated against Jira's
	// own project-key shape; duplicates after normalization are rejected
	// so "SKY" and "sky" can't both land. Rule completeness is enforced
	// by validateProjectRules — partial saves are not a supported state.
	if req.JiraProjects != nil {
		seen := map[string]bool{}
		next := make([]config.JiraProjectConfig, 0, len(*req.JiraProjects))
		for _, p := range *req.JiraProjects {
			key := normalizeJiraProjectKey(p.Key)
			if key == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "jira_projects: project key must not be empty"})
				return
			}
			if !jiraProjectKeyRe.MatchString(key) {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "jira_projects: invalid project key " + key + " (must match Jira's format: a leading uppercase letter followed by uppercase letters, digits, or underscores)",
				})
				return
			}
			if seen[key] {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "jira_projects: duplicate project key " + key})
				return
			}
			seen[key] = true
			normalized := config.JiraProjectConfig{
				Key:        key,
				Pickup:     p.Pickup,
				InProgress: p.InProgress,
				Done:       p.Done,
			}
			if err := validateProjectRules(normalized); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			next = append(next, normalized)
		}
		cfg.Jira.Projects = next
	}
	if req.AIModel != "" {
		cfg.AI.Model = req.AIModel
	}
	if req.AIAutoDelegate != nil {
		cfg.AI.AutoDelegateEnabled = *req.AIAutoDelegate
	}
	if req.ServerPort > 0 {
		cfg.Server.Port = req.ServerPort
	}

	// Hard-block a transition into SSH mode if our preflight against
	// the configured GitHub host can't authenticate. Otherwise the
	// toggle would "succeed" silently — repairOriginURL is a local
	// config write that doesn't test connectivity, so the failure
	// wouldn't surface until the next poll/delegation tries to fetch.
	// Only gate the transition (prev != "ssh") so a user with broken
	// SSH today can still save unrelated fields without being held
	// hostage to fix SSH first; switching AWAY from SSH is also
	// unblocked. Probe target is derived from creds.GitHubURL so GHE
	// users see hints with their hostname, not "github.com".
	if cfg.GitHub.CloneProtocol == "ssh" && prevGHCloneProtocol != "ssh" {
		sshHost := worktree.SSHHostFromBaseURL(creds.GitHubURL)
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		err := worktree.PreflightSSH(ctx, sshHost)
		cancel()
		if err != nil {
			log.Printf("[settings] blocked SSH switch against %s for %q: preflight failed: %v", sshHost, creds.GitHubURL, err)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error":  fmt.Sprintf("SSH preflight against %s failed — fix your SSH setup or keep HTTPS. %s", sshHost, err.Error()),
				"field":  "github_clone_protocol",
				"stderr": err.Error(),
			})
			return
		}
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

	// Detect what changed and fire the appropriate callback. Treat a
	// CloneProtocol flip the same as URL/PAT/PollInterval changes:
	// onGitHubChanged re-runs profiling AND bootstrapBareClones, which
	// is exactly what we need to repair every bare's origin URL to the
	// new form.
	ghChanged := creds.GitHubURL != prevGHURL ||
		creds.GitHubPAT != prevGHPAT ||
		cfg.GitHub.PollInterval != prevGHPollInterval ||
		cfg.GitHub.CloneProtocol != prevGHCloneProtocol

	jiraChanged := creds.JiraURL != prevJiraURL ||
		creds.JiraPAT != prevJiraPAT ||
		!jiraProjectsEqual(cfg.Jira.Projects, prevJiraProjects) ||
		cfg.Jira.PollInterval != prevJiraPollInterval

	// Mark Jira restarted synchronously before launching the async callback so
	// jiraPollReady flips false before this request returns. Otherwise the
	// frontend can race ahead and hit /api/jira/stock while the old state is
	// still reported as ready.
	if ghChanged && s.onGitHubChanged != nil {
		// GitHub change triggers full restart (includes Jira poller restart)
		s.MarkJiraRestarted()
		go s.onGitHubChanged()
	} else if jiraChanged && s.onJiraChanged != nil {
		// Jira-only change — just restart Jira poller
		s.MarkJiraRestarted()
		go s.onJiraChanged()
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// handleJiraConnect validates and stores Jira credentials without saving
// the rest of the settings. This powers the two-stage settings flow: connect
// first, then configure projects and statuses.
func (s *Server) handleJiraConnect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
		PAT string `json:"pat"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.URL == "" || req.PAT == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url and pat are required"})
		return
	}

	jiraUser, err := auth.ValidateJira(req.URL, req.PAT)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}

	// Load existing state before writing anything (fail early)
	creds, err := auth.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load credentials: " + err.Error()})
		return
	}
	cfg, err := config.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load config: " + err.Error()})
		return
	}

	// Persist credentials and config
	creds.JiraURL = req.URL
	creds.JiraPAT = req.PAT
	creds.JiraDisplayName = jiraUser.DisplayName
	cfg.Jira.BaseURL = req.URL

	if err := auth.Store(creds); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to store credentials: " + err.Error()})
		return
	}
	if err := config.Save(cfg); err != nil {
		// Roll back keychain to avoid creds/config desync
		creds.JiraURL = ""
		creds.JiraPAT = ""
		auth.Store(creds) //nolint:errcheck
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save config: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":       "connected",
		"display_name": jiraUser.DisplayName,
	})
}

// handleJiraStatuses returns available statuses for given Jira projects.
// Query params: ?project=PROJ1&project=PROJ2 (or uses configured projects if omitted).
func (s *Server) handleJiraStatuses(w http.ResponseWriter, r *http.Request) {
	creds, _ := auth.Load()
	if creds.JiraPAT == "" || creds.JiraURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Jira not configured"})
		return
	}

	projects := r.URL.Query()["project"]
	if len(projects) == 0 {
		cfg, _ := config.Load()
		projects = cfg.Jira.ProjectKeys()
	}
	if len(projects) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no projects specified"})
		return
	}

	client := jira.NewClient(creds.JiraURL, creds.JiraPAT)

	// Intersect statuses across all projects — only return statuses that
	// exist in every project. A union would let users pick a status that
	// fails on some projects (TransitionTo can't find the transition).
	var counts map[string]int            // status name → number of projects it appears in
	var canonical map[string]jira.Status // status name → first-seen Status object
	for i, proj := range projects {
		projectStatuses, err := client.ProjectStatuses(proj)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch statuses for " + proj + ": " + err.Error()})
			return
		}
		if i == 0 {
			counts = make(map[string]int, len(projectStatuses))
			canonical = make(map[string]jira.Status, len(projectStatuses))
		}
		seen := map[string]bool{}
		for _, st := range projectStatuses {
			if !seen[st.Name] {
				seen[st.Name] = true
				counts[st.Name]++
				if _, ok := canonical[st.Name]; !ok {
					canonical[st.Name] = st
				}
			}
		}
	}

	var statuses []jira.Status
	for name, count := range counts {
		if count == len(projects) {
			statuses = append(statuses, canonical[name])
		}
	}
	sort.Slice(statuses, func(i, j int) bool {
		return statuses[i].Name < statuses[j].Name
	})

	writeJSON(w, http.StatusOK, statuses)
}

// handleGitHubPreflightSSH probes whether the user's machine can
// authenticate to GitHub over SSH (key + agent + known_hosts all
// usable). Powers the Setup wizard's gating UX and the Settings
// page's "Test SSH connection" button. Always returns HTTP 200 — the
// body's "ok" flag is the verdict — so the client can distinguish
// "preflight reported failure" from "the server itself errored".
//
// Logs both the success path and the failure stderr to the daemon's
// log so users investigating issues see the exact ssh output even
// when the UI only renders the friendly summary.
func (s *Server) handleGitHubPreflightSSH(w http.ResponseWriter, r *http.Request) {
	// Probe target tracks the configured GitHub base URL so the Test
	// SSH button on the Settings page works for GHE deployments. We
	// load creds (not config) because creds.GitHubURL is the URL the
	// user actually authenticates against; cfg.GitHub.BaseURL mirrors
	// it but the keychain copy is the source of truth.
	creds, _ := auth.Load()
	sshHost := worktree.SSHHostFromBaseURL(creds.GitHubURL)

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := worktree.PreflightSSH(ctx, sshHost); err != nil {
		log.Printf("[settings] SSH preflight against %s failed: %v", sshHost, err)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     false,
			"stderr": err.Error(),
			"host":   sshHost,
		})
		return
	}
	log.Printf("[settings] SSH preflight ok (%s authenticated)", sshHost)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "host": sshHost})
}
