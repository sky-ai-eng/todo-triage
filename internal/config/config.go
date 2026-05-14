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
	"log"
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

// JiraConfig holds the per-team Jira settings. Projects is the source of
// truth for "which Jira projects this team tracks" and what status rules
// each project uses — teams with multiple projects whose workflows differ
// ("Backlog/Selected/InProgress/Done" vs "New/Triage/Active/Resolved")
// store one entry per project rather than collapsing to a unified
// superset.
type JiraConfig struct {
	BaseURL      string              `json:"base_url"`
	PollInterval time.Duration       `json:"poll_interval"`
	Projects     []JiraProjectConfig `json:"projects"`
}

// JiraProjectConfig is the per-project status configuration. Key is the
// Jira project_key (e.g. "SKY") used to match events to rules.
type JiraProjectConfig struct {
	Key        string         `json:"key"`
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

// ProjectKeys returns the list of project keys in order, with empty
// keys filtered out. Helpful for callers that just need the names
// (poller dispatch, JQL queries, validation).
func (c JiraConfig) ProjectKeys() []string {
	keys := make([]string, 0, len(c.Projects))
	for _, p := range c.Projects {
		if p.Key != "" {
			keys = append(keys, p.Key)
		}
	}
	return keys
}

// RuleForProject returns the per-project rules for the given key, or
// nil when no project with that key is configured. Callers should
// degrade gracefully on a nil return — typically by treating the event
// as if "no rules configured" (no terminal check, no transitions).
func (c JiraConfig) RuleForProject(key string) *JiraProjectConfig {
	for i := range c.Projects {
		if c.Projects[i].Key == key {
			return &c.Projects[i]
		}
	}
	return nil
}

// AllPickupMembers returns the union of every project's Pickup.Members
// — used by JQL queries that span the team's full project list. Each
// member is returned once; ordering follows first-seen project order.
func (c JiraConfig) AllPickupMembers() []string {
	return c.unionMembers(func(p JiraProjectConfig) []string { return p.Pickup.Members })
}

// AllDoneMembers returns the union of every project's Done.Members. Used
// by JQL queries that exclude terminal tickets across the team's full
// project list.
func (c JiraConfig) AllDoneMembers() []string {
	return c.unionMembers(func(p JiraProjectConfig) []string { return p.Done.Members })
}

func (c JiraConfig) unionMembers(pick func(JiraProjectConfig) []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, p := range c.Projects {
		for _, m := range pick(p) {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
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
// project, AND every project has its three status rules populated. The rule
// check is deliberate — after a config-shape upgrade old rule entries silently
// drop out, leaving the new Pickup/InProgress/Done rules empty. Without this
// gate the poller would still start and emit degraded events (no terminal
// check, failing claims on the server), which violates the "full re-setup on
// upgrade" contract. Pickup only needs members (TF never writes to pickup);
// InProgress and Done additionally need a canonical write target.
func (c JiraConfig) Ready(pat, url string) bool {
	if pat == "" || url == "" || len(c.Projects) == 0 {
		return false
	}
	for _, p := range c.Projects {
		if p.Key == "" {
			return false
		}
		if len(p.Pickup.Members) == 0 {
			return false
		}
		if p.InProgress.Canonical == "" || len(p.InProgress.Members) == 0 {
			return false
		}
		if p.Done.Canonical == "" || len(p.Done.Members) == 0 {
			return false
		}
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
//
// Error contract: on the first per-table read failure, Load returns
// (Default(), err) — never a partially-built struct. Callers that
// swallow the error (cfg, _ := config.Load()) implicitly take
// Default() in the error path, matching the fresh-install behavior.
// Without this contract, a mid-sequence failure would surface a mix
// of "values from earlier successful reads" + "Default() for later
// tables" — silently inconsistent state that's hard to reason about
// at call sites.
//
// To avoid the silent-degradation footgun for callers that drop the
// error (a transient DB hiccup could otherwise quietly swap their
// configured BaseURL/poll intervals for the package defaults), every
// error path emits a `[config] load failed, degrading to Default()`
// log line. Callers that need stricter handling still get the error
// back to act on it.
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
		return loadFail(fmt.Errorf("read instance_config: %w", err))
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
		return loadFail(fmt.Errorf("read org_settings: %w", err))
	default:
		if ghURL.Valid {
			cfg.GitHub.BaseURL = ghURL.String
		}
		d, err := time.ParseDuration(ghInterval)
		if err != nil {
			return loadFail(fmt.Errorf("parse org_settings github_poll_interval %q: %w", ghInterval, err))
		}
		cfg.GitHub.PollInterval = d
		cfg.GitHub.CloneProtocol = cloneProto
		if jiraURL.Valid {
			cfg.Jira.BaseURL = jiraURL.String
		}
		d, err = time.ParseDuration(jiraInterval)
		if err != nil {
			return loadFail(fmt.Errorf("parse org_settings jira_poll_interval %q: %w", jiraInterval, err))
		}
		cfg.Jira.PollInterval = d
	}

	// team_settings (AI thresholds). The AI columns ship NOT NULL DEFAULT
	// in both backends, so scanning directly into int is safe — the schema
	// invariant holds whether the row was inserted by Save() or by any
	// future admin path.
	//
	// jira_projects is read for compatibility but not used as the source
	// of truth — jira_project_status_rules is. The column stays in sync
	// on every Save() as a fast path for "which projects to poll" without
	// joining, but Load assembles cfg.Jira.Projects from the rules table.
	var projectsJSON string
	var aiThreshold, aiInterval int
	switch err := db.QueryRowContext(ctx, `
		SELECT jira_projects, ai_reprioritize_threshold, ai_preference_update_interval
		FROM team_settings WHERE team_id = ?
	`, runmode.LocalDefaultTeamID).Scan(&projectsJSON, &aiThreshold, &aiInterval); {
	case errors.Is(err, sql.ErrNoRows):
		// keep Default()
	case err != nil:
		return loadFail(fmt.Errorf("read team_settings: %w", err))
	default:
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
		return loadFail(fmt.Errorf("read user_settings: %w", err))
	default:
		cfg.AI.Model = aiModel
		cfg.AI.AutoDelegateEnabled = aiAutoDelegate
	}

	// jira_project_status_rules — one row per project key, each carrying
	// its own Pickup/InProgress/Done. Iterated in project_key order so
	// the in-memory slice is stable across reads.
	projects, err := loadJiraProjectConfigs(ctx, db, runmode.LocalDefaultTeamID)
	if err != nil {
		return loadFail(fmt.Errorf("read jira_project_status_rules: %w", err))
	}
	cfg.Jira.Projects = projects

	return cfg, nil
}

// loadFail centralizes the "log + return Default() + wrap error"
// pattern used by every error branch in Load(). The log line
// guarantees callers that drop the error (cfg, _ := config.Load())
// still surface the failure in the process log instead of silently
// substituting defaults.
func loadFail(err error) (Config, error) {
	log.Printf("[config] load failed, degrading to Default(): %v", err)
	return Default(), err
}

func loadJiraProjectConfigs(ctx context.Context, db *sql.DB, teamID string) ([]JiraProjectConfig, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT project_key,
		       pickup_members, in_progress_members, in_progress_canonical,
		       done_members, done_canonical
		FROM jira_project_status_rules
		WHERE team_id = ?
		ORDER BY project_key
	`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []JiraProjectConfig
	for rows.Next() {
		var key, pickupJSON, inProgressJSON, doneJSON string
		var inProgressCanon, doneCanon sql.NullString
		if err := rows.Scan(&key, &pickupJSON, &inProgressJSON, &inProgressCanon, &doneJSON, &doneCanon); err != nil {
			return nil, err
		}
		p := JiraProjectConfig{Key: key}
		if err := json.Unmarshal([]byte(pickupJSON), &p.Pickup.Members); err != nil {
			return nil, fmt.Errorf("unmarshal pickup_members for %s: %w", key, err)
		}
		if err := json.Unmarshal([]byte(inProgressJSON), &p.InProgress.Members); err != nil {
			return nil, fmt.Errorf("unmarshal in_progress_members for %s: %w", key, err)
		}
		if err := json.Unmarshal([]byte(doneJSON), &p.Done.Members); err != nil {
			return nil, fmt.Errorf("unmarshal done_members for %s: %w", key, err)
		}
		if inProgressCanon.Valid {
			p.InProgress.Canonical = inProgressCanon.String
		}
		if doneCanon.Valid {
			p.Done.Canonical = doneCanon.String
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ErrInvalidConfig is returned by Save when the supplied Config
// fails the minimum-shape validation. Today this guards the zero-
// value foot-gun: Save(Config{}) would otherwise silently write
// port=0 + takeover_dir=” to instance_config, clobbering any
// existing row.
var ErrInvalidConfig = errors.New("config: invalid Config — fields must be populated (use Load() or Default() as a base, then mutate)")

// Save upserts the five settings tables for the local sentinels.
// Jira project rules are materialized one row per project in
// cfg.Jira.Projects, each carrying its own Pickup/InProgress/Done.
// Rules for projects no longer in the list have their rows deleted
// so the table stays clean.
//
// Required contract: cfg must come from Load() or Default(), then
// be mutated — building a Config struct from scratch and calling
// Save will fail validation (ErrInvalidConfig). The zero-value
// check on Server.Port is a footgun guard: Server.Port=0 would
// otherwise overwrite the persisted port with 0, and there's no
// meaningful way for a caller to express "leave port alone" with
// the current API.
func Save(cfg Config) error {
	if cfg.Server.Port <= 0 {
		return fmt.Errorf("%w: Server.Port=%d (must be > 0; build the Config from Load() or Default())",
			ErrInvalidConfig, cfg.Server.Port)
	}

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

	// team_settings — jira_projects is a denormalized fast path for
	// "which projects to poll" without joining; jira_project_status_rules
	// is the source of truth. Keep them in sync on every save.
	keys := cfg.Jira.ProjectKeys()
	if keys == nil {
		keys = []string{}
	}
	projectsJSON, err := json.Marshal(keys)
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

	// jira_project_status_rules — upsert one row per project, then
	// drop rows for projects no longer in the list. Per-project values
	// are persisted independently so two projects with different
	// workflows can coexist on the same team.
	for _, p := range cfg.Jira.Projects {
		if p.Key == "" {
			continue
		}
		pickupJSON, err := json.Marshal(orEmpty(p.Pickup.Members))
		if err != nil {
			return fmt.Errorf("marshal pickup_members for %s: %w", p.Key, err)
		}
		inProgressJSON, err := json.Marshal(orEmpty(p.InProgress.Members))
		if err != nil {
			return fmt.Errorf("marshal in_progress_members for %s: %w", p.Key, err)
		}
		doneJSON, err := json.Marshal(orEmpty(p.Done.Members))
		if err != nil {
			return fmt.Errorf("marshal done_members for %s: %w", p.Key, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO jira_project_status_rules (
				team_id, project_key,
				pickup_members, in_progress_members, in_progress_canonical,
				done_members, done_canonical, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(team_id, project_key) DO UPDATE SET
				pickup_members = excluded.pickup_members,
				in_progress_members = excluded.in_progress_members,
				in_progress_canonical = excluded.in_progress_canonical,
				done_members = excluded.done_members,
				done_canonical = excluded.done_canonical,
				updated_at = CURRENT_TIMESTAMP
		`,
			runmode.LocalDefaultTeamID, p.Key,
			string(pickupJSON), string(inProgressJSON), nullIfEmpty(p.InProgress.Canonical),
			string(doneJSON), nullIfEmpty(p.Done.Canonical),
		); err != nil {
			return fmt.Errorf("upsert jira_project_status_rules[%s]: %w", p.Key, err)
		}
	}

	// Delete rows for projects no longer in the config. Build the
	// placeholder list dynamically — SQLite has no array binding.
	if len(keys) == 0 {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM jira_project_status_rules WHERE team_id = ?`,
			runmode.LocalDefaultTeamID,
		); err != nil {
			return fmt.Errorf("clear jira_project_status_rules: %w", err)
		}
	} else {
		placeholders := make([]string, len(keys))
		args := make([]any, 0, len(keys)+1)
		args = append(args, runmode.LocalDefaultTeamID)
		for i, k := range keys {
			placeholders[i] = "?"
			args = append(args, k)
		}
		query := fmt.Sprintf(
			`DELETE FROM jira_project_status_rules WHERE team_id = ? AND project_key NOT IN (%s)`,
			joinPlaceholders(placeholders),
		)
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("prune jira_project_status_rules: %w", err)
		}
	}

	return tx.Commit()
}

// joinPlaceholders concatenates "?" placeholders with commas. Inlined
// rather than pulled from strings to avoid the import for one call.
func joinPlaceholders(p []string) string {
	out := ""
	for i, s := range p {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
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
