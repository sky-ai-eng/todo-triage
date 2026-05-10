package pgtest

import (
	"testing"
)

// TestHarness_Boots smoke-tests the boot path. If this passes, the
// rest of the Postgres test suite can rely on Shared(t) returning a
// usable harness.
func TestHarness_Boots(t *testing.T) {
	h := Shared(t)

	var n int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM events_catalog`).Scan(&n); err != nil {
		t.Fatalf("query events_catalog: %v", err)
	}
	if n == 0 {
		t.Errorf("events_catalog has no rows — seeds didn't run")
	}
}
