// Package resume implements the `triagefactory resume` CLI subcommand
// that hands the user back into a previously taken-over Claude Code
// session. The takeover endpoint sets `runs.worktree_path` to the
// destination dir and `runs.session_id` to the captured session — this
// command reads those, cd's to the worktree, and exec's
// `claude --resume <sid>` so the user's terminal becomes the
// interactive session directly.
//
// UX:
//
//   triagefactory resume                — auto-resume when there's
//                                          exactly one taken-over run;
//                                          pick newest with a numbered
//                                          picker if more than one.
//   triagefactory resume <short-id>     — disambiguate by run-ID
//                                          prefix (matches the 8-char
//                                          shortRunID convention used
//                                          in the modal).
//
// We syscall.Exec rather than fork claude so the user gets a clean
// interactive process without an extra pid in the tree.
package resume

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// Handle dispatches the resume subcommand.
func Handle(args []string) {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			printHelp()
			return
		}
	}

	database, err := db.Open()
	if err != nil {
		fail("open database: %v", err)
	}
	defer database.Close()
	// Idempotent — no-op if the schema is already current. Covers the
	// rare case of running `resume` against a freshly-created DB
	// before the server has ever migrated it.
	if err := db.Migrate(database); err != nil {
		fail("migrate database: %v", err)
	}

	runs, err := db.ListTakenOverRunsForResume(database)
	if err != nil {
		fail("list taken-over runs: %v", err)
	}
	if len(runs) == 0 {
		fmt.Fprintln(os.Stderr, "no taken-over runs available — click \"Take over\" on an active run first.")
		os.Exit(1)
	}

	// Optional positional arg: filter by run-id prefix. Matches the
	// short id we surface in the takeover modal.
	if len(args) > 0 {
		prefix := args[0]
		filtered := runs[:0]
		for _, r := range runs {
			if strings.HasPrefix(r.RunID, prefix) {
				filtered = append(filtered, r)
			}
		}
		switch len(filtered) {
		case 0:
			fail("no taken-over run with id prefix %q", prefix)
		case 1:
			execClaudeResume(filtered[0])
		default:
			fmt.Fprintf(os.Stderr, "%d taken-over runs match prefix %q — be more specific:\n\n", len(filtered), prefix)
			renderRuns(filtered)
			os.Exit(1)
		}
		return
	}

	if len(runs) == 1 {
		execClaudeResume(runs[0])
	}

	// Multiple candidates and no prefix — interactive picker.
	choice := pickRun(runs)
	execClaudeResume(choice)
}

// execClaudeResume cd's to the run's worktree and replaces the current
// process with `claude --resume <session-id>`. Replacing the process
// means the user's interactive shell talks to claude directly — no
// orphan triagefactory pid sitting around. Path lookup uses
// exec.LookPath so we surface a clear "claude not found" error
// instead of cryptic exec failures if Claude Code isn't on PATH.
func execClaudeResume(r db.TakenOverRun) {
	if _, err := os.Stat(r.WorktreePath); err != nil {
		fail("takeover dir at %s is gone: %v", r.WorktreePath, err)
	}
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		fail("claude not found on PATH (install Claude Code: https://docs.claude.com/en/docs/claude-code): %v", err)
	}
	if err := os.Chdir(r.WorktreePath); err != nil {
		fail("cd %s: %v", r.WorktreePath, err)
	}
	args := []string{"claude", "--resume", r.SessionID}
	if err := syscall.Exec(claudePath, args, os.Environ()); err != nil {
		fail("exec claude: %v", err)
	}
}

// pickRun renders a numbered list of taken-over runs and reads the
// user's choice from stdin. Loops on invalid input rather than
// exiting — typing a non-number on first try shouldn't lose the list.
func pickRun(runs []db.TakenOverRun) db.TakenOverRun {
	fmt.Fprintln(os.Stderr, "Multiple taken-over runs available:")
	fmt.Fprintln(os.Stderr)
	renderRuns(runs)
	fmt.Fprintln(os.Stderr)
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Fprintf(os.Stderr, "Pick [1-%d] (q to quit): ", len(runs))
		line, err := reader.ReadString('\n')
		if err != nil {
			fail("read choice: %v", err)
		}
		line = strings.TrimSpace(line)
		if line == "q" || line == "Q" {
			os.Exit(0)
		}
		n, err := strconv.Atoi(line)
		if err != nil || n < 1 || n > len(runs) {
			fmt.Fprintf(os.Stderr, "  invalid choice: %q\n", line)
			continue
		}
		return runs[n-1]
	}
}

// renderRuns prints a compact numbered table of taken-over runs to
// stderr (so it doesn't pollute stdout if a future caller pipes
// resume's output anywhere). Format: "  N. <short-id>  <source-id>
// <task-title> (Xm ago)".
func renderRuns(runs []db.TakenOverRun) {
	now := time.Now()
	for i, r := range runs {
		short := r.RunID
		if len(short) > 8 {
			short = short[:8]
		}
		title := r.TaskTitle
		if title == "" {
			title = "(no title)"
		}
		if len(title) > 60 {
			title = title[:57] + "..."
		}
		source := r.SourceID
		if source == "" {
			source = "—"
		}
		fmt.Fprintf(os.Stderr, "  %d. %s  %s  %s  (%s ago)\n",
			i+1, short, source, title, humanDuration(now.Sub(r.CompletedAt)))
	}
}

// humanDuration renders a duration as "5m" / "2h" / "3d" — the
// granularity matches what the user cares about ("how long ago did I
// take this over?") without precision noise.
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func printHelp() {
	fmt.Println(`triagefactory resume — hand back into a previously taken-over Claude Code session.

Usage:
  triagefactory resume                — auto-resume when there's exactly one
                                         taken-over run; otherwise show a
                                         numbered picker.
  triagefactory resume <short-id>     — disambiguate by run-ID prefix
                                         (the 8-char id from the modal).

The command cd's to the takeover worktree and exec's
"claude --resume <session-id>", so your terminal becomes the
interactive Claude Code session directly.`)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "triagefactory resume: "+format+"\n", args...)
	os.Exit(1)
}
