# Usage

## Prerequisites

- [Go](https://go.dev/) 1.23+
- [Node.js](https://nodejs.org/) 20+
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) (for AI scoring and delegation)

## Running

```bash
# Default (port 3000, opens browser)
./triagefactory

# Custom port, no browser
./triagefactory --port 8080 --no-browser

# Top-level help — points humans at the user commands and agents at exec.
./triagefactory --help
```

## CLI subcommands

The binary's subcommands fall into two audiences. Run `./triagefactory --help` for a one-screen summary.

### User commands

These are meant for you to invoke from a terminal.

```bash
# Symlink the binary onto PATH so `triagefactory resume` works from
# any directory. Defaults: /usr/local/bin on macOS, ~/.local/bin on Linux.
# Override with --dest /full/path/to/triagefactory.
./triagefactory install

# Resume a Claude Code session you previously took over via the
# "Take over" button on a delegated run's card.
#
#   bare:                auto-resumes when there's exactly one
#                        taken-over session; picker on stdin otherwise.
#   <short-id> arg:      disambiguate by run-ID prefix (the 8-char id
#                        from the takeover modal).
#
# `triagefactory resume` cd's to the takeover working tree and execs
# `claude --resume <session-id>`, replacing the current process so
# your terminal becomes the interactive Claude Code session directly.
triagefactory resume
triagefactory resume abc12345
```

The takeover working trees live under `~/.triagefactory/takeovers/run-<id>/` and persist across server restarts. `scripts/clean-slate.sh` wipes them when resetting local state.

### Agent commands

These are invoked by delegated Claude Code agents inside their worktree, not by you. Documented for completeness.

```bash
# Scoped GitHub commands the agent uses instead of touching the
# GitHub API directly — credentials stay in the OS keychain and
# every call is logged to run_artifacts.
./triagefactory exec gh pr view --owner sky-ai-eng --repo myrepo --number 42

# Scoped Jira commands, same shape.
./triagefactory exec jira issue view SKY-194

# Check a delegated run's status (used by the agent's lifecycle hooks).
./triagefactory status <run-id>
```

Run `./triagefactory exec --help` for the full subcommand list.

## Configuration

Config lives at `~/.triagefactory/config.yaml` and can be edited via the Settings page or directly:

```yaml
github:
  base_url: "https://github.com"
  poll_interval: 1m

jira:
  base_url: "https://jira.yourcompany.com"
  poll_interval: 2m
  projects: [PROJ, INFRA]
  pickup_statuses: [Open, Ready for Development]
  in_progress_status: "In Progress"

ai:
  model: sonnet

server:
  port: 3000
```

### Jira setup

Jira uses a two-stage flow in Settings:

1. Enter your Jira URL and Personal Access Token, click **Connect**. Credentials are validated and stored immediately.
2. The card expands to reveal project selection, poll interval, and status configuration. Statuses are fetched automatically from your Jira instance.
3. **Save** is disabled until you've configured projects, pickup statuses, and an in-progress status.

### Credentials

All credentials (GitHub PAT, Jira PAT) are stored in your OS keychain, never on disk. Token fields in Settings show "leave blank to keep current" when a token is already stored.

## GitHub polling

The poller tracks PRs across several categories:

- **Review requested** — PRs where your review is pending
- **Authored** — Your open PRs, including CI status from the check-runs API
- **Mentioned** — PRs where you were @mentioned
- **Reviewed** — PRs you've previously reviewed (tracks for follow-up)
- **Merged / Closed** — Terminal PRs tracked for dashboard statistics

All discovery queries filter to recent activity. The tracker diffs snapshots on each poll cycle and emits typed events only on state transitions — see [tracked-events.md](tracked-events.md) for the full event taxonomy.

## Repo profiling

Configured repos are automatically profiled on first run using Claude Haiku. The profiler fetches README.md, CLAUDE.md, and AGENTS.md from each repo and generates a summary used by the AI scorer and delegation agents.

Profiles are cached for 3 days. The **Re-profile** button on the Repos page forces an immediate refresh regardless of TTL.