package config

import (
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents ~/.triagefactory/config.yaml.
type Config struct {
	GitHub GitHubConfig `yaml:"github"`
	Jira   JiraConfig   `yaml:"jira"`
	Server ServerConfig `yaml:"server"`
	AI     AIConfig     `yaml:"ai"`
}

type GitHubConfig struct {
	BaseURL      string        `yaml:"base_url"`
	PollInterval time.Duration `yaml:"poll_interval"`
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
			PollInterval: 60 * time.Second,
		},
		Jira: JiraConfig{
			PollInterval: 60 * time.Second,
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

// configPath returns the path to ~/.triagefactory/config.yaml.
func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".triagefactory", "config.yaml"), nil
}

// Load reads the config from disk, falling back to defaults for missing fields.
func Load() (Config, error) {
	cfg := Default()

	path, err := configPath()
	if err != nil {
		return cfg, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil // no config file yet, use defaults
		}
		return cfg, err
	}

	// Unmarshal on top of defaults — only overrides fields present in the file
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}

	return cfg, nil
}

// Save writes the config to disk, creating the directory if needed.
func Save(cfg Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}
