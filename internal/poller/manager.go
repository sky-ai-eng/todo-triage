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

// Manager manages the lifecycle of polling loops, allowing them to be
// stopped and restarted when credentials or config change.
type Manager struct {
	database *sql.DB
	bus      *eventbus.Bus
	tracker  *tracker.Tracker
	users    db.UsersStore // SKY-264: source of the session user's github_username
	repos    db.RepoStore  // SKY-288: configured-repo names for GitHub poller startup

	// OnError fires when a poll cycle returns an error. Source is "github"
	// or "jira". Wired from main to a toast helper so users see the
	// failure without log-diving; nil-safe if caller doesn't set it.
	OnError func(source string, err error)

	mu       sync.Mutex
	ghStop   chan struct{}
	jiraStop chan struct{}
}

func NewManager(database *sql.DB, bus *eventbus.Bus, users db.UsersStore, tasks db.TaskStore, entities db.EntityStore, repos db.RepoStore) *Manager {
	return &Manager{
		database: database,
		bus:      bus,
		tracker:  tracker.New(database, bus, tasks, entities),
		users:    users,
		repos:    repos,
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
func (m *Manager) startGitHub(cfg config.Config, creds auth.Credentials) {
	if !cfg.GitHub.Ready(creds.GitHubPAT, creds.GitHubURL) {
		log.Println("[github] credentials not configured, skipping tracker")
		return
	}

	repos, err := m.repos.ListConfiguredNames(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		log.Printf("[github] error loading configured repos: %v", err)
		return
	}
	if len(repos) == 0 {
		log.Println("[github] no repos configured, skipping tracker")
		return
	}

	// NULL/empty github_username means identity hasn't been captured
	// yet (fresh install before first Settings save) — short-circuit
	// so the tracker doesn't start without knowing who "me" is.
	username, err := m.users.GetGitHubUsername(context.Background(), runmode.LocalDefaultUserID)
	if err != nil {
		log.Printf("[github] failed to read users.github_username: %v", err)
		return
	}
	if username == "" {
		log.Println("[github] no username stored, skipping tracker")
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
	userTeams, err := client.ListMyTeams()
	if err != nil {
		log.Printf("[github] failed to list teams for %s: %v (team-based review requests will be missed until next restart)", username, err)
		userTeams = nil
	}

	go func() {
		// Initial poll
		if _, err := m.tracker.RefreshGitHub(client, username, userTeams, repos); err != nil {
			log.Printf("[github] tracker error: %v", err)
			m.reportError("github", err)
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if _, err := m.tracker.RefreshGitHub(client, username, userTeams, repos); err != nil {
					log.Printf("[github] tracker error: %v", err)
					m.reportError("github", err)
				}
			case <-stop:
				return
			}
		}
	}()

	log.Printf("[github] tracker started (interval: %s, user: %s, repos: %d, teams: %d)", interval, username, len(repos), len(userTeams))
}

// startJira launches the Jira tracking loop.
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
		if _, err := m.tracker.RefreshJira(client, creds.JiraURL, projects); err != nil {
			log.Printf("[jira] tracker error: %v", err)
			m.reportError("jira", err)
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if _, err := m.tracker.RefreshJira(client, creds.JiraURL, projects); err != nil {
					log.Printf("[jira] tracker error: %v", err)
					m.reportError("jira", err)
				}
			case <-stop:
				return
			}
		}
	}()

	log.Printf("[jira] tracker started (interval: %s, projects: %v)", interval, projectKeys)
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
