package runmode

import (
	"strings"
	"testing"
)

func TestModeFromEnv(t *testing.T) {
	cases := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"", ModeLocal, false},
		{"local", ModeLocal, false},
		{"Local", ModeLocal, false},
		{"LOCAL", ModeLocal, false},
		{"LoCaL", ModeLocal, false}, // arbitrary mixed case
		{"multi", ModeMulti, false},
		{"Multi", ModeMulti, false},
		{"MULTI", ModeMulti, false},
		{"MuLtI", ModeMulti, false},
		{"multi-tenant", "", true},
		{"prod", "", true},
		{" local ", "", true}, // exact match — no whitespace tolerance
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ModeFromEnv(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ModeFromEnv(%q) = %q, nil; want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ModeFromEnv(%q) errored: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ModeFromEnv(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCurrent_DefaultsToLocal pins the package-init default. A test
// process that never calls Init or SetForTest must still see a usable
// mode — local is the safe choice because production behavior in
// local mode is what the test suite already expects.
func TestCurrent_DefaultsToLocal(t *testing.T) {
	// Don't call SetForTest here — we're explicitly checking the
	// init-time default. SetForTest's cleanup would mask any drift
	// in that default for subsequent tests.
	if got := Current(); got != ModeLocal {
		t.Errorf("Current() = %q at init time, want %q", got, ModeLocal)
	}
}

// withCleanInit clears the init flag for the duration of the test and
// restores the previous state on cleanup. Used by tests that exercise
// Init's first-call branch — without this, test-suite ordering would
// determine whether Init's been called by the time we run.
func withCleanInit(t *testing.T) {
	t.Helper()
	modeMu.Lock()
	prevMode, prevInit := currentMode, initialized
	currentMode = ModeLocal
	initialized = false
	modeMu.Unlock()
	t.Cleanup(func() {
		modeMu.Lock()
		currentMode = prevMode
		initialized = prevInit
		modeMu.Unlock()
	})
}

func TestInit_FirstCall(t *testing.T) {
	withCleanInit(t)
	if err := Init(ModeMulti); err != nil {
		t.Errorf("Init(ModeMulti) errored on clean slate: %v", err)
	}
	if got := Current(); got != ModeMulti {
		t.Errorf("after Init(ModeMulti), Current() = %q", got)
	}
}

func TestInit_IdempotentOnSameMode(t *testing.T) {
	withCleanInit(t)
	if err := Init(ModeLocal); err != nil {
		t.Fatalf("first Init: %v", err)
	}
	if err := Init(ModeLocal); err != nil {
		t.Errorf("second Init with same mode should be a no-op, errored: %v", err)
	}
	if got := Current(); got != ModeLocal {
		t.Errorf("Current() = %q after double-Init(local), want %q", got, ModeLocal)
	}
}

func TestInit_RejectsConflictingReInit(t *testing.T) {
	withCleanInit(t)
	if err := Init(ModeLocal); err != nil {
		t.Fatalf("first Init: %v", err)
	}
	err := Init(ModeMulti)
	if err == nil {
		t.Fatalf("second Init with different mode should have errored")
	}
	if !strings.Contains(err.Error(), "already initialized") {
		t.Errorf("error should mention 'already initialized'; got %q", err.Error())
	}
	// Crucially: state must NOT have been mutated.
	if got := Current(); got != ModeLocal {
		t.Errorf("after rejected re-init, Current() = %q (must be unchanged)", got)
	}
}

func TestInit_RejectsUnknown(t *testing.T) {
	withCleanInit(t)
	err := Init(Mode("bogus"))
	if err == nil {
		t.Fatalf("Init(Mode(\"bogus\")) should have errored")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error message should reference the bad value; got %q", err.Error())
	}
	if got := Current(); got != ModeLocal {
		t.Errorf("after rejected Init, Current() = %q (must be unchanged)", got)
	}
}

// TestSetForTest_RestoresAfter exercises the t.Cleanup-based restore
// path. Subtest sets multi; after subtest exits, parent sees local
// again (because the parent's SetForTest set local).
func TestSetForTest_RestoresAfter(t *testing.T) {
	SetForTest(t, ModeLocal)
	if got := Current(); got != ModeLocal {
		t.Fatalf("setup: Current() = %q, want %q", got, ModeLocal)
	}

	t.Run("inner-flips-to-multi", func(t *testing.T) {
		SetForTest(t, ModeMulti)
		if got := Current(); got != ModeMulti {
			t.Errorf("inside subtest: Current() = %q, want %q", got, ModeMulti)
		}
	})

	if got := Current(); got != ModeLocal {
		t.Errorf("after subtest restore: Current() = %q, want %q", got, ModeLocal)
	}
}

// TestSetForTest_FlipsInitialized confirms SetForTest treats the test
// as "post-init", so a subsequent Init follows the conflict / idempotent
// branches rather than the first-call branch.
func TestSetForTest_FlipsInitialized(t *testing.T) {
	SetForTest(t, ModeLocal)
	// Init with the same mode is the idempotent case.
	if err := Init(ModeLocal); err != nil {
		t.Errorf("Init(ModeLocal) after SetForTest(local) should be idempotent, errored: %v", err)
	}
	// Init with a different mode is the conflict case.
	if err := Init(ModeMulti); err == nil {
		t.Errorf("Init(ModeMulti) after SetForTest(local) should error, got nil")
	}
}
