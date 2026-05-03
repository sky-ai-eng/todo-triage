package curator

import "strings"

// BuildAllowedTools returns the --allowedTools value for a Curator
// invocation. Subset of the delegate allowlist (internal/delegate/
// allowlist.go) with the differences that matter for this runtime:
//
//   - No "Bash(<selfBin> exec *)". The Curator is a chat assistant,
//     not a delegated agent — it doesn't issue scoped GH/Jira
//     subcommands and giving it that capability would surface the
//     run-scoped TRIAGE_FACTORY_RUN_ID env var assumptions to a
//     non-run context.
//   - No git push / commit / merge / rebase. The Curator's cwd is
//     the project's knowledge directory, not a checked-out repo;
//     git write operations there would either no-op or pollute a
//     scratch directory that the user expects to hold knowledge
//     files only.
//   - No package-manager / build-system commands. The Curator does
//     not build code; widening the surface earns nothing and adds
//     channels for prompt-injected drift.
//
// File ops (Read/Write/Edit/Glob/Grep/WebFetch/WebSearch) plus
// read-side text utilities are kept — those are the tools the
// Curator actually needs to manage knowledge-base/*.md and crawl
// for information.
func BuildAllowedTools() string {
	bashPatterns := []string{
		// File inspection (read-only).
		"Bash(cat *)", "Bash(head *)", "Bash(tail *)",
		"Bash(ls *)", "Bash(tree *)",
		"Bash(stat *)", "Bash(file *)", "Bash(wc *)",
		"Bash(du *)",
		"Bash(pwd)", "Bash(whoami)", "Bash(hostname)",
		"Bash(date *)", "Bash(which *)", "Bash(type *)",
		"Bash(true)", "Bash(false)",

		// Text search.
		"Bash(grep *)", "Bash(egrep *)", "Bash(fgrep *)",
		"Bash(rg *)", "Bash(ag *)",
		"Bash(find *)", "Bash(fd *)",

		// Text processing.
		"Bash(sort *)", "Bash(uniq *)",
		"Bash(cut *)", "Bash(tr *)", "Bash(paste *)",
		"Bash(tee *)", "Bash(awk *)", "Bash(sed *)",
		"Bash(echo *)", "Bash(printf *)",
		"Bash(diff *)", "Bash(cmp *)", "Bash(comm *)",
		"Bash(xargs *)", "Bash(rev *)", "Bash(fold *)",

		// Structured data.
		"Bash(jq *)", "Bash(yq *)",

		// Filesystem ops within the knowledge dir.
		"Bash(mkdir *)", "Bash(touch *)",
		"Bash(cp *)", "Bash(mv *)", "Bash(ln *)",
	}

	otherTools := []string{
		"Read", "Write", "Edit", "Glob", "Grep", "WebSearch", "WebFetch",
	}

	return strings.Join(append(bashPatterns, otherTools...), ",")
}
