package worktree

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// sshPreflightCache holds a per-host cached result so a burst of simultaneous
// clone failures (e.g. during bootstrap with many repos) triggers at most one
// live SSH probe rather than N sequential 15-second probes.
//
// Cache policy: a success result is kept for the process lifetime (SSH key
// configuration doesn't change without a restart). A failure result expires
// after sshPreflightFailureTTL so the user can fix their SSH setup and have
// the new state detected on the next clone cycle.
var sshPreflightCache = struct {
	mu      sync.Mutex
	entries map[string]sshPreflightEntry
}{entries: make(map[string]sshPreflightEntry)}

const sshPreflightFailureTTL = 60 * time.Second

type sshPreflightEntry struct {
	err      error // nil = success
	cachedAt time.Time
}

func (e sshPreflightEntry) valid() bool {
	if e.err == nil {
		return true // success cached for process lifetime
	}
	return time.Since(e.cachedAt) < sshPreflightFailureTTL
}

// PreflightSSH runs a non-interactive `ssh -T <host>` against the given
// host (typically "git@github.com") to verify that the user has a
// usable SSH key + agent + known_hosts entry — the prerequisites for
// `git clone` over SSH to succeed without prompting. The check is the
// canonical way to test GitHub SSH access; GitHub returns a greeting
// with the authenticated username on success and exits 1 (because no
// shell is granted), so we treat the presence of the greeting in the
// combined output as the success signal rather than the exit code.
//
// Options used:
//   - -T:                          disable pty allocation (we don't want a shell)
//   - BatchMode=yes:               never prompt for passphrases or unknown hosts
//   - StrictHostKeyChecking=accept-new: write the host key on first
//     connection so a clean machine doesn't fail the very first probe;
//     after that the host key is pinned the standard way
//
// Returns nil on success. On failure returns an error whose Error()
// string includes the combined stdout+stderr of the ssh process so
// callers can surface the underlying reason (no agent loaded,
// publickey denied, host unreachable, etc.). The caller does not need
// to parse this string to make routing decisions — preflight failure
// itself is the "SSH side is the cause" signal.
func PreflightSSH(ctx context.Context, host string) error {
	if host == "" {
		host = "git@github.com"
	}
	cmd := exec.CommandContext(ctx, "ssh",
		"-T",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		host,
	)
	out, _ := cmd.CombinedOutput()
	combined := strings.TrimSpace(string(out))

	// GitHub's greeting on a successful auth handshake (no shell granted):
	//   "Hi <username>! You've successfully authenticated, but GitHub does
	//    not provide shell access."
	// The exit code is 1 because of "no shell access", so we key off the
	// stable greeting substring rather than the status code.
	if strings.Contains(combined, "successfully authenticated") {
		return nil
	}

	// Diagnose the most common failure modes so the user gets an
	// actionable message rather than just "Permission denied
	// (publickey)". The probe is a best-effort `ssh-add -l` against
	// whatever SSH_AUTH_SOCK the binary inherited; its exit code maps
	// cleanly to "no agent" / "empty agent" / "agent loaded".
	hint := diagnoseSSHFailure(ctx, combined)
	return fmt.Errorf("%s\n\n%s", combined, hint)
}

