package agentproc

import "strconv"

// BuildArgs assembles the argv for a `claude` invocation. Pulled out of
// Run so the flag set is unit-testable without spawning a subprocess.
//
// The shape mirrors what the delegate spawner used inline before this
// package existed: stream-json output, verbose, allowedTools, optional
// --resume + --max-turns. New flags should be added here so both the
// initial-run and resume paths pick them up uniformly.
func BuildArgs(opts RunOptions) []string {
	args := []string{
		"-p", opts.Message,
	}
	if opts.SessionID != "" {
		args = append(args, "--resume", opts.SessionID)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	args = append(args,
		"--output-format", "stream-json",
		"--verbose",
	)
	if opts.AllowedTools != "" {
		args = append(args, "--allowedTools", opts.AllowedTools)
	}
	for _, dir := range opts.AddDirs {
		if dir == "" {
			continue
		}
		args = append(args, "--add-dir", dir)
	}
	if opts.SystemPrompt != "" {
		// --append-system-prompt is additive: it sits after Claude
		// Code's default system prompt rather than replacing it. The
		// curator runtime uses this for the project-context prompt;
		// the delegate runtime currently leaves it unset so the
		// envelope (mission text) carries all role-shaping content.
		args = append(args, "--append-system-prompt", opts.SystemPrompt)
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(opts.MaxTurns))
	}
	return args
}
