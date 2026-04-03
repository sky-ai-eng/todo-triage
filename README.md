# Todo Tinder

A local-first developer productivity tool that aggregates action items from GitHub and Jira into an AI-prioritized triage queue. Swipe through cards to claim, dismiss, snooze, or delegate tasks to a Claude Code agent.

## How it works

Todo Tinder runs as a single Go binary on your machine. It polls GitHub and Jira for items that need your attention, scores them with AI, and presents them in a Tinder-style swipe interface. Tasks you delegate get sent to a headless Claude Code agent that performs PR reviews autonomously, streaming results back in real time.

- **Triage** -- Swipe cards left (dismiss), right (claim), up (delegate to AI), or down (snooze)
- **Board** -- Kanban view with drag-and-drop between Queue, In Progress, and Done columns
- **Delegation** -- Agent-delegated tasks run in isolated git worktrees with live activity streaming

Credentials are stored in your OS keychain. Everything runs locally -- nothing leaves your machine except GitHub/Jira API calls and Claude API requests.

## Prerequisites

- [Go](https://go.dev/) 1.23+
- [Node.js](https://nodejs.org/) 20+
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) (for AI scoring and delegation)

## Quick start

```bash
# Clone the repository
git clone https://github.com/sky-ai-eng/todo-tinder.git
cd todo-tinder

# Build the frontend
cd frontend && npm install && npm run build && cd ..

# Build the binary
go build -o ./todotinder .

# Run (opens browser automatically)
./todotinder
```

On first launch, you'll be prompted to connect your GitHub and/or Jira accounts.

## Usage

```bash
# Start the server (default port 3000)
./todotinder

# Custom port, no auto-open browser
./todotinder --port 8080 --no-browser
```

### CLI subcommands

The binary also exposes subcommands used internally by the delegation agent:

```bash
# Execute GitHub commands in the context of a delegated run
./todotinder exec gh pr view --owner sky-ai-eng --repo myrepo --number 42

# Check agent run status
./todotinder status <run-id>
```

## Architecture

```
todotinder (Go binary)
  |-- Embedded React SPA (frontend/dist)
  |-- SQLite database (~/.todotinder/todotinder.db)
  |-- OS Keychain (credentials)
  |-- GitHub/Jira pollers (background goroutines)
  |-- AI scorer (Claude Haiku, background)
  |-- Delegation spawner (Claude Code CLI, per-task)
  `-- WebSocket hub (real-time streaming)
```

### Project structure

```
main.go                    -- Entrypoint, server/CLI dispatch
internal/
  domain/                  -- Shared types (Task, AgentRun, etc.)
  db/                      -- SQLite schema, queries
  server/                  -- HTTP handlers, routes
  auth/                    -- OS keychain storage, credential validation
  config/                  -- YAML config (~/.todotinder/config.yaml)
  ai/                      -- AI scoring pipeline, prompt templates
  delegate/                -- Agent process spawner, NDJSON stream parser
  github/                  -- GitHub REST API client
  poller/                  -- GitHub/Jira polling goroutines
  worktree/                -- Git bare clone + worktree management
cmd/exec/                  -- CLI commands for delegated agents
pkg/websocket/             -- WebSocket hub for real-time updates
frontend/                  -- React + Tailwind + Vite SPA
```

## Configuration

Config lives at `~/.todotinder/config.yaml` and can be edited via the Settings page or directly:

```yaml
github:
  base_url: "https://github.com"
  poll_interval: 1m
jira:
  base_url: "https://jira.yourcompany.com"
  poll_interval: 2m
  projects: [PROJ, INFRA]
ai:
  model: sonnet
server:
  port: 3000
```

## GitHub polling

The poller fetches three categories of open PRs:

1. **Review requested** -- PRs where your review is pending
2. **Authored** -- Your own open PRs (with CI status from check-runs API)
3. **Mentioned** -- PRs where you were @mentioned

All queries filter to `state:open` only. CI status is tracked for authored PRs so failing builds surface higher in triage.

## License

[Business Source License 1.1](LICENSE) -- free for internal use, converts to Apache 2.0 on 2030-03-31. See [CONTRIBUTING.md](CONTRIBUTING.md) for contribution terms.
