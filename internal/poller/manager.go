package poller

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/tracker"
)

// localGitHubUserID is the per-org GitHub-identity userID resolution
// used by the poller. In local mode that's the synthetic sentinel; in
// multi mode this needs to become a per-org lookup against the
// org's owner/operator user (deferred — credential-per-org resolution
// is outside D9c scope, which owns only the outer per-org loop).
var localGitHubUserID = runmode.LocalDefaultUserID

// Manager manages the lifecycle of polling loops, allowing them to be
// stopped and restarted when credentials or config change.
type Manager struct {
	database *sql.DB
	bus      *eventbus.Bus
	tracker  *tracker.Tracker
	users    db.UsersStore // SKY-264: source of the session user's github_username
	repos    db.RepoStore  // SKY-288: configured-repo names for GitHub poller startup
	orgs     db.OrgsStore  // SKY-312: enumerate active orgs at each poll tick

	// OnError fires when a poll cycle returns an error. Source is "github"
	// or "jira". Wired from main to a toast helper so users see the
	// failure without log-diving; nil-safe if caller doesn't set it.
	OnError func(source string, err error)

	mu       sync.Mutex
	ghStop   chan struct{}
	jiraStop chan struct{}
}

func NewManager(database *sql.DB, bus *eventbus.Bus, users db.UsersStore, tasks db.TaskStore, entities db.EntityStore, repos db.RepoStore, orgs db.OrgsStore) *Manager {
	return &Manager{
		database: database,
		bus:      bus,
		tracker:  tracker.New(database, bus, tasks, entities),
		users:    users,
		repos:    repos,
		orgs:     orgs,
	}
}

// reportError invokes the OnError callback if set. Centralized so adding
// behavior later (metrics, rate-limiting) has one call site.
func (m *Manager) reportError(source string, err error) {
	if m.OnError != nil {
		m.OnError(source, err)
	}
}

// RestartAll stops all polling loops and restarts any that are fully configured.
func (m *Manager) RestartAll() {
	m.stopAll()

	cfg, _ := config.Load()
	creds, _ := auth.Load()

	m.startGitHub(cfg, creds)
	m.startJira(cfg, creds)
}

// RestartGitHub stops and restarts only the GitHub polling loop.
func (m *Manager) RestartGitHub() {
	m.mu.Lock()
	if m.ghStop != nil {
		close(m.ghStop)
		m.ghStop = nil
		log.Println("[github] tracker stopped")
	}
	m.mu.Unlock()

	cfg, _ := config.Load()
	creds, _ := auth.Load()
	m.startGitHub(cfg, creds)
}

// RestartJira stops and restarts only the Jira polling loop.
func (m *Manager) RestartJira() {
	m.mu.Lock()
	if m.jiraStop != nil {
		close(m.jiraStop)
		m.jiraStop = nil
		log.Println("[jira] tracker stopped")
	}
	m.mu.Unlock()

	cfg, _ := config.Load()
	creds, _ := auth.Load()
	m.startJira(cfg, creds)
}

// StopAll stops all running polling loops without restarting.
func (m *Manager) StopAll() {
	m.stopAll()
}

// Restart is a convenience alias for RestartAll.
func (m *Manager) Restart() {
	m.RestartAll()
}

func (m *Manager) stopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.ghStop != nil {
		close(m.ghStop)
		m.ghStop = nil
		log.Println("[github] tracker stopped")
	}
	if m.jiraStop != nil {
		close(m.jiraStop)
		m.jiraStop = nil
		log.Println("[jira] tracker stopped")
	}
}

// startGitHub launches the GitHub tracking loop.
//
// SKY-312: each tick iterates active orgs and dispatches one
// RefreshGitHub call per org. Per-org repo lists and per-org user
// identities are resolved inside the loop so a new org added between
// ticks picks up on the next cycle without a poller restart. Local
// mode collapses to N=1 (the synthetic sentinel org). Bounded
// per-org concurrency is a future optimization — sequential is fine
// for v1 multi-mode given the poll period (≥1 minute baseline).
func (m *Manager) startGitHub(cfg config.Config, creds auth.Credentials) {
	if !cfg.GitHub.Ready(creds.GitHubPAT, creds.GitHubURL) {
		log.Println("[github] credentials not configured, skipping tracker")
		return
	}

	interval := cfg.GitHub.PollInterval
	if interval < 10*time.Second {
		interval = time.Minute
	}

	client := ghclient.NewClient(creds.GitHubURL, creds.GitHubPAT)
	stop := make(chan struct{})

	m.mu.Lock()
	m.ghStop = stop
	m.mu.Unlock()

	// Resolve the user's team memberships once per GitHub start. Teams
	// rarely change mid-session, and every GitHub config change (creds,
	// repos) already triggers a RestartGitHub which re-enters this path —
	// so picking up new memberships is a question of when the user next
	// reconnects, not of refresh cadence. An empty list on failure means
	// team-based review requests won't surface until next restart; that's
	// a degraded-but-honest state and the error is logged.
	//
	// Team resolution stays out of the per-org loop because the
	// credential set (cfg.GitHub PAT) is process-global today —
	// per-org credential resolution is deferred (out of D9c scope).
	userTeams, err := client.ListMyTeams()
	if err != nil {
		log.Printf("[github] failed to list teams: %v (team-based review requests will be missed until next restart)", err)
		userTeams = nil
	}

	go func() {
		// Initial poll
		m.runGitHubCycle(client, userTeams)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.runGitHubCycle(client, userTeams)
			case <-stop:
				return
			}
		}
	}()

	log.Printf("[github] tracker started (interval: %s, teams: %d)", interval, len(userTeams))
}

