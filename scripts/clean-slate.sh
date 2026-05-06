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
  rm -rf ~/.triagefactory/projects
  echo "  removed projects dir"
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
