package worktree

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Per-repo mutexes prevent concurrent fetches from racing on the same bare repo.
var (
	repoMu   sync.Mutex
	repoLocks = map[string]*sync.Mutex{}
)

func lockRepo(owner, repo string) *sync.Mutex {
	key := owner + "/" + repo
	repoMu.Lock()
	defer repoMu.Unlock()
	mu, ok := repoLocks[key]
	if !ok {
		mu = &sync.Mutex{}
		repoLocks[key] = mu
	}
	return mu
}

const (
	reposDir = ".todotinder/repos" // bare clones: ~/.todotinder/repos/{owner}/{repo}.git
	runsDir  = "todotinder-runs"   // worktrees: /tmp/todotinder-runs/{run-id}
)

func repoDir(owner, repo string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, reposDir, owner, repo+".git"), nil
}

func runDir(runID string) string {
	return filepath.Join(os.TempDir(), runsDir, runID)
}

// Create sets up an isolated worktree for an agent run.
// Ensures a bare clone exists, fetches the target PR ref, and creates a worktree at the given SHA.
// Returns the worktree path.
func Create(ctx context.Context, owner, repo, cloneURL, sha string, prNumber int, runID string) (string, error) {
	// Serialize git operations per repo to avoid lock conflicts
	mu := lockRepo(owner, repo)
	mu.Lock()
	defer mu.Unlock()

	bareDir, err := repoDir(owner, repo)
	if err != nil {
		return "", fmt.Errorf("resolve repo dir: %w", err)
	}

	// Bare clone on first use (treeless: skip blobs until checkout needs them)
	if _, err := os.Stat(bareDir); os.IsNotExist(err) {
		log.Printf("[worktree] cloning %s/%s (first time)...", owner, repo)
		if err := os.MkdirAll(filepath.Dir(bareDir), 0755); err != nil {
			return "", fmt.Errorf("mkdir: %w", err)
		}
		start := time.Now()
		if err := gitRunCtx(ctx, "", "clone", "--bare", "--filter=blob:none", cloneURL, bareDir); err != nil {
			return "", fmt.Errorf("bare clone: %w", err)
		}
		log.Printf("[worktree] clone %s/%s completed in %s", owner, repo, time.Since(start).Round(time.Millisecond))
	}

	// Only fetch the specific PR ref we need — not all refs
	prRef := fmt.Sprintf("+refs/pull/%d/head:refs/pull/%d/head", prNumber, prNumber)
	start := time.Now()
	if err := gitRunCtx(ctx, bareDir, "fetch", "origin", prRef); err != nil {
		return "", fmt.Errorf("fetch PR refs: %w", err)
	}
	log.Printf("[worktree] fetch PR #%d completed in %s", prNumber, time.Since(start).Round(time.Millisecond))

	// Create worktree at target SHA
	wtDir := runDir(runID)
	if err := os.MkdirAll(filepath.Dir(wtDir), 0755); err != nil {
		return "", fmt.Errorf("mkdir runs: %w", err)
	}

	if err := gitRunCtx(ctx, bareDir, "worktree", "add", "--detach", wtDir, sha); err != nil {
		return "", fmt.Errorf("worktree add: %w", err)
	}

	log.Printf("[worktree] created at %s (sha: %s)", wtDir, sha[:12])
	return wtDir, nil
}

// Remove cleans up a worktree after a run completes or fails.
func Remove(runID string) error {
	wtDir := runDir(runID)
	if err := os.RemoveAll(wtDir); err != nil {
		return fmt.Errorf("remove worktree dir: %w", err)
	}

	// Prune stale worktree refs from all bare repos
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	pruneAll(filepath.Join(home, reposDir))

	log.Printf("[worktree] removed %s", runID)
	return nil
}

// Cleanup removes all orphaned worktrees on startup and prunes bare repos.
func Cleanup() {
	runsBase := filepath.Join(os.TempDir(), runsDir)
	entries, err := os.ReadDir(runsBase)
	if err != nil {
		return // no runs dir, nothing to clean
	}

	count := 0
	for _, e := range entries {
		if e.IsDir() {
			os.RemoveAll(filepath.Join(runsBase, e.Name()))
			count++
		}
	}

	if count > 0 {
		log.Printf("[worktree] cleaned up %d orphaned worktrees", count)
	}

	// Prune all bare repos
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	pruneAll(filepath.Join(home, reposDir))
}

func pruneAll(baseDir string) {
	filepath.Walk(baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && strings.HasSuffix(path, ".git") {
			if err := gitRun(path, "worktree", "prune"); err != nil {
				log.Printf("[worktree] prune %s: %v", path, err)
			}
			return filepath.SkipDir
		}
		return nil
	})
}

func gitRunCtx(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("cancelled")
		}
		return fmt.Errorf("%s: %s", err, string(out))
	}
	return nil
}

func gitRun(dir string, args ...string) error {
	return gitRunCtx(context.Background(), dir, args...)
}
