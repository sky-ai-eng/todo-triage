package resume

import (
	"testing"
	"time"
)

// TestHumanDuration checks the picker's "X ago" formatting. Coarse
// granularity is the point — the user only cares whether a takeover
// is "fresh" (minutes) or "stale" (hours/days), not exact precision.
func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Second, "<1m"},
		{59 * time.Second, "<1m"},
		{1 * time.Minute, "1m"},
		{45 * time.Minute, "45m"},
		{59 * time.Minute, "59m"},
		{1 * time.Hour, "1h"},
		{12 * time.Hour, "12h"},
		{23 * time.Hour, "23h"},
		{24 * time.Hour, "1d"},
		{72 * time.Hour, "3d"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := humanDuration(tc.in)
			if got != tc.want {
				t.Errorf("humanDuration(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestPrintHelpDoesntCrash is a smoke test — printHelp writes to
// stdout and shouldn't reference any unset state. Catches dumb typos
// like nil dereferences in help text formatting.
func TestPrintHelpDoesntCrash(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("printHelp panicked: %v", r)
		}
	}()
	printHelp()
}
