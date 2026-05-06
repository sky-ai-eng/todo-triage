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
	out, err := cmd.CombinedOutput()
	combined := strings.TrimSpace(string(out))

	// GitHub's greeting on a successful auth handshake (no shell granted):
	//   "Hi <username>! You've successfully authenticated, but GitHub does
	//    not provide shell access."
	// The exit code is 1 because of "no shell access", so we key off the
	// stable greeting substring rather than the status code.
	if strings.Contains(combined, "successfully authenticated") {
		return nil
	}

	if err != nil {
		return fmt.Errorf("ssh preflight against %s failed: %w; output: %s", host, err, combined)
	}
	// No error and no greeting — unusual; treat as failure with the raw output.
	return fmt.Errorf("ssh preflight against %s did not return GitHub auth greeting; output: %s", host, combined)
}

// CachedPreflightSSH is identical to PreflightSSH but de-duplicates probes
// across concurrent callers and caches the result: successes are kept for the
// process lifetime, failures for sshPreflightFailureTTL (60 s) so a user who
// fixes their SSH setup gets re-detected on the next clone cycle without
// restarting the process.
func CachedPreflightSSH(ctx context.Context, host string) error {
	if host == "" {
		host = "git@github.com"
	}

	sshPreflightCache.mu.Lock()
	if e, ok := sshPreflightCache.entries[host]; ok && e.valid() {
		sshPreflightCache.mu.Unlock()
		return e.err
	}
	sshPreflightCache.mu.Unlock()

	err := PreflightSSH(ctx, host)

	sshPreflightCache.mu.Lock()
	sshPreflightCache.entries[host] = sshPreflightEntry{err: err, cachedAt: time.Now()}
	sshPreflightCache.mu.Unlock()

	return err
}
