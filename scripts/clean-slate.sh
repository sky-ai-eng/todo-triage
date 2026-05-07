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

# Resolve server.takeover_dir from the settings YAML blob BEFORE wiping
# the DB. Default is ~/.triagefactory/takeovers, but a user can redirect
# it (cmd/uninstall reads the same setting via cfg.Server.ResolvedTakeoverDir).
# omitempty means an absent or empty key falls back to the default.
takeover_dir="$HOME/.triagefactory/takeovers"
if command -v sqlite3 >/dev/null 2>&1 && [ -f ~/.triagefactory/triagefactory.db ]; then
  yaml_blob=$(sqlite3 ~/.triagefactory/triagefactory.db "SELECT data FROM settings WHERE id=1" 2>/dev/null || true)
  if [ -n "$yaml_blob" ]; then
    custom=$(printf '%s\n' "$yaml_blob" | sed -n 's/^[[:space:]]*takeover_dir:[[:space:]]*//p' | head -n 1 | tr -d '"' | tr -d "'")
    if [ -n "$custom" ]; then
      case "$custom" in
        "~/"*)
          takeover_dir="$HOME/${custom#~/}"
          ;;
        /*)
          takeover_dir="$custom"
          ;;
      esac
    fi
  fi
fi

# Database
rm -f ~/.triagefactory/triagefactory.db ~/.triagefactory/triagefactory.db-wal ~/.triagefactory/triagefactory.db-shm
echo "  removed database"

# Config (settings now live in the DB above; this only removes a stale
# pre-DB config.yaml left behind by ancient installs).
if [ -f ~/.triagefactory/config.yaml ]; then
  rm -f ~/.triagefactory/config.yaml
  echo "  removed legacy config.yaml"
fi

# Project knowledge dirs — the Curator materializes per-project repo
# worktrees at ~/.triagefactory/projects/<id>/repos/<owner>/<repo>/
# (and writes knowledge/summary files alongside). Project rows just
# got wiped with the database, so the disk state is orphaned. Worse:
# each repo subdir is a registered worktree of the bare clone in
# ~/.triagefactory/repos/, holding its branch as "checked out." A
# subsequent run that tries to `git fetch` that branch (e.g. the
# delegate path's `workspace add`) fails with "refusing to fetch into
# branch ... checked out at <stale path>" until the registrations
# get pruned. Wiping projects/ now and re-pruning each bare's
# worktrees/ tracker below closes that loop.
if [ -d ~/.triagefactory/projects ]; then
  # The Curator runs Claude Code with cwd =
  # ~/.triagefactory/projects/<id>/, which makes Claude Code create
  # ~/.claude/projects/<encoded(<cwd>)>/<sessionID>.jsonl. Walk each
  # project ID dir and delete its encoded session entry BEFORE removing
  # the projects tree (mirrors removeClaudeProjectsForCurator in
  # cmd/uninstall/uninstall.go — keep the two in sync).
  for dir in ~/.triagefactory/projects/*; do
    [ -d "$dir" ] || continue
    resolved=$(cd "$dir" && pwd -P) || continue
    encoded=$(printf '%s' "$resolved" | tr '/.' '-')
    rm -rf ~/.claude/projects/"$encoded"
  done
  rm -rf ~/.triagefactory/projects
  echo "  removed projects dir and any curator session JSONLs"
fi

# Prune stale worktree registrations from every preserved bare. The
# bare clones themselves (~/.triagefactory/repos/) stay — they're
# expensive to refetch and not part of the first-run flow — but their
# internal worktrees/ tracker now points at directories we just
# deleted (projects/, takeovers/, /tmp/triagefactory-runs/). Pruning
# is idempotent and cheap. Without this, the next `git worktree add`
# / `git fetch` against any of these bares would hit the stale-
# registration errors described in the projects-dir comment above.
if [ -d ~/.triagefactory/repos ]; then
  pruned=0
  while IFS= read -r bare; do
    git -C "$bare" worktree prune 2>/dev/null || true
    pruned=$((pruned + 1))
  done < <(find ~/.triagefactory/repos -type d -name '*.git' 2>/dev/null)
  if [ "$pruned" -gt 0 ]; then
    echo "  pruned worktrees from $pruned bare clone(s)"
  fi
fi

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
if [ -d "$takeover_dir" ]; then
  for dir in "$takeover_dir"/run-*; do
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
  rm -rf "$takeover_dir"
  echo "  removed takeovers and their session JSONLs ($takeover_dir)"
fi

# Keychain — keep this list in sync with auth.Clear() in
# internal/auth/keychain.go (the canonical list, used by `triagefactory
# uninstall`). Drift between the two means stale entries linger after
# clean-slate; jira_display_name was the most recent miss.
for key in github_url github_pat github_username jira_url jira_pat jira_display_name; do
  security delete-generic-password -s triagefactory -a "$key" 2>/dev/null && echo "  removed keychain: $key" || true
done

echo "Done. Restart the server for a fresh setup."
