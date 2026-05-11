// Package runmode owns the deployment-posture flag the binary reads
// once at startup. Multiple downstream concerns consume it — D2 stores
// dispatch SQLite vs Postgres, D4b paths branch on it, D5 secret
// store picks keychain vs Vault, D7 auth middleware mounts only in
// multi, D8 agent-runner sandbox lifecycle differs by mode, D13's
// container image bakes TF_MODE=multi at build time. Centralizing
// the flag here (rather than inside any one consumer like internal/
// paths) keeps the import lines reading naturally at every call
// site: a store-wiring file checking `runmode.Current() ==
// runmode.ModeMulti` reads correctly, where `paths.Current()` would
// be a name pun.
//
// Mode is read once at process startup from the TF_MODE environment
// variable via InitFromEnv(), called as the first thing in main.go
// before any subsystem touches a path or opens a DB. Default (empty
// TF_MODE) is ModeLocal so existing local installs see no behavior
// change.
//
// LocalDefaultOrg also lives here even though it's not strictly mode
// state — it's the synthetic org-context value local-mode passes
// everywhere D2/D4b/D9 expect a real orgID. Belongs with the mode
// primitives because it only makes sense in concert with them.
package runmode

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// Mode names the runtime mode the binary is operating in. Set once
// at startup and never reassigned outside tests.
type Mode string

const (
	// ModeLocal is the single-binary, single-user, SQLite-backed mode
	// the current product ships as. Default when TF_MODE is unset.
	ModeLocal Mode = "local"

	// ModeMulti is the multi-tenant, Postgres-backed mode the
	// architecture spec (docs/multi-tenant-architecture.html) targets
	// for v1. The binary boots into ModeMulti when TF_MODE=multi;
	// downstream tickets (D2 store dispatch, D3 schema, D4b
	// resolvers) consume the flag.
	ModeMulti Mode = "multi"
)

// Local-mode sentinel identity values. Four constants — one each for
// org, team, user, and agent — used as the canonical local-mode
// identity values at every API entry point. Pre-SKY-269 these were
// synthetic runtime constants with no DB row backing them; post-
// SKY-269 the SQLite migration inserts one row per sentinel into
// orgs/teams/users so FKs from every resource table have a real
// target. The byte values here MUST match the migration's INSERTs
// verbatim — TestBootstrapLocalTenancy_ConstantsMatchRows asserts the
// equivalence so any drift fails CI rather than producing silently-
// broken FKs at runtime.
//
// The nil-shape UUIDs (00000000-...000N) are deliberately chosen for
// log visibility — a row id starting with thirty zeros is instantly
// recognizable as "the local-mode sentinel" rather than as a random
// tenant. gen_random_uuid() never produces these.
const (
	LocalDefaultOrgID   = "00000000-0000-0000-0000-000000000001"
	LocalDefaultTeamID  = "00000000-0000-0000-0000-000000000010"
	LocalDefaultUserID  = "00000000-0000-0000-0000-000000000100"
	LocalDefaultAgentID = "00000000-0000-0000-0000-000000001000"
)

// LocalDefaultOrg is the pre-SKY-269 spelling — kept as an alias of
// LocalDefaultOrgID so call sites that already reference the old name
// keep compiling. New code should reference LocalDefaultOrgID directly.
// Slated for removal once the in-tree sweep completes; see the
// project memory entry under team-agent-reframe / D-LocalParity for
// status.
const LocalDefaultOrg = LocalDefaultOrgID

// currentMode + initialized + modeMu form the package's mutable state.
// Reads through Current() take an RLock — cheap, contention-free for
// readers — so that SetForTest (which writes from test goroutines, in
// parallel suites) is provably race-free against Current()'s reads.
// Production reads are infrequent enough that the RLock overhead is
// noise; we trade a few nanoseconds for the simpler reasoning.
//
// initialized tracks whether Init has been called in production. It
// starts false; Init flips it true. SetForTest snapshots both fields
// and restores them via t.Cleanup, so tests can flip state freely
// without leaking into other tests or into a subsequent Init call.
var (
	currentMode Mode = ModeLocal
	initialized bool
	modeMu      sync.RWMutex
)

// Current returns the active mode. Always safe to call from any
// goroutine.
func Current() Mode {
	modeMu.RLock()
	defer modeMu.RUnlock()
	return currentMode
}

// Init sets the process-wide mode. Production code calls this exactly
// once, at the top of main(), via InitFromEnv. The contract:
//
//   - First call with a valid mode → sets currentMode, returns nil.
//   - Subsequent call with the SAME mode → idempotent no-op, returns
//     nil (so a stray double-init from cmd dispatch wouldn't fatal).
//   - Subsequent call with a DIFFERENT mode → returns an error
//     without mutating state. Catches accidental "let me re-init
//     mid-run" bugs that would otherwise silently flip behavior under
//     subsystems that already cached the original value.
//   - Unknown Mode value → returns an error.
//
// Tests should use SetForTest instead so the cleanup restores the
// previous (initialized, currentMode) pair. SetForTest also flips
// initialized=true so a test's downstream Init calls follow the
// idempotent / conflict branches above (predictable).
func Init(m Mode) error {
	if m != ModeLocal && m != ModeMulti {
		return fmt.Errorf("unknown mode %q (want %q or %q)", m, ModeLocal, ModeMulti)
	}
	modeMu.Lock()
	defer modeMu.Unlock()
	if initialized {
		if currentMode == m {
			return nil
		}
		return fmt.Errorf("already initialized as %q; cannot re-init as %q", currentMode, m)
	}
	currentMode = m
	initialized = true
	return nil
}

// InitFromEnv reads TF_MODE from the environment and initializes the
// mode accordingly. Empty / unset → ModeLocal (so existing local
// installs see no change). Any other value falls through to
// ModeFromEnv's parsing, which errors on unknown values.
//
// Call as the first thing in main.go and every cmd/*/Handle()
// entrypoint, before any subsystem touches a path or opens a DB.
func InitFromEnv() error {
	m, err := ModeFromEnv(os.Getenv("TF_MODE"))
	if err != nil {
		return err
	}
	return Init(m)
}

// ModeFromEnv parses a TF_MODE env-var string into a Mode. Empty
// string maps to ModeLocal (the safe default); known values map to
// their constants; anything else errors. Case-insensitive (any
// mixed-case spelling of "local" / "multi" works) but not
// whitespace-tolerant — operators that pass " local " typo'd a
// space and we surface it rather than silently accept.
func ModeFromEnv(s string) (Mode, error) {
	switch strings.ToLower(s) {
	case "":
		return ModeLocal, nil
	case "local":
		return ModeLocal, nil
	case "multi":
		return ModeMulti, nil
	default:
		return "", fmt.Errorf("unknown TF_MODE=%q (want %q or %q, or empty for local)", s, ModeLocal, ModeMulti)
	}
}
