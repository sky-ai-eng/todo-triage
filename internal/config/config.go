// Package config holds the user-editable settings struct and persists it
// to the normalized settings tables (org_settings, team_settings,
// user_settings, jira_project_status_rules, instance_config). Init must
// be called once at startup with an open DB handle before any Load/Save
// call.
//
// The same tables exist in both SQLite and Postgres (multi-mode admins
// edit them via SKY-257 D14 admin UI). In local mode every read is
// scoped to runmode.LocalDefaultOrgID / LocalDefaultTeamID /
// LocalDefaultUserID.
package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Config is the persisted settings shape. JSON tags are retained for
// HTTP responses (internal/server/settings.go); the struct itself
// is no longer marshaled to a single blob.
type Config struct {
	GitHub GitHubConfig `json:"github"`
	Jira   JiraConfig   `json:"jira"`
	Server ServerConfig `json:"server"`
	AI     AIConfig     `json:"ai"`
}

type GitHubConfig struct {
	BaseURL      string        `json:"base_url"`
	PollInterval time.Duration `json:"poll_interval"`
	// CloneProtocol controls how bare clones in ~/.triagefactory/repos/
	// are created. "ssh" uses git@github.com:owner/repo.git, "https"
	// uses GitHub's clone_url (depends on a credential helper holding
	// a PAT). Empty string means the default clone protocol is used,
	// which is "ssh".
	CloneProtocol string `json:"clone_protocol,omitempty"`
}

type JiraConfig struct {
	BaseURL      string        `json:"base_url"`
	PollInterval time.Duration `json:"poll_interval"`
	Projects     []string      `json:"projects"`
	// Pickup/InProgress/Done are team-wide in local mode: the same
	// rule applies to every Jira project the team works on. Persisted
	// to jira_project_status_rules (team-keyed) with one row per
	// project_key, all sharing identical values. Multi-mode admins
	// can edit per-project values directly via the admin UI through
	// a separate handler path — do not route those edits through
	// config.Save() or they collapse back to one shared value.
	Pickup     JiraStatusRule `json:"pickup"`
	InProgress JiraStatusRule `json:"in_progress"`
	Done       JiraStatusRule `json:"done"`
}

// JiraStatusRule captures the two questions a user answers about a Jira state:
// which statuses count as this state (Members) and which status TF writes when
// it transitions into it (Canonical). Canonical is empty for Pickup since TF
// never writes tickets back to the pickup state — it only reads them.
type JiraStatusRule struct {
	Members   []string `json:"members"`
	Canonical string   `json:"canonical,omitempty"`
}

// Contains reports whether the given Jira status name is a member of this rule.
func (r JiraStatusRule) Contains(status string) bool {
	for _, m := range r.Members {
		if m == status {
			return true
		}
	}
	return false
}

type ServerConfig struct {
	Port int `json:"port"`
	// TakeoverDir is where the takeover endpoint clones run worktrees so
	// the user can resume the headless Claude Code session interactively.
	// Lives outside $TMPDIR so the worktree-cleanup safety rail leaves it
	// alone. A leading "~" is expanded against the user's home dir at use
	// time. Empty means "use the default" (~/.triagefactory/takeovers).
	TakeoverDir string `json:"takeover_dir,omitempty"`
}

// ResolvedTakeoverDir returns ServerConfig.TakeoverDir with a leading "~"
// expanded and the default applied when the field is empty. Centralized
// here so callers don't each re-implement the home-dir math.
func (c ServerConfig) ResolvedTakeoverDir() (string, error) {
	dir := c.TakeoverDir
	if dir == "" {
		dir = "~/.triagefactory/takeovers"
	}
	if len(dir) >= 2 && dir[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, dir[2:])
	}
	return dir, nil
}

type AIConfig struct {
	Model                    string `json:"model"`
	ReprioritizeThreshold    int    `json:"reprioritize_threshold"`
	PreferenceUpdateInterval int    `json:"preference_update_interval"`
	AutoDelegateEnabled      bool   `json:"auto_delegate_enabled"`
}

// Ready returns true if GitHub credentials are configured.
// Repo count must be checked separately via the DB.
func (c GitHubConfig) Ready(pat, url string) bool {
	return pat != "" && url != ""
}

