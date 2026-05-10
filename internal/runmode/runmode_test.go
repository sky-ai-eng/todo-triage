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
		{"multi", ModeMulti, false},
		{"Multi", ModeMulti, false},
		{"MULTI", ModeMulti, false},
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

func TestInit_AcceptsValidModes(t *testing.T) {
	// Use SetForTest to snapshot+restore so the test doesn't leak
	// the post-Init state into other tests.
	SetForTest(t, ModeLocal)

	if err := Init(ModeLocal); err != nil {
		t.Errorf("Init(ModeLocal) errored: %v", err)
	}
	if got := Current(); got != ModeLocal {
		t.Errorf("after Init(ModeLocal), Current() = %q", got)
	}

	if err := Init(ModeMulti); err != nil {
		t.Errorf("Init(ModeMulti) errored: %v", err)
	}
	if got := Current(); got != ModeMulti {
		t.Errorf("after Init(ModeMulti), Current() = %q", got)
	}
}

func TestInit_RejectsUnknown(t *testing.T) {
	SetForTest(t, ModeLocal)

	err := Init(Mode("bogus"))
	if err == nil {
		t.Fatalf("Init(Mode(\"bogus\")) should have errored")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error message should reference the bad value; got %q", err.Error())
	}
	// Mode must not have been mutated by the failed Init.
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
