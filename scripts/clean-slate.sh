#!/usr/bin/env bash
# Wipe all local state: database, config, keychain credentials, and
# any in-progress takeover dirs + their session JSONLs. Use this to
# test the first-run experience from scratch.
#
# Bare repo clones under ~/.triagefactory/repos/ are deliberately
# preserved — they're expensive to re-fetch and aren't part of the
# first-run flow this script targets. Wipe them manually if you need
# to.

set -euo pipefail

echo "Cleaning Triage Factory local state..."

# Database
rm -f ~/.triagefactory/triagefactory.db ~/.triagefactory/triagefactory.db-wal ~/.triagefactory/triagefactory.db-shm
echo "  removed database"

# Config
rm -f ~/.triagefactory/config.yaml
echo "  removed config"

# Takeovers — interactive-resume working copies created by the
# "Take over" flow. After a DB wipe their corresponding run rows are
# gone, so the dirs are orphaned. We also wipe each takeover's
# entry under ~/.claude/projects/ (Claude Code's per-cwd session
# storage; we materialized the session JSONL there during takeover).
# Enumerate takeover dirs BEFORE removing them so we can compute the
# encoded project-dir name from each absolute path. Encoding rule
# matches Claude Code's: replace every '/' AND every '.' with '-' in
# the symlink-resolved absolute path. The dot replacement is the
# easy-to-miss part — paths like ~/.triagefactory/... contain dots
# and slash-only encoding would silently miss the project dir Claude
# Code actually uses. See encodeClaudeProjectDir in
# internal/worktree/worktree.go for the full story.
if [ -d ~/.triagefactory/takeovers ]; then
  for dir in ~/.triagefactory/takeovers/run-*; do
    [ -d "$dir" ] || continue
    # `cd && pwd -P` is the POSIX-portable way to get the symlink-
    # resolved path; `realpath` isn't on default macOS.
    resolved=$(cd "$dir" && pwd -P) || continue
    # Claude Code encoding: every '/' AND every '.' becomes '-'.
    # Slash-only is wrong for paths like ~/.triagefactory/...
    # because the dot in `.triagefactory` is collapsed by Claude
    # too — see the comment on encodeClaudeProjectDir in
    # internal/worktree/worktree.go for the full story.
    encoded=$(printf '%s' "$resolved" | tr '/.' '-')
    rm -rf ~/.claude/projects/"$encoded"
  done
  rm -rf ~/.triagefactory/takeovers
  echo "  removed takeovers and their session JSONLs"
fi

# Keychain
for key in github_url github_pat github_username jira_url jira_pat; do
  security delete-generic-password -s triagefactory -a "$key" 2>/dev/null && echo "  removed keychain: $key" || true
done

echo "Done. Restart the server for a fresh setup."
