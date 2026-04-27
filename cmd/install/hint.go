package install

import (
	"fmt"
	"os/exec"
)

// HintIfMissing prints a one-shot hint at server startup if the
// `triagefactory` binary isn't reachable via $PATH. The takeover modal
// surfaces a `cd … && claude --resume <id>` command; users who'd
// rather use the shorter `triagefactory resume <id>` need the binary
// on PATH. Pointing at the `install` subcommand once on startup is a
// low-friction nudge that catches first-time users without spamming
// repeat starters.
//
// Best-effort: any error (LookPath failing for non-PATH reasons,
// terminal redirected, etc.) is silently ignored. The hint is just a
// hint.
func HintIfMissing() {
	if _, err := exec.LookPath("triagefactory"); err == nil {
		return
	}
	fmt.Println()
	fmt.Println("  tip: `triagefactory` isn't on your PATH yet — run")
	fmt.Println("       `triagefactory install` so the takeover resume command works")
	fmt.Println("       from any terminal.")
	fmt.Println()
}
