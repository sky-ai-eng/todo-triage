package uninstall

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolvedTakeoversDir_Default(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, ".triagefactory")
	got, err := resolvedTakeoversDir(dataDir)
	if err != nil {
		t.Fatalf("resolvedTakeoversDir() error: %v", err)
	}

	want := filepath.Join(dataDir, "takeovers")
	if got != want {
		t.Fatalf("resolvedTakeoversDir() = %q, want %q", got, want)
	}
}

func TestResolvedTakeoversDir_ConfigOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, ".triagefactory")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dataDir, err)
	}
	cfgPath := filepath.Join(dataDir, "config.yaml")
	cfg := "server:\n  takeover_dir: ~/custom-takeovers\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", cfgPath, err)
	}

	got, err := resolvedTakeoversDir(dataDir)
	if err != nil {
		t.Fatalf("resolvedTakeoversDir() error: %v", err)
	}

	want := filepath.Join(home, "custom-takeovers")
	if got != want {
		t.Fatalf("resolvedTakeoversDir() = %q, want %q", got, want)
	}
}

func TestBuildPlan_DetectsTakeoversOutsideDataDir(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, ".triagefactory")
	takeoversDir := filepath.Join(root, "custom-takeovers")
	if err := os.MkdirAll(takeoversDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", takeoversDir, err)
	}

	plan := buildPlan(dataDir, takeoversDir, "")
	if !plan.hasTakeovers {
		t.Fatalf("plan.hasTakeovers = false, want true")
	}
	if plan.empty() {
		t.Fatalf("plan.empty() = true, want false")
	}
}

func TestRemoveClaudeProjectsForTakeovers_CountsOnlyExistingDirs(t *testing.T) {
	home := t.TempDir()
	takeoversDir := filepath.Join(t.TempDir(), "takeovers")
	if err := os.MkdirAll(takeoversDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", takeoversDir, err)
	}

	runA := filepath.Join(takeoversDir, "run-a")
	runB := filepath.Join(takeoversDir, "run-b")
	for _, runDir := range []string{runA, runB} {
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", runDir, err)
		}
	}

	projectA := claudeProjectDirForRun(t, home, runA)
	if err := os.MkdirAll(projectA, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", projectA, err)
	}

	n, err := removeClaudeProjectsForTakeovers(takeoversDir, home)
	if err != nil {
		t.Fatalf("removeClaudeProjectsForTakeovers() error: %v", err)
	}
	if n != 1 {
		t.Fatalf("removeClaudeProjectsForTakeovers() removed %d dirs, want 1", n)
	}
	if _, err := os.Stat(projectA); !os.IsNotExist(err) {
		t.Fatalf("projectA still exists or unexpected stat error: %v", err)
	}
}

func TestRemoveClaudeProjectsForTakeovers_ReturnsRemoveErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based permission test is not reliable on Windows")
	}

	home := t.TempDir()
	takeoversDir := filepath.Join(t.TempDir(), "takeovers")
	if err := os.MkdirAll(takeoversDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", takeoversDir, err)
	}
	runDir := filepath.Join(takeoversDir, "run-perm")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", runDir, err)
	}

	projectDir := claudeProjectDirForRun(t, home, runDir)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", projectDir, err)
	}

	projectsRoot := filepath.Join(home, ".claude", "projects")
	if err := os.Chmod(projectsRoot, 0o555); err != nil {
		t.Fatalf("Chmod(%q): %v", projectsRoot, err)
	}
	defer func() {
		_ = os.Chmod(projectsRoot, 0o755)
	}()

	n, err := removeClaudeProjectsForTakeovers(takeoversDir, home)
	if err == nil {
		t.Fatalf("removeClaudeProjectsForTakeovers() error = nil, want non-nil")
	}
	if n != 0 {
		t.Fatalf("removeClaudeProjectsForTakeovers() removed %d dirs, want 0", n)
	}
}

func claudeProjectDirForRun(t *testing.T, home, runDir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(runDir)
	if err != nil {
		resolved = runDir
	}
	encoded := strings.NewReplacer("/", "-", ".", "-").Replace(resolved)
	return filepath.Join(home, ".claude", "projects", encoded)
}
