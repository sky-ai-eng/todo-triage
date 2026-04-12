package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupPlainCheckout creates a fake worktree with a .git directory (plain
// checkout layout, not linked worktree). Returns the worktree root and the
// path where .git/info/exclude lives so tests can pre-populate or assert
// against it directly.
func setupPlainCheckout(t *testing.T) (wtDir, excludePath string) {
	t.Helper()
	wtDir = t.TempDir()
	gitDir := filepath.Join(wtDir, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "info"), 0755); err != nil {
		t.Fatalf("mkdir .git/info: %v", err)
	}
	return wtDir, filepath.Join(gitDir, "info", "exclude")
}

// setupLinkedWorktree creates a fake linked worktree: .git is a pointer
// file containing "gitdir: <externalPath>" and the external gitdir has
// its own info/ directory. Matches how `git worktree add` sets things up.
func setupLinkedWorktree(t *testing.T) (wtDir, excludePath string) {
	t.Helper()
	root := t.TempDir()
	wtDir = filepath.Join(root, "worktree")
	extGitDir := filepath.Join(root, "bare.git", "worktrees", "wt-42")
	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatalf("mkdir wtDir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(extGitDir, "info"), 0755); err != nil {
		t.Fatalf("mkdir external gitdir/info: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, ".git"), []byte("gitdir: "+extGitDir+"\n"), 0644); err != nil {
		t.Fatalf("write .git pointer: %v", err)
	}
	return wtDir, filepath.Join(extGitDir, "info", "exclude")
}

func TestWriteLocalExcludes_CreatesFileWhenMissing(t *testing.T) {
	wtDir, excludePath := setupPlainCheckout(t)

	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude file: %v", err)
	}
	s := string(content)

	if !strings.Contains(s, "_scratch/") {
		t.Errorf("missing _scratch/ pattern: %q", s)
	}
	if !strings.Contains(s, "task_memory/") {
		t.Errorf("missing task_memory/ pattern: %q", s)
	}
	if !strings.Contains(s, managedExcludeHeader) {
		t.Errorf("missing managed header: %q", s)
	}
}

// TestWriteLocalExcludes_PreservesExistingContent is the core regression
// test for issue 1: any pre-existing content in .git/info/exclude (user
// patterns, comments, other tool-managed lines) must survive untouched.
// The old unconditional-overwrite implementation would have failed this.
func TestWriteLocalExcludes_PreservesExistingContent(t *testing.T) {
	wtDir, excludePath := setupPlainCheckout(t)

	// Pre-populate with user content — representative of what someone
	// might have added by hand or via another tool.
	userContent := `# git ls-files --others --exclude-from=.git/info/exclude
# Lines that start with '#' are comments.
# For a project mostly in C, the following would be a good set of
# exclude patterns (uncomment them if you want to use them):
# *.[oa]
# *~
node_modules/
*.swp
`
	if err := os.WriteFile(excludePath, []byte(userContent), 0644); err != nil {
		t.Fatalf("pre-populate: %v", err)
	}

	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude file: %v", err)
	}
	gotStr := string(got)

	// Every line of the original user content must still be present.
	for _, line := range strings.Split(strings.TrimSpace(userContent), "\n") {
		if !strings.Contains(gotStr, line) {
			t.Errorf("user line %q was lost; file now:\n%s", line, gotStr)
		}
	}

	// Our managed patterns must be present too.
	if !strings.Contains(gotStr, "_scratch/") {
		t.Error("missing _scratch/ after append")
	}
	if !strings.Contains(gotStr, "task_memory/") {
		t.Error("missing task_memory/ after append")
	}
}

// TestWriteLocalExcludes_Idempotent verifies that running the function
// twice against the same worktree doesn't duplicate entries. Any agent
// lifecycle that spins up a worktree, configures it, tears it down, and
// re-uses the same wtDir (re-delegation) would otherwise accumulate
// duplicate pattern lines.
func TestWriteLocalExcludes_Idempotent(t *testing.T) {
	wtDir, excludePath := setupPlainCheckout(t)

	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("first call: %v", err)
	}
	firstContent, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read after first call: %v", err)
	}

	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("second call: %v", err)
	}
	secondContent, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read after second call: %v", err)
	}

	if string(firstContent) != string(secondContent) {
		t.Errorf("file diverged between calls:\nfirst:\n%s\nsecond:\n%s", firstContent, secondContent)
	}

	// No pattern should appear more than once.
	for _, p := range managedExcludePatterns {
		count := strings.Count(string(secondContent), p)
		if count != 1 {
			t.Errorf("pattern %q appears %d times, want 1", p, count)
		}
	}
}

// TestWriteLocalExcludes_PartialExisting covers the case where one of our
// managed patterns is already present (maybe from a previous partial
// setup, or an unrelated tool) but the other isn't. We should add only
// the missing pattern, not duplicate the existing one.
func TestWriteLocalExcludes_PartialExisting(t *testing.T) {
	wtDir, excludePath := setupPlainCheckout(t)

	// _scratch/ already there; task_memory/ missing.
	if err := os.WriteFile(excludePath, []byte("other-tool-pattern/\n_scratch/\n"), 0644); err != nil {
		t.Fatalf("pre-populate: %v", err)
	}

	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(content)

	// _scratch/ should appear exactly once — not duplicated by the append.
	if count := strings.Count(s, "_scratch/"); count != 1 {
		t.Errorf("_scratch/ appears %d times, want 1; file:\n%s", count, s)
	}
	// task_memory/ must have been added.
	if !strings.Contains(s, "task_memory/") {
		t.Errorf("task_memory/ missing; file:\n%s", s)
	}
	// The unrelated user pattern must survive.
	if !strings.Contains(s, "other-tool-pattern/") {
		t.Errorf("user pattern lost; file:\n%s", s)
	}
}

func TestWriteLocalExcludes_LinkedWorktreePointer(t *testing.T) {
	wtDir, excludePath := setupLinkedWorktree(t)

	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read external exclude: %v", err)
	}
	s := string(content)
	if !strings.Contains(s, "_scratch/") || !strings.Contains(s, "task_memory/") {
		t.Errorf("managed patterns not written through pointer file; got:\n%s", s)
	}
}

func TestWriteLocalExcludes_AppendsTrailingNewlineWhenMissing(t *testing.T) {
	wtDir, excludePath := setupPlainCheckout(t)

	// Existing file does not end with a newline.
	if err := os.WriteFile(excludePath, []byte("no-trailing-newline/"), 0644); err != nil {
		t.Fatalf("pre-populate: %v", err)
	}

	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(content)

	// Original line must still be a recognizable whole line, not mashed
	// into the managed header. Verify the header appears on its own line.
	if !strings.Contains(s, "\n"+managedExcludeHeader) && !strings.HasPrefix(s, managedExcludeHeader) {
		t.Errorf("managed header not on its own line; file:\n%s", s)
	}
	// The original pattern must still be findable as a whole line.
	foundLine := false
	for _, line := range strings.Split(s, "\n") {
		if line == "no-trailing-newline/" {
			foundLine = true
			break
		}
	}
	if !foundLine {
		t.Errorf("original unterminated line was corrupted; file:\n%s", s)
	}
}