// diagnoseSSHFailure inspects the ssh stderr and the local ssh-agent
// state to produce a one-paragraph hint pointing at the most likely
// fix. Returned text is plain and intended for direct UI display.
//
// The three branches correspond to ssh-add -l's documented exit codes:
//
//	0: agent reachable, has identities → key probably not on GitHub,
//	   or BatchMode is rejecting a passphrase-protected key whose
//	   keychain unlock dialog is being suppressed.
//	1: agent reachable, no identities → most common case on macOS
//	   with passphrase-protected keys: the keychain agent is empty
//	   and BatchMode blocks the unlock prompt.
//	2: agent socket unreachable (SSH_AUTH_SOCK unset or stale).
func diagnoseSSHFailure(ctx context.Context, sshOut string) string {
	addCmd := exec.CommandContext(ctx, "ssh-add", "-l")
	addOut, _ := addCmd.CombinedOutput()
	exit := -1
	if addCmd.ProcessState != nil {
		exit = addCmd.ProcessState.ExitCode()
	}

	const githubKeysURL = "https://github.com/settings/keys"
	const macOSNote = "On macOS, `ssh-add --apple-use-keychain ~/.ssh/id_ed25519` persists the key in the login keychain so you don't have to re-add it after every reboot."

	switch exit {
	case 1:
		// Agent reachable but empty — by far the most common cause
		// of "Permission denied (publickey)" with BatchMode on macOS.
		return strings.Join([]string{
			"Your ssh-agent has no keys loaded, so the connection had nothing to offer.",
			"",
			"Fix:",
			"  ssh-add ~/.ssh/id_ed25519     # or your key path",
			"",
			"Don't have a key yet?",
			"  ssh-keygen -t ed25519 -C \"you@example.com\"",
			"  ssh-add ~/.ssh/id_ed25519",
			"  cat ~/.ssh/id_ed25519.pub      # paste at " + githubKeysURL,
			"",
			macOSNote,
		}, "\n")
	case 2:
		// Socket missing — SSH_AUTH_SOCK isn't set in this process's
		// environment. Common when the binary was launched outside
		// a normal login shell (Finder, launchd plist, etc.).
		return strings.Join([]string{
			"ssh-agent isn't reachable from this process (SSH_AUTH_SOCK is unset or stale).",
			"",
			"Fix: start Triage Factory from a terminal that has the agent running.",
			"On macOS this is automatic in any login shell; double-clicking the binary",
			"in Finder runs it in launchd's reduced environment without the agent.",
			"",
			"Check from the terminal you launched TF in:",
			"  echo $SSH_AUTH_SOCK            # should be non-empty",
			"  ssh-add -l                     # should list your key(s)",
		}, "\n")
	case 0:
		// Agent has keys but GitHub still rejected — key probably
		// isn't on GitHub, or it's a different identity than the
		// one authorized.
		count := strings.Count(string(addOut), "\n")
		if count == 0 {
			count = 1
		}
		return strings.Join([]string{
			fmt.Sprintf("Your ssh-agent has %d key(s) loaded, but GitHub rejected them.", count),
			"",
			"The most likely fix:",
			"  - Make sure the public key is added at " + githubKeysURL,
			"  - Confirm you're connecting as the right GitHub user",
			"",
			"To see which key was offered, run:",
			"  ssh -vT git@github.com 2>&1 | grep 'Offering public key'",
		}, "\n")
	default:
		// ssh-add wasn't found, was killed, or returned an unexpected
		// status. Fall back to a generic hint that covers the broad
		// strokes without being misleading.
		return strings.Join([]string{
			"Couldn't determine the ssh-agent state — falling back to general guidance.",
			"",
			"Common fixes:",
			"  - `ssh-add ~/.ssh/id_ed25519` to load your key",
			"  - Add the public key at " + githubKeysURL,
			"  - Run `ssh -vT git@github.com` for verbose output",
		}, "\n")
	}
}

// CachedPreflightSSH is identical to PreflightSSH but de-duplicates probes
// across concurrent callers and caches the result: successes are kept for the
// process lifetime, failures for sshPreflightFailureTTL (60 s) so a user who
// fixes their SSH setup gets re-detected on the next clone cycle without
// restarting the process.
//
// Concurrency: the global mutex is held across the probe so the
// double-checked-lock pattern actually dedupes. N concurrent callers
// see one ssh + one ssh-add subprocess pair; the rest hit the
// populated cache when they acquire the lock. We accept global
// (rather than per-host) serialization here because the only host in
// production use is "git@github.com" and the probe is bounded by
// PreflightSSH's 15-second context — switching to per-host locks
// would only matter if a future caller probes multiple hosts in
// parallel, which doesn't exist today.
func CachedPreflightSSH(ctx context.Context, host string) error {
	if host == "" {
		host = "git@github.com"
	}

	sshPreflightCache.mu.Lock()
	defer sshPreflightCache.mu.Unlock()

	if e, ok := sshPreflightCache.entries[host]; ok && e.valid() {
		return e.err
	}

	err := PreflightSSH(ctx, host)
	sshPreflightCache.entries[host] = sshPreflightEntry{err: err, cachedAt: time.Now()}
	return err
}
