package install

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestDefaultDestination locks in the per-OS defaults so a future
// refactor (e.g. switching macOS to ~/.local/bin too) doesn't slip
// past review unannounced. The defaults matter — they're what users
// see when they run `triagefactory install` with no args.
func TestDefaultDestination(t *testing.T) {
	got := defaultDestination()
	switch runtime.GOOS {
	case "darwin":
		if got != "/usr/local/bin/triagefactory" {
			t.Errorf("darwin default = %q, want /usr/local/bin/triagefactory", got)
		}
	default:
		// Linux + everything else share the XDG userland default.
		if got != "~/.local/bin/triagefactory" {
			t.Errorf("default = %q, want ~/.local/bin/triagefactory", got)
		}
	}
}

// TestExpandHome covers the small home-dir helper. Critical because
// `~/.local/bin/...` is the default Linux destination and a bug here
// would either fail with `~` in the path or write to a literal
// directory named `~`.
func TestExpandHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		in   string
		want string
	}{
		{"~", home},
		{"~/.local/bin/triagefactory", filepath.Join(home, ".local/bin/triagefactory")},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
		{"~user/elsewhere", "~user/elsewhere"}, // not a leading "~/" — pass through
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := expandHome(tc.in)
			if err != nil {
				t.Fatalf("expandHome: %v", err)
			}
			if got != tc.want {
				t.Errorf("expandHome(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestOnPath checks the "is this dir on $PATH" detection used by the
// post-install warning. False positives or negatives would either
// silence the warning when the user really needs it (worse) or warn
// when everything's fine (just noisy).
func TestOnPath(t *testing.T) {
	bin := t.TempDir()
	other := t.TempDir()

	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/local/bin")
	if !onPath(bin) {
		t.Errorf("onPath(%q) = false, want true (PATH contains it)", bin)
	}
	if onPath(other) {
		t.Errorf("onPath(%q) = true, want false (PATH doesn't contain it)", other)
	}
}
