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

// LocalDefaultOrg is the synthetic org id local-mode callers pass at
// every D2 store call + D4b path resolver. The store layer's SQLite
// impls assert it equals this constant; the path layer (D4b) strips
// it from resolved paths so existing local installs see no on-disk
// change. Callers should reference the constant rather than the
// literal string so a future rename stays grep-able.
const LocalDefaultOrg = "default"

// currentMode holds the process-wide mode after Init runs. The init()
// default is ModeLocal so unit tests that don't bother calling Init
// behave like a local install. Reads are unsynchronized — Init only
// runs at startup before any goroutine spawns, and Go's memory model
// guarantees the init-time write is visible. SetForTest mutates this
// under a t.Cleanup so test-suite parallelism stays safe.
var (
	currentMode Mode         = ModeLocal
	modeMu      sync.RWMutex // guards currentMode for SetForTest's parallel-test use
)

// Current returns the active mode. Always safe to call.
func Current() Mode {
	modeMu.RLock()
	defer modeMu.RUnlock()
	return currentMode
}

// Init sets the process-wide mode. Returns an error if m is not a
// known Mode value. Production code should call this exactly once at
// startup (via InitFromEnv); tests use SetForTest instead so the
// previous value is restored on cleanup.
func Init(m Mode) error {
	if m != ModeLocal && m != ModeMulti {
		return fmt.Errorf("unknown mode %q (want %q or %q)", m, ModeLocal, ModeMulti)
	}
	modeMu.Lock()
	currentMode = m
	modeMu.Unlock()
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
// their constants; anything else errors. Case-insensitive on the
// known values to be forgiving of "Local" / "MULTI" typos.
func ModeFromEnv(s string) (Mode, error) {
	switch s {
	case "", "local", "Local", "LOCAL":
		return ModeLocal, nil
	case "multi", "Multi", "MULTI":
		return ModeMulti, nil
	default:
		return "", fmt.Errorf("unknown TF_MODE=%q (want %q or %q, or empty for local)", s, ModeLocal, ModeMulti)
	}
}
