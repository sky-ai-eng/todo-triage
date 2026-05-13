// Package config holds the user-editable settings struct and persists
// it to a singleton row in the SQLite DB (~/.triagefactory/triagefactory.db,
// table `settings`). Init must be called once at startup with an open
// DB handle before any Load/Save call.
package config

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the persisted settings shape. YAML tags are retained for
// the blob serialization stored in the settings row.
type Config struct {
	GitHub GitHubConfig `yaml:"github"`
	Jira   JiraConfig   `yaml:"jira"`
	Server ServerConfig `yaml:"server"`
	AI     AIConfig     `yaml:"ai"`
}

type GitHubConfig struct {
	BaseURL      string        `yaml:"base_url"`
	PollInterval time.Duration `yaml:"poll_interval"`
	// CloneProtocol controls how bare clones in ~/.triagefactory/repos/
	// are created. "ssh" uses git@github.com:owner/repo.git, "https"
	// uses GitHub's clone_url (depends on a credential helper holding
	// a PAT). Empty string is treated as "https" by callers.
	CloneProtocol string `yaml:"clone_protocol,omitempty"`
}

type JiraConfig struct {
	BaseURL      string         `yaml:"base_url"`
	PollInterval time.Duration  `yaml:"poll_interval"`
	Projects     []string       `yaml:"projects"`
	Pickup       JiraStatusRule `yaml:"pickup"`
	InProgress   JiraStatusRule `yaml:"in_progress"`
	Done         JiraStatusRule `yaml:"done"`
}

// JiraStatusRule captures the two questions a user answers about a Jira state:
// which statuses count as this state (Members) and which status TF writes when
// it transitions into it (Canonical). Canonical is empty for Pickup since TF
// never writes tickets back to the pickup state — it only reads them.
type JiraStatusRule struct {
	Members   []string `yaml:"members"             json:"members"`
	Canonical string   `yaml:"canonical,omitempty" json:"canonical,omitempty"`
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
	Port int `yaml:"port"`
	// TakeoverDir is where the takeover endpoint clones run worktrees so
	// the user can resume the headless Claude Code session interactively.
	// Lives outside $TMPDIR so the worktree-cleanup safety rail leaves it
	// alone. A leading "~" is expanded against the user's home dir at use
	// time. Empty means "use the default" (~/.triagefactory/takeovers).
	TakeoverDir string `yaml:"takeover_dir,omitempty"`
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
	Model                    string `yaml:"model"`
	ReprioritizeThreshold    int    `yaml:"reprioritize_threshold"`
	PreferenceUpdateInterval int    `yaml:"preference_update_interval"`
	AutoDelegateEnabled      bool   `yaml:"auto_delegate_enabled"`
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
// Load() and write to the settings row on first Save().
func Init(db *sql.DB) error {
	if db == nil {
		return errors.New("config.Init: nil db")
	}
	pkgMu.Lock()
	pkgDB = db
	pkgMu.Unlock()
	return nil
}

// Load reads the settings row, falling back to Default() when no row
// exists yet. Unmarshal errors return the partially-populated struct
// alongside the error so callers can degrade gracefully if they want.
func Load() (Config, error) {
	pkgMu.RLock()
	db := pkgDB
	pkgMu.RUnlock()
	if db == nil {
		return Default(), ErrNotInitialized
	}

	var blob string
	err := db.QueryRow(`SELECT data FROM settings WHERE id = 1`).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return Default(), nil
	}
	if err != nil {
		return Default(), fmt.Errorf("read settings: %w", err)
	}

	cfg := Default()
	if err := yaml.Unmarshal([]byte(blob), &cfg); err != nil {
		return cfg, fmt.Errorf("unmarshal settings: %w", err)
	}
	return cfg, nil
}

// Save upserts the settings row.
func Save(cfg Config) error {
	pkgMu.RLock()
	db := pkgDB
	pkgMu.RUnlock()
	if db == nil {
		return ErrNotInitialized
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = db.Exec(
		`INSERT INTO settings (id, data, updated_at) VALUES (1, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(id) DO UPDATE SET data = excluded.data, updated_at = CURRENT_TIMESTAMP`,
		string(out),
	)
	return err
}
