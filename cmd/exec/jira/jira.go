package jira

import (
	"fmt"
	"os"

	jiraclient "github.com/sky-ai-eng/todo-tinder/internal/jira"
)

// Handle dispatches jira subcommands.
func Handle(client *jiraclient.Client, args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: todotinder exec jira <resource> <action> [flags]")
		os.Exit(1)
	}

	resource := args[0]
	cmdArgs := args[1:]

	switch resource {
	case "ticket":
		handleTicket(client, cmdArgs)
	default:
		fmt.Fprintf(os.Stderr, "unknown jira resource: %s\n", resource)
		os.Exit(1)
	}
}