// Ready returns true if Jira is fully configured: credentials, at least one
// project, AND all three status rules populated. The rule check is deliberate
// — after a config-shape upgrade old YAML keys silently drop out, leaving the
// new Pickup/InProgress/Done rules empty. Without this gate the poller would
// still start and emit degraded events (no terminal check, failing claims on
// the server), which violates the "full re-setup on upgrade" contract.
// Pickup only needs members (TF never writes to pickup); InProgress and Done
// additionally need a canonical write target.
func (c JiraConfig) Ready(pat, url string) bool {
	if pat == "" || url == "" || len(c.Projects) == 0 {
		return false
	}
	if len(c.Pickup.Members) == 0 {
		return false
	}
	if c.InProgress.Canonical == "" || len(c.InProgress.Members) == 0 {
		return false
	}
	if c.Done.Canonical == "" || len(c.Done.Members) == 0 {
		return false
	}
	return true
}

// Default returns a Config with sensible defaults matching the spec.
func Default() Config {
	return Config{
		GitHub: GitHubConfig{
			PollInterval:  5 * time.Minute,
			CloneProtocol: "ssh",
		},
		Jira: JiraConfig{
			PollInterval: 5 * time.Minute,
		},
		Server: ServerConfig{
			Port: 3000,
		},
		AI: AIConfig{
			Model:                    "sonnet",
			ReprioritizeThreshold:    5,
			PreferenceUpdateInterval: 20,
			AutoDelegateEnabled:      true,
		},
	}
}

// Package-level DB handle used by Load/Save. Set once via Init at
// startup. A package singleton lets the 25+ existing call sites keep
// the no-arg Load()/Save() signatures rather than threading *sql.DB
// through every layer.
var (
	pkgMu sync.RWMutex
	pkgDB *sql.DB
)

// ErrNotInitialized is returned by Load/Save when called before Init.
// In production this is a startup-ordering bug (Init must run after
// db.Migrate); in tests it's a hint to set up a temp DB.
var ErrNotInitialized = errors.New("config: Init not called")

// Init wires the package against an open, migrated DB handle.
// Subsequent calls replace the handle (useful for tests). Does NOT
// touch the filesystem; fresh installs land at Default() on first
// Load() and write the per-tenant rows on first Save().
func Init(db *sql.DB) error {
	if db == nil {
		return errors.New("config.Init: nil db")
	}
	pkgMu.Lock()
	pkgDB = db
	pkgMu.Unlock()
	return nil
}

// Load assembles a Config from the five tables (instance_config,
// org_settings, team_settings, user_settings, jira_project_status_rules)
// scoped to the local-mode sentinel IDs. Missing rows degrade to
// Default() values — first Save() upserts them.
func Load() (Config, error) {
	pkgMu.RLock()
	db := pkgDB
	pkgMu.RUnlock()
	if db == nil {
		return Default(), ErrNotInitialized
	}

	cfg := Default()
	ctx := context.Background()

	// instance_config (server: port + takeover_dir)
	var port int
	var takeover string
	switch err := db.QueryRowContext(ctx,
		`SELECT server_port, server_takeover_dir FROM instance_config WHERE id = 1`,
	).Scan(&port, &takeover); {
	case errors.Is(err, sql.ErrNoRows):
		// keep Default()
	case err != nil:
		return cfg, fmt.Errorf("read instance_config: %w", err)
	default:
		cfg.Server.Port = port
		cfg.Server.TakeoverDir = takeover
	}

	// org_settings (github + jira URLs and intervals)
	var ghURL, jiraURL sql.NullString
	var ghInterval, jiraInterval, cloneProto string
	switch err := db.QueryRowContext(ctx, `
		SELECT github_base_url, github_poll_interval, github_clone_protocol,
		       jira_base_url, jira_poll_interval
		FROM org_settings WHERE org_id = ?
	`, runmode.LocalDefaultOrgID).Scan(&ghURL, &ghInterval, &cloneProto, &jiraURL, &jiraInterval); {
	case errors.Is(err, sql.ErrNoRows):
		// keep Default()
	case err != nil:
		return cfg, fmt.Errorf("read org_settings: %w", err)
	default:
		if ghURL.Valid {
			cfg.GitHub.BaseURL = ghURL.String
		}
		if d, err := time.ParseDuration(ghInterval); err == nil {
			cfg.GitHub.PollInterval = d
		}
		cfg.GitHub.CloneProtocol = cloneProto
		if jiraURL.Valid {
			cfg.Jira.BaseURL = jiraURL.String
		}
		if d, err := time.ParseDuration(jiraInterval); err == nil {
			cfg.Jira.PollInterval = d
		}
	}

	// team_settings (jira_projects + AI thresholds). Save always writes
	// non-NULL ints from the in-memory Config, so scanning directly into
	// int matches the only path that ever populates this row in local
	// mode. A future multi-mode admin path that writes NULL via a
	// different code path would surface a scan error here, which is the
	// behavior we want — it would mean the local config layer is being
	// pointed at multi-mode-shaped data.
	var projectsJSON string
	var aiThreshold, aiInterval int
	switch err := db.QueryRowContext(ctx, `
		SELECT jira_projects, ai_reprioritize_threshold, ai_preference_update_interval
		FROM team_settings WHERE team_id = ?
	`, runmode.LocalDefaultTeamID).Scan(&projectsJSON, &aiThreshold, &aiInterval); {
	case errors.Is(err, sql.ErrNoRows):
		// keep Default()
	case err != nil:
		return cfg, fmt.Errorf("read team_settings: %w", err)
	default:
		if projectsJSON != "" {
			var projects []string
			if err := json.Unmarshal([]byte(projectsJSON), &projects); err != nil {
				return cfg, fmt.Errorf("unmarshal team_settings.jira_projects: %w", err)
			}
			cfg.Jira.Projects = projects
		}
		cfg.AI.ReprioritizeThreshold = aiThreshold
		cfg.AI.PreferenceUpdateInterval = aiInterval
	}

	// user_settings (AI model + auto-delegate)
	var aiModel string
	var aiAutoDelegate bool
	switch err := db.QueryRowContext(ctx, `
		SELECT ai_model, ai_auto_delegate_enabled
		FROM user_settings WHERE user_id = ?
	`, runmode.LocalDefaultUserID).Scan(&aiModel, &aiAutoDelegate); {
	case errors.Is(err, sql.ErrNoRows):
		// keep Default()
	case err != nil:
		return cfg, fmt.Errorf("read user_settings: %w", err)
	default:
		cfg.AI.Model = aiModel
		cfg.AI.AutoDelegateEnabled = aiAutoDelegate
	}

	// jira_project_status_rules (Pickup / InProgress / Done)
	//
	// Team-keyed (different teams within an org can have different
	// status rules for the same Jira project). Local mode treats all
	// projects uniformly — all rows for the LocalDefaultTeam share the
	// same values. Read the first row to populate the team-wide rule.
	// Returns ErrNoRows when no projects have been configured yet,
	// which keeps Default()'s empty rules.
	if rule, err := loadJiraStatusRules(ctx, db, runmode.LocalDefaultTeamID); err != nil {
		return cfg, fmt.Errorf("read jira_project_status_rules: %w", err)
	} else if rule != nil {
		cfg.Jira.Pickup = rule.Pickup
		cfg.Jira.InProgress = rule.InProgress
		cfg.Jira.Done = rule.Done
	}

	return cfg, nil
}

