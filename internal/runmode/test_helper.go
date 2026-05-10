package runmode

import (
	"testing"
)

// SetForTest swaps the process mode for the duration of t and
// restores the previous value via t.Cleanup. Access to currentMode
// goes through modeMu, so this helper is data-race-free, but it is
// not safe for overlapping parallel tests: it mutates shared global
// state, so concurrent callers can still interfere logically. Tests
// that call SetForTest must avoid running in parallel with each other
// or otherwise serialize their use of the helper.
//
// Lives in a non-_test.go file so consumers' test packages can call
// it (a _test.go file would only be visible to runmode_test). The
// testing import this brings into the package is dependency-free
// and doesn't bloat production builds.
func SetForTest(t testing.TB, m Mode) {
	t.Helper()
	if m != ModeLocal && m != ModeMulti {
		t.Fatalf("runmode.SetForTest: unknown mode %q", m)
	}
	modeMu.Lock()
	prev := currentMode
	currentMode = m
	modeMu.Unlock()
	t.Cleanup(func() {
		modeMu.Lock()
		currentMode = prev
		modeMu.Unlock()
	})
}
