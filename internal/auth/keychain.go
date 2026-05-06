package auth

import (
	"fmt"
	"log"
	"os"
	"sort"
	"sync"

	"github.com/zalando/go-keyring"
)

const service = "triagefactory"

// Keychain keys
const (
	keyGitHubURL       = "github_url"
	keyGitHubPAT       = "github_pat"
	keyGitHubUsername  = "github_username"
	keyJiraURL         = "jira_url"
	keyJiraPAT         = "jira_pat"
	keyJiraDisplayName = "jira_display_name"
)

// Environment variable names (TRIAGE_FACTORY_ prefix matches existing convention).
var envKeys = map[string]string{
	keyGitHubURL:       "TRIAGE_FACTORY_GITHUB_URL",
	keyGitHubPAT:       "TRIAGE_FACTORY_GITHUB_PAT",
	keyGitHubUsername:  "TRIAGE_FACTORY_GITHUB_USERNAME",
	keyJiraURL:         "TRIAGE_FACTORY_JIRA_URL",
	keyJiraPAT:         "TRIAGE_FACTORY_JIRA_PAT",
	keyJiraDisplayName: "TRIAGE_FACTORY_JIRA_DISPLAY_NAME",
}

// Credentials holds the stored auth configuration.
type Credentials struct {
	GitHubURL       string
	GitHubPAT       string
	GitHubUsername  string
	JiraURL         string
	JiraPAT         string
	JiraDisplayName string
}

// Store saves all credentials to the OS keychain.
// If the keychain backend is unavailable and env vars supply at least one PAT,
// the error is logged and suppressed; otherwise it is returned so the caller
// knows credentials were not persisted.
func Store(creds Credentials) error {
	if !probeKeychain() {
		if len(EnvProvided()) > 0 {
			return nil
		}
		return fmt.Errorf("keychain backend unavailable and no TRIAGE_FACTORY_*_PAT env vars set")
	}
	pairs := []struct{ key, val string }{
		{keyGitHubURL, creds.GitHubURL},
		{keyGitHubPAT, creds.GitHubPAT},
		{keyGitHubUsername, creds.GitHubUsername},
		{keyJiraURL, creds.JiraURL},
		{keyJiraPAT, creds.JiraPAT},
		{keyJiraDisplayName, creds.JiraDisplayName},
	}
	for _, p := range pairs {
		if p.val == "" {
			continue
		}
		if err := keyring.Set(service, p.key, p.val); err != nil {
			return fmt.Errorf("keychain store %s: %w", p.key, err)
		}
	}
	return nil
}

// Load retrieves credentials from the OS keychain, then overlays any
// TRIAGE_FACTORY_* environment variables on top (env wins if set).
// If the keychain is unavailable but env vars supply at least one PAT,
// the keychain error is suppressed.
func Load() (Credentials, error) {
	creds, keychainErr := loadFromKeychain()

	anyEnv := false
	overlay := func(key string, dst *string) {
		if v := os.Getenv(envKeys[key]); v != "" {
			*dst = v
			anyEnv = true
		}
	}
	overlay(keyGitHubURL, &creds.GitHubURL)
	overlay(keyGitHubPAT, &creds.GitHubPAT)
	overlay(keyGitHubUsername, &creds.GitHubUsername)
	overlay(keyJiraURL, &creds.JiraURL)
	overlay(keyJiraPAT, &creds.JiraPAT)
	overlay(keyJiraDisplayName, &creds.JiraDisplayName)

	if anyEnv {
		logEnvOnce()
	}

	if keychainErr != nil && (creds.GitHubPAT != "" || creds.JiraPAT != "") {
		return creds, nil
	}
	return creds, keychainErr
}

func loadFromKeychain() (Credentials, error) {
	var creds Credentials
	var err error

	creds.GitHubURL, err = get(keyGitHubURL)
	if err != nil {
		return creds, err
	}
	creds.GitHubPAT, err = get(keyGitHubPAT)
	if err != nil {
		return creds, err
	}
	creds.GitHubUsername, err = get(keyGitHubUsername)
	if err != nil {
		return creds, err
	}
	creds.JiraURL, err = get(keyJiraURL)
	if err != nil {
		return creds, err
	}
	creds.JiraPAT, err = get(keyJiraPAT)
	if err != nil {
		return creds, err
	}
	creds.JiraDisplayName, err = get(keyJiraDisplayName)
	if err != nil {
		return creds, err
	}

	return creds, nil
}

func deleteKeys(keys ...string) error {
	if !probeKeychain() {
		return nil
	}
	for _, key := range keys {
		if err := keyring.Delete(service, key); err != nil && err != keyring.ErrNotFound {
			return fmt.Errorf("keychain delete %s: %w", key, err)
		}
	}
	return nil
}

// Clear removes all credentials from the OS keychain.
func Clear() error {
	return deleteKeys(keyGitHubURL, keyGitHubPAT, keyGitHubUsername, keyJiraURL, keyJiraPAT, keyJiraDisplayName)
}

// ClearGitHub removes GitHub credentials from the OS keychain.
func ClearGitHub() error {
	return deleteKeys(keyGitHubURL, keyGitHubPAT, keyGitHubUsername)
}

// ClearJira removes Jira credentials from the OS keychain.
func ClearJira() error {
	return deleteKeys(keyJiraURL, keyJiraPAT, keyJiraDisplayName)
}

// IsConfigured returns true if at least one PAT is available (from keychain or env vars).
func IsConfigured() bool {
	creds, err := Load()
	if err != nil {
		return false
	}
	return creds.GitHubPAT != "" || creds.JiraPAT != ""
}

// EnvProvided returns which credential groups have values supplied by
// environment variables: "github" if URL+PAT are set, "jira" likewise.
func EnvProvided() []string {
	var out []string
	if os.Getenv(envKeys[keyGitHubURL]) != "" && os.Getenv(envKeys[keyGitHubPAT]) != "" {
		out = append(out, "github")
	}
	if os.Getenv(envKeys[keyJiraURL]) != "" && os.Getenv(envKeys[keyJiraPAT]) != "" {
		out = append(out, "jira")
	}
	return out
}

// get retrieves a value from the keychain, returning empty string if not found.
func get(key string) (string, error) {
	val, err := keyring.Get(service, key)
	if err == keyring.ErrNotFound {
		return "", nil
	}
	return val, err
}

// --- env var helpers ---

var envLogOnce sync.Once

func logEnvOnce() {
	envLogOnce.Do(func() {
		var names []string
		for _, envName := range envKeys {
			if os.Getenv(envName) != "" {
				names = append(names, envName)
			}
		}
		sort.Strings(names)
		log.Printf("[auth] credentials provided via environment: %v", names)
	})
}

// --- keychain availability probe ---

var (
	keychainProbeOnce sync.Once
	keychainOK        bool
)

func probeKeychain() bool {
	keychainProbeOnce.Do(func() {
		_, err := keyring.Get(service, "__probe__")
		keychainOK = err == nil || err == keyring.ErrNotFound
		if !keychainOK {
			log.Printf("[auth] keychain backend unavailable: %v", err)
		}
	})
	return keychainOK
}