// jiraStatusRules bundles the three rules read from a single
// jira_project_status_rules row.
type jiraStatusRules struct {
	Pickup     JiraStatusRule
	InProgress JiraStatusRule
	Done       JiraStatusRule
}

func loadJiraStatusRules(ctx context.Context, db *sql.DB, teamID string) (*jiraStatusRules, error) {
	var pickupJSON, inProgressJSON, doneJSON string
	var inProgressCanon, doneCanon sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT pickup_members, in_progress_members, in_progress_canonical,
		       done_members, done_canonical
		FROM jira_project_status_rules
		WHERE team_id = ?
		LIMIT 1
	`, teamID).Scan(&pickupJSON, &inProgressJSON, &inProgressCanon, &doneJSON, &doneCanon)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rules jiraStatusRules
	if err := json.Unmarshal([]byte(pickupJSON), &rules.Pickup.Members); err != nil {
		return nil, fmt.Errorf("unmarshal pickup_members: %w", err)
	}
	if err := json.Unmarshal([]byte(inProgressJSON), &rules.InProgress.Members); err != nil {
		return nil, fmt.Errorf("unmarshal in_progress_members: %w", err)
	}
	if err := json.Unmarshal([]byte(doneJSON), &rules.Done.Members); err != nil {
		return nil, fmt.Errorf("unmarshal done_members: %w", err)
	}
	if inProgressCanon.Valid {
		rules.InProgress.Canonical = inProgressCanon.String
	}
	if doneCanon.Valid {
		rules.Done.Canonical = doneCanon.String
	}
	return &rules, nil
}

// Save upserts the five settings tables for the local sentinels.
// Jira project rules are materialized one row per project in
// cfg.Jira.Projects, all sharing identical Pickup/InProgress/Done
// values (local mode). Projects no longer in the list have their
// rules row deleted so the table stays clean.
func Save(cfg Config) error {
	pkgMu.RLock()
	db := pkgDB
	pkgMu.RUnlock()
	if db == nil {
		return ErrNotInitialized
	}

	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// instance_config
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO instance_config (id, server_port, server_takeover_dir, updated_at)
		VALUES (1, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			server_port = excluded.server_port,
			server_takeover_dir = excluded.server_takeover_dir,
			updated_at = CURRENT_TIMESTAMP
	`, cfg.Server.Port, cfg.Server.TakeoverDir); err != nil {
		return fmt.Errorf("upsert instance_config: %w", err)
	}

	// org_settings
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO org_settings (
			org_id, github_base_url, github_poll_interval, github_clone_protocol,
			jira_base_url, jira_poll_interval, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(org_id) DO UPDATE SET
			github_base_url = excluded.github_base_url,
			github_poll_interval = excluded.github_poll_interval,
			github_clone_protocol = excluded.github_clone_protocol,
			jira_base_url = excluded.jira_base_url,
			jira_poll_interval = excluded.jira_poll_interval,
			updated_at = CURRENT_TIMESTAMP
	`,
		runmode.LocalDefaultOrgID,
		nullIfEmpty(cfg.GitHub.BaseURL),
		cfg.GitHub.PollInterval.String(),
		defaultedCloneProtocol(cfg.GitHub.CloneProtocol),
		nullIfEmpty(cfg.Jira.BaseURL),
		cfg.Jira.PollInterval.String(),
	); err != nil {
		return fmt.Errorf("upsert org_settings: %w", err)
	}

	// team_settings
	projects := cfg.Jira.Projects
	if projects == nil {
		projects = []string{}
	}
	projectsJSON, err := json.Marshal(projects)
	if err != nil {
		return fmt.Errorf("marshal jira_projects: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO team_settings (
			team_id, jira_projects, ai_reprioritize_threshold,
			ai_preference_update_interval, updated_at
		) VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(team_id) DO UPDATE SET
			jira_projects = excluded.jira_projects,
			ai_reprioritize_threshold = excluded.ai_reprioritize_threshold,
			ai_preference_update_interval = excluded.ai_preference_update_interval,
			updated_at = CURRENT_TIMESTAMP
	`,
		runmode.LocalDefaultTeamID,
		string(projectsJSON),
		cfg.AI.ReprioritizeThreshold,
		cfg.AI.PreferenceUpdateInterval,
	); err != nil {
		return fmt.Errorf("upsert team_settings: %w", err)
	}

	// user_settings
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_settings (
			user_id, ai_model, ai_auto_delegate_enabled, updated_at
		) VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(user_id) DO UPDATE SET
			ai_model = excluded.ai_model,
			ai_auto_delegate_enabled = excluded.ai_auto_delegate_enabled,
			updated_at = CURRENT_TIMESTAMP
	`,
		runmode.LocalDefaultUserID,
		cfg.AI.Model,
		cfg.AI.AutoDelegateEnabled,
	); err != nil {
		return fmt.Errorf("upsert user_settings: %w", err)
	}

	// jira_project_status_rules — one row per project in
	// cfg.Jira.Projects, all sharing the same values. Drop rules
	// for projects no longer in the list.
	pickupJSON, err := json.Marshal(orEmpty(cfg.Jira.Pickup.Members))
	if err != nil {
		return fmt.Errorf("marshal pickup_members: %w", err)
	}
	inProgressJSON, err := json.Marshal(orEmpty(cfg.Jira.InProgress.Members))
	if err != nil {
		return fmt.Errorf("marshal in_progress_members: %w", err)
	}
	doneJSON, err := json.Marshal(orEmpty(cfg.Jira.Done.Members))
	if err != nil {
		return fmt.Errorf("marshal done_members: %w", err)
	}
	// DELETE+INSERT scoped to the local team. In multi mode an admin UI
	// (SKY-257 D14) edits per-project rows directly through its own
	// handlers — it must NOT route through this code path, or per-project
	// Canonical/members differences would collapse to one shared value.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM jira_project_status_rules WHERE team_id = ?`,
		runmode.LocalDefaultTeamID,
	); err != nil {
		return fmt.Errorf("clear jira_project_status_rules: %w", err)
	}
	for _, key := range projects {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO jira_project_status_rules (
				team_id, project_key,
				pickup_members, in_progress_members, in_progress_canonical,
				done_members, done_canonical, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		`,
			runmode.LocalDefaultTeamID, key,
			string(pickupJSON), string(inProgressJSON), nullIfEmpty(cfg.Jira.InProgress.Canonical),
			string(doneJSON), nullIfEmpty(cfg.Jira.Done.Canonical),
		); err != nil {
			return fmt.Errorf("upsert jira_project_status_rules[%s]: %w", key, err)
		}
	}

	return tx.Commit()
}

// defaultedCloneProtocol substitutes "ssh" when the field is empty —
// the org_settings CHECK constraint rejects empty strings, and "ssh"
// is the documented Default().
func defaultedCloneProtocol(p string) string {
	if p == "" {
		return "ssh"
	}
	return p
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
