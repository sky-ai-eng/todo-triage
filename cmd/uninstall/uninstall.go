// Package uninstall implements the `triagefactory uninstall` CLI
// subcommand: a one-shot wipe of all local state created by the
// running binary. Mirrors scripts/clean-slate.sh but ships inside the
// binary so users who installed via Homebrew (and therefore don't have
// the repo) still have a clean exit.
//
// What it removes:
//   - ~/.triagefactory/ in full (db, config, takeovers, bare repo clones)
//   - the corresponding ~/.claude/projects/<encoded> session JSONL dirs
//     for any takeovers (enumerated BEFORE the takeovers dir is deleted)
//   - all keychain entries under the "triagefactory" service
//   - the symlink left by `triagefactory install` at its default
//     destination, when present
//
// What it does NOT remove:
//   - the binary itself. With Homebrew installs, that's `brew uninstall
//     triagefactory`. We can't reliably (or safely) do it for the user
//     because the running process owns the file we'd be removing.
//
// Destructive and irreversible — defaults to interactive confirmation,
// `--yes` skips. Best-effort: each step is independent, failures are
// logged, and the command exits non-zero only when something actually
// failed.
package uninstall

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
)

// Handle dispatches the uninstall subcommand.
func Handle(args []string) {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fail("resolve home dir: %v", err)
	}
	dataDir := filepath.Join(home, ".triagefactory")
	linkPath := defaultInstallLink()

	plan := buildPlan(dataDir, linkPath)
	if plan.empty() {
		fmt.Println("triagefactory: nothing to uninstall — no local state found.")
		fmt.Println("If you installed via Homebrew, run `brew uninstall triagefactory` to remove the binary.")
		return
	}

	fmt.Println("triagefactory uninstall — about to remove:")
	for _, line := range plan.summary() {
		fmt.Printf("  - %s\n", line)
	}
	fmt.Println()
	fmt.Println("This is irreversible. The binary itself stays — remove it with `brew uninstall triagefactory` (or by hand for source builds).")

	if !*yes && !confirm("Proceed? [y/N] ") {
		fmt.Println("aborted.")
		os.Exit(1)
	}

	failed := false

	// Order: enumerate takeover dirs and clear their Claude project
	// entries BEFORE removing the takeovers tree, otherwise we lose
	// the inputs needed to compute the encoded names.
	if plan.hasTakeovers {
		if n, err := removeClaudeProjectsForTakeovers(filepath.Join(dataDir, "takeovers"), home); err != nil {
			fmt.Fprintf(os.Stderr, "  warn: enumerating takeovers: %v\n", err)
			failed = true
		} else if n > 0 {
			fmt.Printf("  removed %d Claude Code session entr%s\n", n, plural(n, "y", "ies"))
		}
	}

	if plan.hasDataDir {
		if err := os.RemoveAll(dataDir); err != nil {
			fmt.Fprintf(os.Stderr, "  warn: remove %s: %v\n", dataDir, err)
			failed = true
		} else {
			fmt.Printf("  removed %s\n", dataDir)
		}
	}

	if err := auth.Clear(); err != nil {
		fmt.Fprintf(os.Stderr, "  warn: clear keychain: %v\n", err)
		failed = true
	} else {
		fmt.Println("  cleared keychain entries")
	}

	if plan.hasInstallLink {
		if err := os.Remove(linkPath); err != nil {
			fmt.Fprintf(os.Stderr, "  warn: remove %s: %v (try: sudo rm %q)\n", linkPath, err, linkPath)
			failed = true
		} else {
			fmt.Printf("  removed install symlink %s\n", linkPath)
		}
	}

	fmt.Println()
	if failed {
		fmt.Println("triagefactory uninstall: completed with warnings (see above).")
		os.Exit(1)
	}
	fmt.Println("triagefactory uninstall: done. To remove the binary, run `brew uninstall triagefactory`.")
}

// uninstallPlan is the set of artifacts present on disk at invocation
// time. We snapshot this before doing anything so we can show the user
// an accurate "about to remove" list without lying about things that
// were never there in the first place.
type uninstallPlan struct {
	dataDir        string
	linkPath       string
	hasDataDir     bool
	hasTakeovers   bool
	hasInstallLink bool
}

func (p uninstallPlan) empty() bool {
	// Keychain entries aren't probed here — go-keyring's only "exists?"
	// API is a Get, which on macOS prompts the user for permission to
	// read each item. Probing here would prompt 6 times before the
	// user even said yes. The Clear() call later is no-op for missing
	// keys, so it's safe to always run.
	return !p.hasDataDir && !p.hasInstallLink
}

func (p uninstallPlan) summary() []string {
	var lines []string
	if p.hasDataDir {
		lines = append(lines, fmt.Sprintf("%s/ (database, config, takeovers, repo clones)", p.dataDir))
	}
	if p.hasTakeovers {
		lines = append(lines, "Claude Code session entries under ~/.claude/projects/ for any takeovers")
	}
	lines = append(lines, "credentials in the OS keychain (GitHub + Jira tokens)")
	if p.hasInstallLink {
		lines = append(lines, fmt.Sprintf("install symlink at %s", p.linkPath))
	}
	return lines
}

func buildPlan(dataDir, linkPath string) uninstallPlan {
	p := uninstallPlan{dataDir: dataDir, linkPath: linkPath}

	if info, err := os.Stat(dataDir); err == nil && info.IsDir() {
		p.hasDataDir = true
	}
	if info, err := os.Stat(filepath.Join(dataDir, "takeovers")); err == nil && info.IsDir() {
		p.hasTakeovers = true
	}
	// Lstat — we want the symlink itself, not its target. A broken
	// symlink (target removed) still counts as something to clean up.
	if linkPath != "" {
		if _, err := os.Lstat(linkPath); err == nil {
			p.hasInstallLink = true
		}
	}
	return p
}

// removeClaudeProjectsForTakeovers walks ~/.triagefactory/takeovers/run-*
// and deletes the matching ~/.claude/projects/<encoded> dir for each.
// Encoding rule mirrors encodeClaudeProjectDir in internal/worktree:
// every '/' AND every '.' becomes '-'. Returns the number of project
// dirs successfully removed.
//
// We don't import internal/worktree to reuse its encoder — uninstall is
// the one entrypoint where we want zero coupling to the server's
// dependency graph (config readers, db drivers, etc.) so the subcommand
// stays cheap and side-effect-free at startup.
func removeClaudeProjectsForTakeovers(takeoversDir, home string) (int, error) {
	entries, err := os.ReadDir(takeoversDir)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "run-") {
			continue
		}
		full := filepath.Join(takeoversDir, entry.Name())
		resolved, err := filepath.EvalSymlinks(full)
		if err != nil {
			resolved = full
		}
		encoded := strings.NewReplacer("/", "-", ".", "-").Replace(resolved)
		projectDir := filepath.Join(home, ".claude", "projects", encoded)
		if err := os.RemoveAll(projectDir); err == nil {
			count++
		}
	}
	return count, nil
}

// defaultInstallLink mirrors the destination logic in cmd/install. We
// don't try to discover non-default locations: if the user passed
// --dest somewhere weird at install time, they know where it went and
// can remove it themselves. False positives would be worse than false
// negatives here.
func defaultInstallLink() string {
	if runtime.GOOS == "darwin" {
		return "/usr/local/bin/triagefactory"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "bin", "triagefactory")
}

func confirm(prompt string) bool {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

func plural(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "triagefactory uninstall: "+format+"\n", args...)
	os.Exit(1)
}
