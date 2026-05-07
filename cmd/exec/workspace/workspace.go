// Package workspace implements the `triagefactory exec workspace` CLI
// surface — agent-callable commands for materializing per-repo worktrees
// inside a Jira delegation run.
//
// The flow: Jira delegations spawn the agent at the run-root (a throwaway
// dir holding only _scratch/entity-memory/), no codebase pre-cloned. The agent reads
// the ticket, decides which repo(s) it needs, and calls `workspace add
// <owner/repo>` to materialize a worktree. The CLI prints the absolute
// worktree path; the agent `cd`s in. `workspace list` returns the JSON
// inventory of worktrees materialized so far for diagnostics.
//
// GitHub PR delegations don't use this surface — their worktree is
// materialized eagerly by the spawner from the PR's owner/repo. The
// `add` command rejects when invoked under a GitHub PR run.
package workspace

import (
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// HelpText is the help block for `workspace` commands, surfaced both
// from `workspace --help` and the top-level `exec --help`.
const HelpText = `Workspace Commands:
  workspace list                 Print configured + materialized repos for this run (JSON)
  workspace add <owner/repo>     Materialize a per-repo worktree for this run; prints absolute path

Discovery:
  Run 'workspace list' first when you're not sure which repo a Jira
  ticket belongs to. Output has two sections:
    - "available": configured repos you could materialize via 'add'
    - "materialized": worktrees you've already added for this run, with
      absolute paths and feature branches

Usage notes:
  - Run id is read from $TRIAGE_FACTORY_RUN_ID (set by the delegation spawner).
  - Feature branch is derived from the task's Jira issue key: feature/<KEY>.
  - The first 'add' for a given repo creates the feature branch off the
    repo's configured base; subsequent 'add's for the same repo are
    idempotent and print the existing path.
  - 'add' is rejected for GitHub PR runs (their worktree is materialized
    eagerly from the PR's repo and cannot be replaced mid-run).
  - 'add' rejects unconfigured repos with a "not configured" error; use
    'list' to enumerate options before guessing.`

// Handle dispatches workspace subcommands.
func Handle(database *db.DB, args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printHelp()
		return
	}
	switch args[0] {
	case "add":
		runAdd(database, args[1:])
	case "list":
		runList(database, args[1:])
	default:
		exitErr("unknown workspace command: " + args[0])
	}
}

func printHelp() {
	fmt.Printf("Usage: triagefactory exec workspace <command> [args]\n\n%s\n", HelpText)
}

func exitErr(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
