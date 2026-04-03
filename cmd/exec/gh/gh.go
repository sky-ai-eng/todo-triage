package gh

import (
	"fmt"
	"os"

	"github.com/sky-ai-eng/todo-tinder/internal/db"
	"github.com/sky-ai-eng/todo-tinder/internal/github"
)

// Handle dispatches gh subcommands.
func Handle(client *github.Client, database *db.DB, args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: todotinder exec gh <resource> <action> [flags]")
		os.Exit(1)
	}

	resource := args[0]
	cmdArgs := args[1:]

	switch resource {
	case "pr":
		handlePR(client, database, cmdArgs)
	default:
		fmt.Fprintf(os.Stderr, "unknown gh resource: %s\n", resource)
		os.Exit(1)
	}
}
