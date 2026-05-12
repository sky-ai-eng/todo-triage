package exec

import (
	"fmt"
	"os"

	"github.com/pressly/goose/v3"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/gh"
	jiraexec "github.com/sky-ai-eng/triage-factory/cmd/exec/jira"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/workspace"
	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
)

// Handle dispatches exec subcommands.
func Handle(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printHelp()
		return
	}

	// Load credentials for API access
	creds, err := auth.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading credentials: %v\n", err)
		os.Exit(1)
	}

	// Open DB for local state (pending reviews, etc.). Config now lives
	// in a settings row, so config.Load() requires an initialized DB —
	// open + migrate before calling Init/Load.
	conn, err := db.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error opening database: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()
	// Silence goose's per-invocation logging ("no migrations to run…")
	// — exec runs on every delegated-agent tool call and the noise
	// drowns out the actual command output. Migration errors still
	// surface via the returned error.
	goose.SetLogger(goose.NopLogger())
	if err := db.Migrate(conn, "sqlite3"); err != nil {
		fmt.Fprintf(os.Stderr, "error running migrations: %v\n", err)
		os.Exit(1)
	}
	if err := config.Init(conn); err != nil {
		fmt.Fprintf(os.Stderr, "error initializing config: %v\n", err)
		os.Exit(1)
	}
	if err := config.MigrateLegacyYAML(conn); err != nil {
		fmt.Fprintf(os.Stderr, "warning: legacy config import: %v\n", err)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: loading config: %v (proceeding with defaults)\n", err)
	}
	database := &db.DB{Conn: conn}

	cmd := args[0]
	cmdArgs := args[1:]

	switch cmd {
	case "gh":
		if isHelp(cmdArgs) {
			gh.Handle(nil, nil, cmdArgs)
			return
		}
		if creds.GitHubPAT == "" {
			fmt.Fprintln(os.Stderr, "GitHub not configured. Run triagefactory and complete setup first.")
			os.Exit(1)
		}
		baseURL := cfg.GitHub.BaseURL
		if baseURL == "" {
			baseURL = creds.GitHubURL
		}
		client := ghclient.NewClient(baseURL, creds.GitHubPAT)
		gh.Handle(client, database, cmdArgs)

	case "jira":
		if isHelp(cmdArgs) {
			jiraexec.Handle(nil, cmdArgs)
			return
		}
		if creds.JiraPAT == "" || creds.JiraURL == "" {
			fmt.Fprintln(os.Stderr, "Jira not configured. Run triagefactory and complete setup first.")
			os.Exit(1)
		}
		jClient := jiraclient.NewClient(creds.JiraURL, creds.JiraPAT)
		jiraexec.Handle(jClient, cmdArgs)

	case "workspace":
		// No credentials needed — workspace acts on local DB + filesystem
		// only. The agent's run identity flows through TRIAGE_FACTORY_RUN_ID,
		// validated inside the subcommand.
		workspace.Handle(database, cmdArgs)

	default:
		fmt.Fprintf(os.Stderr, "unknown exec command: %s\nRun 'triagefactory exec --help' for usage.\n", cmd)
		os.Exit(1)
	}
}

// HandleStatus processes status update commands from the agent.
func HandleStatus(args []string) {
	fmt.Fprintln(os.Stderr, "not implemented: status")
}

func isHelp(args []string) bool {
	return len(args) == 0 || args[0] == "--help" || args[0] == "-h"
}

func printHelp() {
	fmt.Printf("Usage: triagefactory exec <command> [args]\n\n%s\n\n%s\n\n%s\n\nCommands print their result to stdout on success and errors to stderr. Most commands print JSON; workspace add prints a raw path.\n", gh.HelpText, jiraexec.HelpText, workspace.HelpText)
}