// runGitHubCycle enumerates active orgs and dispatches a per-org
// RefreshGitHub. Per-org failures are logged and reported via
// OnError but do not abort the remaining orgs in the cycle — a
// transient failure on org A shouldn't starve orgs B..N of polls.
func (m *Manager) runGitHubCycle(client *ghclient.Client, userTeams []string) {
	ctx := context.Background()
	orgIDs, err := m.orgs.ListActiveSystem(ctx)
	if err != nil {
		log.Printf("[github] list active orgs: %v", err)
		m.reportError("github", err)
		return
	}
	for _, orgID := range orgIDs {
		repos, err := m.repos.ListConfiguredNamesSystem(ctx, orgID)
		if err != nil {
			log.Printf("[github] org %s: load configured repos: %v", orgID, err)
			continue
		}
		if len(repos) == 0 {
			continue
		}
		// NULL/empty github_username means identity hasn't been
		// captured yet (fresh install before first Settings save)
		// — skip this org without surfacing as an error.
		username, err := m.users.GetGitHubUsernameSystem(ctx, localGitHubUserID)
		if err != nil {
			log.Printf("[github] org %s: read users.github_username: %v", orgID, err)
			continue
		}
		if username == "" {
			continue
		}
		if _, err := m.tracker.RefreshGitHub(orgID, client, username, userTeams, repos); err != nil {
			log.Printf("[github] org %s: tracker error: %v", orgID, err)
			m.reportError("github", err)
		}
	}
}

// startJira launches the Jira tracking loop.
//
// SKY-312: each tick iterates active orgs and dispatches one
// RefreshJira call per org. Jira project rules are still process-
// global today (sourced from cfg.Jira), so the per-org loop is
// effectively a fan-out of the same project set across orgs — that
// matches local-mode behavior (N=1, the synthetic sentinel org) and
// keeps the multi-mode outer-loop shape symmetric with the GitHub
// path. Per-org Jira project configuration is a future concern.
func (m *Manager) startJira(cfg config.Config, creds auth.Credentials) {
	if !cfg.Jira.Ready(creds.JiraPAT, creds.JiraURL) {
		log.Println("[jira] not fully configured, skipping tracker")
		return
	}

	interval := cfg.Jira.PollInterval
	if interval < 10*time.Second {
		interval = time.Minute
	}

	client := jiraclient.NewClient(creds.JiraURL, creds.JiraPAT)
	stop := make(chan struct{})

	m.mu.Lock()
	m.jiraStop = stop
	m.mu.Unlock()

	projects := toTrackerJiraRules(cfg.Jira.Projects)
	projectKeys := cfg.Jira.ProjectKeys()

	go func() {
		// Initial poll
		m.runJiraCycle(client, creds.JiraURL, projects)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.runJiraCycle(client, creds.JiraURL, projects)
			case <-stop:
				return
			}
		}
	}()

	log.Printf("[jira] tracker started (interval: %s, projects: %v)", interval, projectKeys)
}

// runJiraCycle enumerates active orgs and dispatches a per-org
// RefreshJira. Per-org failures are logged and reported via
// OnError but do not abort the remaining orgs in the cycle.
func (m *Manager) runJiraCycle(client *jiraclient.Client, baseURL string, projects tracker.JiraRules) {
	ctx := context.Background()
	orgIDs, err := m.orgs.ListActiveSystem(ctx)
	if err != nil {
		log.Printf("[jira] list active orgs: %v", err)
		m.reportError("jira", err)
		return
	}
	for _, orgID := range orgIDs {
		if _, err := m.tracker.RefreshJira(orgID, client, baseURL, projects); err != nil {
			log.Printf("[jira] org %s: tracker error: %v", orgID, err)
			m.reportError("jira", err)
		}
	}
}

// toTrackerJiraRules converts the config-layer per-project rule slice
// to the tracker-local view. Kept narrow on purpose — the tracker
// package doesn't import internal/config so the two shapes stay
// decoupled.
func toTrackerJiraRules(projects []config.JiraProjectConfig) tracker.JiraRules {
	out := make(tracker.JiraRules, 0, len(projects))
	for _, p := range projects {
		out = append(out, tracker.JiraProjectRules{
			Key:           p.Key,
			PickupMembers: p.Pickup.Members,
			DoneMembers:   p.Done.Members,
		})
	}
	return out
}
