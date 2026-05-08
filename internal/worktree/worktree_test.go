package worktree

import (
	"context"
	"os"
	"os/exec"
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
	if !strings.Contains(s, managedExcludeBegin) || !strings.Contains(s, managedExcludeEnd) {
		t.Errorf("missing marker pair: %q", s)
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

	// Our managed pattern must be present too.
	if !strings.Contains(gotStr, "_scratch/") {
		t.Error("missing _scratch/ after append")
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
// managed patterns is present in unrelated user content (e.g. the user
// added _scratch/ manually before we ran) and the other isn't. The
// managed block is always written as a complete manifest, so the user's
// line stays untouched AND our block appears with both patterns. Git
// dedupes duplicate lines internally, so two occurrences of _scratch/
// are functionally equivalent to one — the important invariant is
// "user content preserved, managed block complete."
func TestWriteLocalExcludes_PartialExisting(t *testing.T) {
	wtDir, excludePath := setupPlainCheckout(t)

	// _scratch/ lives in user content; we still write the managed block.
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

	// User content preserved (both lines still present as whole lines)
	gotLines := strings.Split(s, "\n")
	wantUserLines := []string{"other-tool-pattern/", "_scratch/"}
	for _, want := range wantUserLines {
		found := false
		for _, line := range gotLines {
			if line == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("user line %q lost; file:\n%s", want, s)
		}
	}

	// Managed block is present and complete — both patterns inside it.
	beginIdx := strings.Index(s, managedExcludeBegin)
	endIdx := strings.Index(s, managedExcludeEnd)
	if beginIdx < 0 || endIdx <= beginIdx {
		t.Fatalf("managed block markers missing or inverted; file:\n%s", s)
	}
	managedSection := s[beginIdx:endIdx]
	for _, p := range managedExcludePatterns {
		if !strings.Contains(managedSection, p) {
			t.Errorf("managed block missing pattern %q; section:\n%s", p, managedSection)
		}
	}
}

// TestWriteLocalExcludes_GrowthReusesBlock is the regression guard for the
// "header duplication on pattern set growth" issue: if managedExcludePatterns
// grows from {A} to {A, B}, a later run should expand the existing managed
// block in place rather than appending a second block with its own header.
// Simulated here by writing a complete managed block containing a subset of
// the current managedExcludePatterns, then running writeLocalExcludes and
// verifying the block now contains the full set and the begin/end markers
// each appear exactly once.
func TestWriteLocalExcludes_GrowthReusesBlock(t *testing.T) {
	wtDir, excludePath := setupPlainCheckout(t)

	// Simulate a previous run that only knew about _scratch/. Format matches
	// what writeLocalExcludes would produce — begin marker, patterns, end
	// marker — but with a subset of the current managedExcludePatterns.
	stale := "user-pattern/\n\n" +
		managedExcludeBegin + "\n" +
		"_scratch/\n" +
		managedExcludeEnd + "\n"
	if err := os.WriteFile(excludePath, []byte(stale), 0644); err != nil {
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

	// Markers must appear exactly once — not duplicated by a new block.
	if n := strings.Count(s, managedExcludeBegin); n != 1 {
		t.Errorf("begin marker appears %d times, want 1; file:\n%s", n, s)
	}
	if n := strings.Count(s, managedExcludeEnd); n != 1 {
		t.Errorf("end marker appears %d times, want 1; file:\n%s", n, s)
	}

	// Managed block should now contain the full set (expanded in place).
	beginIdx := strings.Index(s, managedExcludeBegin)
	endIdx := strings.Index(s, managedExcludeEnd)
	managedSection := s[beginIdx:endIdx]
	for _, p := range managedExcludePatterns {
		if !strings.Contains(managedSection, p) {
			t.Errorf("managed block missing %q after growth; section:\n%s", p, managedSection)
		}
	}

	// User content outside the block must survive.
	if !strings.Contains(s, "user-pattern/") {
		t.Errorf("user line lost after growth rewrite; file:\n%s", s)
	}
}

// TestWriteLocalExcludes_RejectsMissingGitdir is the regression guard
// for the "valid prefix but bogus target" case. A .git pointer file that
// starts with "gitdir:" but references a path that doesn't exist (or
// isn't a directory) would previously pass the textual prefix check and
// then silently get its info/ parent created by the write path's
// MkdirAll, writing to an arbitrary location. The fix stats the
// resolved gitdir before returning and rejects if it's missing or not a
// directory.
func TestWriteLocalExcludes_RejectsMissingGitdir(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, wtDir string)
	}{
		{
			"gitdir path does not exist",
			func(t *testing.T, wtDir string) {
				bogus := filepath.Join(t.TempDir(), "does-not-exist")
				if err := os.WriteFile(filepath.Join(wtDir, ".git"), []byte("gitdir: "+bogus+"\n"), 0644); err != nil {
					t.Fatalf("write .git: %v", err)
				}
			},
		},
		{
			"gitdir path is a file, not a directory",
			func(t *testing.T, wtDir string) {
				fileTarget := filepath.Join(t.TempDir(), "not-a-dir")
				if err := os.WriteFile(fileTarget, []byte("regular file"), 0644); err != nil {
					t.Fatalf("write target file: %v", err)
				}
				if err := os.WriteFile(filepath.Join(wtDir, ".git"), []byte("gitdir: "+fileTarget+"\n"), 0644); err != nil {
					t.Fatalf("write .git: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wtDir := t.TempDir()
			tc.setup(t, wtDir)

			err := writeLocalExcludes(wtDir)
			if err == nil {
				t.Fatal("expected error on bogus gitdir, got nil")
			}
			// Error should make the cause diagnosable — mention either
			// "missing gitdir" or "not a directory".
			msg := err.Error()
			if !strings.Contains(msg, "missing gitdir") && !strings.Contains(msg, "not a directory") {
				t.Errorf("error should mention missing/invalid gitdir, got: %v", err)
			}
		})
	}
}

// TestWriteLocalExcludes_IgnoresExtraLinesInPointer verifies that a .git
// pointer file with content past the first newline still parses
// correctly — git's canonical format is one line, but some tools append
// extra config to the same file, and we should read only the first line.
func TestWriteLocalExcludes_IgnoresExtraLinesInPointer(t *testing.T) {
	root := t.TempDir()
	wtDir := filepath.Join(root, "worktree")
	extGitDir := filepath.Join(root, "bare.git", "worktrees", "wt-xyz")
	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatalf("mkdir wtDir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(extGitDir, "info"), 0755); err != nil {
		t.Fatalf("mkdir ext gitdir: %v", err)
	}
	// Pointer with garbage after the first newline — should be ignored.
	pointerContent := "gitdir: " + extGitDir + "\n" +
		"# this line is not part of the pointer\n" +
		"corrupted garbage " + strings.Repeat("X", 200) + "\n"
	if err := os.WriteFile(filepath.Join(wtDir, ".git"), []byte(pointerContent), 0644); err != nil {
		t.Fatalf("write .git: %v", err)
	}

	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The managed patterns should land in the correct external info/exclude.
	content, err := os.ReadFile(filepath.Join(extGitDir, "info", "exclude"))
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	s := string(content)
	for _, p := range managedExcludePatterns {
		if !strings.Contains(s, p) {
			t.Errorf("managed pattern %q missing; file:\n%s", p, s)
		}
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
	if !strings.Contains(s, "_scratch/") {
		t.Errorf("managed patterns not written through pointer file; got:\n%s", s)
	}
}

// TestWriteLocalExcludes_RejectsMalformedGitPointer is the regression guard
// for a real bug: strings.TrimPrefix is a silent no-op when the prefix
// isn't present, so a corrupted or non-pointer .git file like
// "malicious-path/" would have been interpreted as the literal gitdir
// path, causing us to write info/exclude to an arbitrary location
// relative to the worktree. The fix explicitly requires the trimmed
// content to start with "gitdir:".
func TestWriteLocalExcludes_RejectsMalformedGitPointer(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"no gitdir prefix", "some-other-path/"},
		{"plain path that looks like a relative dir", "../../etc"},
		{"random garbage", "kjsdfhkjshdf"},
		{"partial prefix", "gitdi: /some/path"},
		{"case-wrong prefix", "GITDIR: /some/path"},
		{"empty file", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wtDir := t.TempDir()
			// Write the malformed pointer file
			if err := os.WriteFile(filepath.Join(wtDir, ".git"), []byte(tc.content), 0644); err != nil {
				t.Fatalf("write .git: %v", err)
			}

			err := writeLocalExcludes(wtDir)
			if err == nil {
				t.Fatalf("expected error on malformed pointer %q, got nil", tc.content)
			}
			// Error message should be diagnostic — mention it's not a
			// valid pointer rather than some downstream "no such file".
			if !strings.Contains(err.Error(), "not a valid worktree pointer") &&
				!strings.Contains(err.Error(), "empty gitdir") {
				t.Errorf("error should mention invalid pointer, got: %v", err)
			}

			// Crucially: no file should have been created anywhere based
			// on the corrupted content. Check that nothing matching
			// "info/exclude" exists under the tempdir beyond the .git
			// file we wrote ourselves.
			var unexpected []string
			_ = filepath.Walk(wtDir, func(path string, info os.FileInfo, walkErr error) error {
				if walkErr != nil {
					return nil
				}
				if info.IsDir() {
					return nil
				}
				if path == filepath.Join(wtDir, ".git") {
					return nil
				}
				unexpected = append(unexpected, path)
				return nil
			})
			if len(unexpected) > 0 {
				t.Errorf("malformed pointer caused unexpected writes: %v", unexpected)
			}
		})
	}
}

// TestWriteLocalExcludes_StrayEndMarkerBeforeBlock is the regression
// guard for a subtle mergeManagedBlock bug: strings.Index finds the
// *first* occurrence, so if the end marker string appeared anywhere in
// the file before the actual begin marker (e.g., inside a user comment
// that pastes the marker verbatim, or stale content from a broken
// previous run), the pair check `endIdx > beginIdx` would fail and we'd
// fall to the append path, duplicating the managed block every run.
//
// The fix searches for the end marker only after the begin marker's
// position, so stray earlier occurrences are ignored. This test
// pre-populates a file with a stray end marker before a valid managed
// block, runs writeLocalExcludes twice, and verifies:
//   - The managed block is rewritten in place (not duplicated)
//   - The stray end marker text is preserved (it's user content, not ours)
//   - Idempotent: second run produces the same file
func TestWriteLocalExcludes_StrayEndMarkerBeforeBlock(t *testing.T) {
	wtDir, excludePath := setupPlainCheckout(t)

	// User content that happens to include our end marker string —
	// maybe as a quoted example in a comment, maybe as leftover from a
	// truncated previous managed block that someone hand-edited. The
	// real managed block sits *after* this stray mention.
	stray := "# example of a triagefactory block looks like:\n" +
		"# " + managedExcludeEnd + "\n" +
		"node_modules/\n\n" +
		managedExcludeBegin + "\n" +
		"_scratch/\n" +
		managedExcludeEnd + "\n"
	if err := os.WriteFile(excludePath, []byte(stray), 0644); err != nil {
		t.Fatalf("pre-populate: %v", err)
	}

	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("first call: %v", err)
	}
	firstContent, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read after first: %v", err)
	}
	firstStr := string(firstContent)

	// The begin marker should appear exactly once (ours, rewritten in
	// place). The end marker appears twice: once in the stray comment
	// line (preserved user content) and once as the actual block
	// terminator. Any more than that means we appended a duplicate.
	if n := strings.Count(firstStr, managedExcludeBegin); n != 1 {
		t.Errorf("begin marker count = %d, want 1 (stray end mention caused duplicate append?)\nfile:\n%s", n, firstStr)
	}
	if n := strings.Count(firstStr, managedExcludeEnd); n != 2 {
		t.Errorf("end marker count = %d, want 2 (one stray comment line + one real block)\nfile:\n%s", n, firstStr)
	}

	// User content preserved
	if !strings.Contains(firstStr, "node_modules/") {
		t.Errorf("user line 'node_modules/' lost; file:\n%s", firstStr)
	}
	if !strings.Contains(firstStr, "# example of a triagefactory block looks like:") {
		t.Errorf("stray user comment lost; file:\n%s", firstStr)
	}

	// Managed block now contains both patterns (growth in place)
	beginIdx := strings.Index(firstStr, managedExcludeBegin)
	searchFrom := beginIdx + len(managedExcludeBegin)
	relEnd := strings.Index(firstStr[searchFrom:], managedExcludeEnd)
	if relEnd < 0 {
		t.Fatalf("end marker not found after begin; file:\n%s", firstStr)
	}
	managedSection := firstStr[beginIdx : searchFrom+relEnd]
	for _, p := range managedExcludePatterns {
		if !strings.Contains(managedSection, p) {
			t.Errorf("managed block missing pattern %q; section:\n%s", p, managedSection)
		}
	}

	// Idempotent: second run produces identical content. If the stray
	// end marker still confused us, we'd append on every run and the
	// files would differ.
	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("second call: %v", err)
	}
	secondContent, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read after second: %v", err)
	}
	if string(secondContent) != firstStr {
		t.Errorf("file diverged between calls:\nfirst:\n%s\n\nsecond:\n%s", firstStr, string(secondContent))
	}
}

// TestWriteLocalExcludes_StrayBeginBeforeBlock is the regression guard
// for the stray-begin / "user content clobber" case. If the file has an
// orphaned begin marker earlier (truncated block whose end was removed,
// stale fragment from a broken previous run), matching the *first*
// begin with the real end marker would treat everything in between as
// "our content" and overwrite it — including legitimate user lines.
//
// Using LastIndex for begin locks onto the most recent occurrence so
// any earlier stray markers and the user content around them stay
// untouched.
func TestWriteLocalExcludes_StrayBeginBeforeBlock(t *testing.T) {
	wtDir, excludePath := setupPlainCheckout(t)

	// File state that would trigger the bug pre-fix:
	//   1. A standalone (orphaned) begin marker with no matching end
	//   2. User content between the orphan and the real block
	//   3. A valid begin+end pair (the real managed block)
	//
	// Before the LastIndex fix, strings.Index(existing, begin) would
	// return the orphan's position, the end search would find the real
	// end, and the replace would wipe out the user content between.
	stale := managedExcludeBegin + "\n" + // orphaned begin, no end
		"node_modules/\n" +
		"*.swp\n" +
		"\n" +
		managedExcludeBegin + "\n" + // real begin
		"_scratch/\n" +
		managedExcludeEnd + "\n"
	if err := os.WriteFile(excludePath, []byte(stale), 0644); err != nil {
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

	// User content between the orphan and the real block must survive.
	// This is the core assertion — the old first-begin behavior would
	// have eaten both lines.
	for _, line := range []string{"node_modules/", "*.swp"} {
		foundLine := false
		for _, l := range strings.Split(s, "\n") {
			if l == line {
				foundLine = true
				break
			}
		}
		if !foundLine {
			t.Errorf("user line %q was clobbered; file:\n%s", line, s)
		}
	}

	// The real managed block has been expanded in place. The orphaned
	// begin earlier in the file is left alone — we'd need a bigger
	// cleanup pass to remove orphaned markers, and that's out of scope
	// for this fix.
	beginIdx := strings.LastIndex(s, managedExcludeBegin)
	searchFrom := beginIdx + len(managedExcludeBegin)
	relEnd := strings.Index(s[searchFrom:], managedExcludeEnd)
	if relEnd < 0 {
		t.Fatalf("end marker not found after the last begin; file:\n%s", s)
	}
	managedSection := s[beginIdx : searchFrom+relEnd]
	for _, p := range managedExcludePatterns {
		if !strings.Contains(managedSection, p) {
			t.Errorf("managed block missing %q; section:\n%s", p, managedSection)
		}
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

	// The begin marker must appear on its own line, not mashed onto the
	// end of the unterminated user line.
	if !strings.Contains(s, "\n"+managedExcludeBegin) {
		t.Errorf("begin marker not on its own line; file:\n%s", s)
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

// --- CopyForTakeover tests ---------------------------------------------
//
// These tests need a real git binary because CopyForTakeover shells out
// to `git worktree add --force --no-checkout` and `git rev-parse
// --git-common-dir`. Mocking those would just re-validate our own mock,
// not the integration with git's actual behavior — and the headline bug
// these tests guard against (`--force` letting the add succeed against
// a branch the original worktree still has registered) is only
// observable through real git.

// gitCmd runs a git subprocess in dir, fatal-erroring on failure with
// the combined output included so test failures are diagnosable.
func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Disable hooks + global config so a developer's local git config
	// (e.g. signing requirements, custom hooks) can't make these tests
	// flaky.
	cmd.Env = append(
		append([]string(nil), os.Environ()...),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"HOME="+t.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// setupBareWithBranch creates a bare repo + a linked worktree on a
// branch named "feature". Returns (bareDir, srcWorktreeDir). The
// worktree contains one tracked file (README.md) and is left registered
// with the bare so CopyForTakeover has to use --force to add a second
// worktree on the same branch.
func setupBareWithBranch(t *testing.T) (bareDir, srcWorktree string) {
	t.Helper()
	root := t.TempDir()
	bareDir = filepath.Join(root, "bare.git")
	gitCmd(t, root, "init", "--bare", bareDir)

	// Seed the bare with one commit on "feature" via a temporary
	// scratch worktree we throw away after pushing.
	seed := filepath.Join(root, "seed")
	gitCmd(t, root, "clone", bareDir, seed)
	gitCmd(t, seed, "config", "user.email", "test@example.com")
	gitCmd(t, seed, "config", "user.name", "Test")
	gitCmd(t, seed, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	gitCmd(t, seed, "add", "README.md")
	gitCmd(t, seed, "commit", "-m", "seed")
	gitCmd(t, seed, "push", "origin", "feature")

	// The src worktree the agent would have been using. Linked off the
	// bare on the "feature" branch — same shape Spawner.Delegate
	// produces in production.
	srcWorktree = filepath.Join(root, "src-wt")
	gitCmd(t, bareDir, "worktree", "add", srcWorktree, "feature")
	return bareDir, srcWorktree
}

// TestCopyForTakeover_HappyPath_Branch is the headline test: the source
// worktree is on a branch and still registered with the bare repo (the
// production scenario at the moment Takeover() runs). CopyForTakeover
// must succeed and leave the destination on the same branch with the
// agent's working tree contents.
func TestCopyForTakeover_HappyPath_Branch(t *testing.T) {
	_, srcWorktree := setupBareWithBranch(t)
	baseDir := t.TempDir()

	// Drop a tracked-modification + untracked file in src so we can
	// verify both make it across to the destination via the overlay.
	if err := os.WriteFile(filepath.Join(srcWorktree, "README.md"), []byte("modified\n"), 0644); err != nil {
		t.Fatalf("modify README: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcWorktree, "untracked.txt"), []byte("hi\n"), 0644); err != nil {
		t.Fatalf("write untracked: %v", err)
	}

	dest, err := CopyForTakeover(context.Background(), "abc123", srcWorktree, baseDir)
	if err != nil {
		t.Fatalf("CopyForTakeover: %v", err)
	}

	// The returned path is composed under baseDir as run-<id>.
	wantSuffix := filepath.Join("run-abc123")
	if !strings.HasSuffix(dest, wantSuffix) {
		t.Errorf("dest %q should end with %q", dest, wantSuffix)
	}

	// Both files made it across.
	if data, err := os.ReadFile(filepath.Join(dest, "README.md")); err != nil {
		t.Fatalf("read README in dest: %v", err)
	} else if string(data) != "modified\n" {
		t.Errorf("README content = %q, want modified", string(data))
	}
	if _, err := os.Stat(filepath.Join(dest, "untracked.txt")); err != nil {
		t.Errorf("untracked file not copied: %v", err)
	}

	// Destination is a real git worktree, on the same branch, with the
	// agent's modifications visible to git status.
	branchOut, err := exec.Command("git", "-C", dest, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD in dest: %v", err)
	}
	if got := strings.TrimSpace(string(branchOut)); got != "feature" {
		t.Errorf("dest branch = %q, want feature", got)
	}
}

// TestCopyForTakeover_MovePath_SourceGone confirms the same-fs happy
// path: after CopyForTakeover via `git worktree move`, the source
// directory under /tmp is gone (renamed out) and the bare repo's
// worktree list shows only the destination at the new path. This is
// the assertion that gives us atomicity + simplicity: there's only ever
// one worktree for the run, before and after.
func TestCopyForTakeover_MovePath_SourceGone(t *testing.T) {
	bareDir, srcWorktree := setupBareWithBranch(t)
	baseDir := t.TempDir()

	dest, err := CopyForTakeover(context.Background(), "moved", srcWorktree, baseDir)
	if err != nil {
		t.Fatalf("CopyForTakeover: %v", err)
	}

	if _, err := os.Stat(srcWorktree); !os.IsNotExist(err) {
		t.Errorf("source worktree should have been moved away, but still exists (err=%v)", err)
	}
	listOut, err := exec.Command("git", "-C", bareDir, "worktree", "list").Output()
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	list := string(listOut)
	if strings.Contains(list, srcWorktree) {
		t.Errorf("worktree list still contains original src %q after move; output:\n%s", srcWorktree, list)
	}
	if !strings.Contains(list, dest) {
		t.Errorf("worktree list missing dest %q; output:\n%s", dest, list)
	}
}

// TestAddAndOverlayForTakeover_ForceAllowsLiveOriginal is the explicit
// regression guard for the cross-filesystem fallback path: without
// --force, `git worktree add` against a branch that's already checked
// out elsewhere fails with "branch is already checked out at <path>."
// The fallback runs WHILE the source worktree is still registered (the
// caller — CopyForTakeover — needs the source files for the overlay
// copy and only Removes the source after this returns). This test
// fails loudly if --force is ever removed from addAndOverlayForTakeover.
//
// We call addAndOverlayForTakeover directly because the same-fs setup
// in t.TempDir() takes the move path through CopyForTakeover, which
// would skip this code entirely.
func TestAddAndOverlayForTakeover_ForceAllowsLiveOriginal(t *testing.T) {
	bareDir, srcWorktree := setupBareWithBranch(t)
	destDir := filepath.Join(t.TempDir(), "run-overlay")

	// Sanity check: the original worktree IS registered with the bare,
	// so a non-force add on the same branch should fail. If this
	// precondition stops holding (e.g. someone changes
	// setupBareWithBranch), the rest of the test stops being
	// meaningful.
	probeDest := filepath.Join(t.TempDir(), "probe")
	probe := exec.Command("git", "-C", bareDir, "worktree", "add", "--no-checkout", probeDest, "feature")
	probe.Env = append(append([]string(nil), os.Environ()...),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "HOME="+t.TempDir())
	if probe.Run() == nil {
		t.Fatalf("precondition: non-force add on co-checked-out branch should fail; setup may be wrong (src=%s)", srcWorktree)
	}

	// Now the real call — must succeed because of --force.
	if err := addAndOverlayForTakeover(context.Background(), "overlay-rid", bareDir, srcWorktree, destDir, "feature"); err != nil {
		t.Fatalf("addAndOverlayForTakeover should succeed with --force, got: %v", err)
	}

	// Both worktrees are now registered on the same branch — confirm
	// git sees both in `worktree list`. The caller (CopyForTakeover)
	// removes the original right after this returns, but at this
	// moment they coexist by design.
	listOut, err := exec.Command("git", "-C", bareDir, "worktree", "list").Output()
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	list := string(listOut)
	if !strings.Contains(list, srcWorktree) {
		t.Errorf("worktree list missing original %q; output:\n%s", srcWorktree, list)
	}
	if !strings.Contains(list, destDir) {
		t.Errorf("worktree list missing takeover dest %q; output:\n%s", destDir, list)
	}
}

// TestAddAndOverlayForTakeover_OverlaySkipsManagedDirs is the fallback-
// path equivalent of TestCopyForTakeover_ManagedDirsSkipped. The move
// path handles managed dirs differently (they ride along then get
// rm'd post-move); this test pins down the overlay's skip-during-walk
// behavior since the move test wouldn't exercise it.
func TestAddAndOverlayForTakeover_OverlaySkipsManagedDirs(t *testing.T) {
	bareDir, srcWorktree := setupBareWithBranch(t)
	destDir := filepath.Join(t.TempDir(), "run-overlay-mgr")

	dir := filepath.Join(srcWorktree, "_scratch")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir _scratch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("nope"), 0644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	if err := addAndOverlayForTakeover(context.Background(), "overlay-mgr", bareDir, srcWorktree, destDir, "feature"); err != nil {
		t.Fatalf("addAndOverlayForTakeover: %v", err)
	}

	if _, err := os.Stat(filepath.Join(destDir, "_scratch", "secret.txt")); !os.IsNotExist(err) {
		t.Errorf("_scratch/ should have been skipped by overlay (err=%v)", err)
	}
}

// TestCopyForTakeover_DetachedHead — a source worktree on a detached
// HEAD (uncommon but possible) means currentBranch returns "" and the
// dest gets --detach instead of a branch name. Verify that path.
func TestCopyForTakeover_DetachedHead(t *testing.T) {
	_, srcWorktree := setupBareWithBranch(t)
	gitCmd(t, srcWorktree, "checkout", "--detach")
	baseDir := t.TempDir()

	dest, err := CopyForTakeover(context.Background(), "detached-run", srcWorktree, baseDir)
	if err != nil {
		t.Fatalf("CopyForTakeover detached: %v", err)
	}

	out, err := exec.Command("git", "-C", dest, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "HEAD" {
		t.Errorf("dest HEAD = %q, want detached HEAD", got)
	}
}

// TestCopyForTakeover_RelativeBaseDir guards review-comment fix #2: a
// relative takeover_dir in config must produce an absolute destination
// path so the resume command works regardless of the user's cwd.
func TestCopyForTakeover_RelativeBaseDir(t *testing.T) {
	_, srcWorktree := setupBareWithBranch(t)

	// Switch to a known cwd so the relative path resolves predictably.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	scratch := t.TempDir()
	if err := os.Chdir(scratch); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	dest, err := CopyForTakeover(context.Background(), "rel-run", srcWorktree, "./relative-base")
	if err != nil {
		t.Fatalf("CopyForTakeover: %v", err)
	}
	if !filepath.IsAbs(dest) {
		t.Errorf("dest %q must be absolute when baseDir is relative", dest)
	}
	// Resolve symlinks on both sides before comparing. On macOS,
	// t.TempDir() returns paths under /var/folders/... while filesystem
	// operations elsewhere in CopyForTakeover canonicalize through the
	// /var → /private/var symlink, so a literal comparison would fail
	// despite the paths being the same directory. Use filepath.Rel
	// rather than strings.HasPrefix so /tmp/a doesn't falsely match
	// /tmp/abc — Rel returns a path starting with ".." when dest is
	// not actually under scratch.
	scratchResolved, err := filepath.EvalSymlinks(scratch)
	if err != nil {
		t.Fatalf("eval scratch: %v", err)
	}
	destResolved, err := filepath.EvalSymlinks(dest)
	if err != nil {
		t.Fatalf("eval dest: %v", err)
	}
	rel, err := filepath.Rel(scratchResolved, destResolved)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("dest %q should resolve under cwd %q (rel=%q, err=%v)", destResolved, scratchResolved, rel, err)
	}
}

// TestCopyForTakeover_ExistingDest — re-invocation against the same
// runID returns the existing path without recreating. Important
// because the user may have started editing the takeover dir; we must
// not clobber it.
func TestCopyForTakeover_ExistingDest(t *testing.T) {
	_, srcWorktree := setupBareWithBranch(t)
	baseDir := t.TempDir()

	first, err := CopyForTakeover(context.Background(), "rerun", srcWorktree, baseDir)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Plant a marker file so we can verify the second call returns the
	// existing dir untouched rather than re-creating.
	marker := filepath.Join(first, "USER_EDIT.txt")
	if err := os.WriteFile(marker, []byte("hands off\n"), 0644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	second, err := CopyForTakeover(context.Background(), "rerun", srcWorktree, baseDir)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if first != second {
		t.Errorf("second call returned different path: %q vs %q", first, second)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("marker file removed by second call — destination was clobbered: %v", err)
	}
}

// TestCopyForTakeover_ExistingDest_NotADirectory — the previous early-
// return treated any os.Stat success as "we're done." If a regular
// file ended up at the destination (typo, errant touch), the endpoint
// would return that path and the resume command would fail when the
// user tried to `cd` into a file. Validate that case is now an
// explicit error.
func TestCopyForTakeover_ExistingDest_NotADirectory(t *testing.T) {
	_, srcWorktree := setupBareWithBranch(t)
	baseDir := t.TempDir()

	// Pre-create a regular file at where the destination would land.
	destPath := filepath.Join(baseDir, "run-conflict")
	if err := os.WriteFile(destPath, []byte("not a directory"), 0644); err != nil {
		t.Fatalf("write conflicting file: %v", err)
	}

	_, err := CopyForTakeover(context.Background(), "conflict", srcWorktree, baseDir)
	if err == nil {
		t.Fatal("expected error when destination is a regular file")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error %q should mention 'not a directory'", err.Error())
	}
	// Error message should hint at remediation so the user knows what to do.
	if !strings.Contains(err.Error(), "remove or rename") {
		t.Errorf("error %q should suggest remove/rename", err.Error())
	}
}

// TestCopyForTakeover_ExistingDest_BrokenWorktree — the destination is
// a directory but contains no git metadata (e.g., interrupted previous
// attempt or someone manually mkdir'd it). The bare stat check would
// previously pass and we'd hand the user a useless path. Validation
// catches it now.
func TestCopyForTakeover_ExistingDest_BrokenWorktree(t *testing.T) {
	_, srcWorktree := setupBareWithBranch(t)
	baseDir := t.TempDir()

	// Pre-create an empty directory where the destination would land.
	destPath := filepath.Join(baseDir, "run-empty")
	if err := os.MkdirAll(destPath, 0755); err != nil {
		t.Fatalf("mkdir empty dest: %v", err)
	}

	_, err := CopyForTakeover(context.Background(), "empty", srcWorktree, baseDir)
	if err == nil {
		t.Fatal("expected error when destination is an empty (non-worktree) directory")
	}
	if !strings.Contains(err.Error(), "not a git worktree") {
		t.Errorf("error %q should identify the directory as not-a-worktree", err.Error())
	}
}

// TestCopyForTakeover_ExistingDest_DanglingGitPointer — destination
// looks like a linked worktree (has a `.git` pointer file) but the
// gitdir it references doesn't exist. Common when the user wipes
// ~/.triagefactory/repos. Resume in this dir would fail any git op,
// so validation must reject.
func TestCopyForTakeover_ExistingDest_DanglingGitPointer(t *testing.T) {
	_, srcWorktree := setupBareWithBranch(t)
	baseDir := t.TempDir()

	destPath := filepath.Join(baseDir, "run-dangling")
	if err := os.MkdirAll(destPath, 0755); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}
	// Pointer file referencing a path that doesn't exist.
	bogus := filepath.Join(t.TempDir(), "vanished-gitdir")
	if err := os.WriteFile(filepath.Join(destPath, ".git"), []byte("gitdir: "+bogus+"\n"), 0644); err != nil {
		t.Fatalf("write .git pointer: %v", err)
	}

	_, err := CopyForTakeover(context.Background(), "dangling", srcWorktree, baseDir)
	if err == nil {
		t.Fatal("expected error when .git pointer is dangling")
	}
	if !strings.Contains(err.Error(), "not a git worktree") {
		t.Errorf("error %q should identify the dest as not-a-worktree", err.Error())
	}
}

// TestCopyForTakeover_Validation — early-return error cases that don't
// need a real git repo. baseDir is part of the table so the empty-
// baseDir case (regression guard against filepath.Abs("") silently
// resolving to the binary's cwd) sits alongside the other arg
// validations rather than as a special case.
func TestCopyForTakeover_Validation(t *testing.T) {
	someTmp := t.TempDir()
	cases := []struct {
		name       string
		runID      string
		src        string
		baseDir    string
		wantSubstr string
	}{
		{"empty runID", "", "/some/path", someTmp, "empty run id"},
		{"empty source", "rid", "", someTmp, "empty source"},
		// Without an explicit guard, filepath.Abs("") returns the
		// binary's current working directory and we'd quietly create
		// takeovers there. The error message must mention the base
		// dir so the diagnostic is obvious.
		{"empty baseDir", "rid", "/some/path", "", "empty destination base dir"},
		{"nonexistent source", "rid", filepath.Join(t.TempDir(), "does-not-exist"), someTmp, "stat source"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CopyForTakeover(context.Background(), tc.runID, tc.src, tc.baseDir)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q should contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

// TestCopyForTakeover_SourceIsAFile rejects a regular file passed as the
// source worktree path. Distinct test from the validation table because
// it needs filesystem setup.
func TestCopyForTakeover_SourceIsAFile(t *testing.T) {
	srcFile := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(srcFile, []byte("not a worktree"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	_, err := CopyForTakeover(context.Background(), "rid", srcFile, t.TempDir())
	if err == nil {
		t.Fatal("expected error when src is a file")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error %q should mention 'not a directory'", err.Error())
	}
}

// TestCopyForTakeover_ManagedDirsSkipped — _scratch/ and .git/ in the
// source must NOT be copied to the destination. The first is TF
// infrastructure that doesn't belong in the user's hands; the second
// would clobber the linked-worktree gitdir pointer.
func TestCopyForTakeover_ManagedDirsSkipped(t *testing.T) {
	_, srcWorktree := setupBareWithBranch(t)
	baseDir := t.TempDir()

	dir := filepath.Join(srcWorktree, "_scratch")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir _scratch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("nope"), 0644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	dest, err := CopyForTakeover(context.Background(), "skip-run", srcWorktree, baseDir)
	if err != nil {
		t.Fatalf("CopyForTakeover: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, "_scratch", "secret.txt")); !os.IsNotExist(err) {
		t.Errorf("_scratch/ should have been skipped, but file exists at dest (err=%v)", err)
	}

	// .git/ in the destination must be the linked-worktree pointer file
	// git wrote during `worktree add`, not anything overlaid from src.
	gitInfo, err := os.Stat(filepath.Join(dest, ".git"))
	if err != nil {
		t.Fatalf("stat dest .git: %v", err)
	}
	if gitInfo.IsDir() {
		t.Errorf("dest .git is a directory — overlay clobbered the linked-worktree pointer")
	}
}

// TestCopyForTakeover_NestedDirs verifies the walk recurses into
// subdirectories rather than only handling the top level. Trivial bug
// to introduce, expensive to debug in production.
func TestCopyForTakeover_NestedDirs(t *testing.T) {
	_, srcWorktree := setupBareWithBranch(t)
	nested := filepath.Join(srcWorktree, "a", "b", "c")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "deep.txt"), []byte("found me"), 0644); err != nil {
		t.Fatalf("write deep: %v", err)
	}

	dest, err := CopyForTakeover(context.Background(), "nested-run", srcWorktree, t.TempDir())
	if err != nil {
		t.Fatalf("CopyForTakeover: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "a", "b", "c", "deep.txt"))
	if err != nil {
		t.Fatalf("read nested file in dest: %v", err)
	}
	if string(got) != "found me" {
		t.Errorf("nested file content = %q", string(got))
	}
}

// --- CleanupWithOptions tests -----------------------------------------

// TestCleanupWithOptions_PreservesProjectDirForTakenOver is the startup-
// path safety: a taken_over run's ~/.claude/projects entry must survive
// the orphan sweep so `claude --resume` keeps working after a binary
// restart. The worktree dir under $TMPDIR still gets removed (it's
// genuine garbage), but the JSONL stays.
func TestCleanupWithOptions_PreservesProjectDirForTakenOver(t *testing.T) {
	tmp := t.TempDir()
	home := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("HOME", home)

	// Three orphaned worktrees: one normal run, one taken-over run, one
	// no-cwd Jira run that's also taken-over (covers the -nocwd suffix
	// trim in CleanupWithOptions).
	runs := []struct {
		dirName  string
		runID    string
		preserve bool
	}{
		{"run-normal", "run-normal", false},
		{"run-taken", "run-taken", true},
		{"run-jira-nocwd", "run-jira", true},
	}

	runsBase := filepath.Join(tmp, runsDir)
	if err := os.MkdirAll(runsBase, 0755); err != nil {
		t.Fatalf("mkdir runs base: %v", err)
	}
	for _, r := range runs {
		dir := filepath.Join(runsBase, r.dirName)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir worktree %s: %v", r.dirName, err)
		}
		// Pre-create the ~/.claude/projects/<encoded> entry that
		// Cleanup would otherwise nuke. Encoding matches what
		// RemoveClaudeProjectDir computes (slashes → dashes,
		// EvalSymlinks-resolved cwd).
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatalf("evalsymlinks %s: %v", dir, err)
		}
		encoded := encodeClaudeProjectDir(resolved)
		projectDir := filepath.Join(home, claudeProjectsDir, encoded)
		if err := os.MkdirAll(projectDir, 0755); err != nil {
			t.Fatalf("mkdir project dir for %s: %v", r.runID, err)
		}
		if err := os.WriteFile(filepath.Join(projectDir, "session.jsonl"), []byte("data"), 0644); err != nil {
			t.Fatalf("write jsonl: %v", err)
		}
	}

	preserve := map[string]bool{}
	for _, r := range runs {
		if r.preserve {
			preserve[r.runID] = true
		}
	}
	CleanupWithOptions(CleanupOptions{PreserveClaudeProjectFor: preserve})

	// All worktree dirs are removed regardless of preservation —
	// preserve only protects the project dir.
	for _, r := range runs {
		if _, err := os.Stat(filepath.Join(runsBase, r.dirName)); !os.IsNotExist(err) {
			t.Errorf("worktree dir %s should have been removed (err=%v)", r.dirName, err)
		}
	}

	// Project dirs: preserved runs keep theirs, non-preserved lose
	// theirs.
	for _, r := range runs {
		dir := filepath.Join(runsBase, r.dirName)
		// Re-derive the encoded name from the dir we already removed
		// — we have to rebuild it from scratch since the dir is gone.
		// Use the parent's resolved tmp path joined with the dirName,
		// which is what RemoveClaudeProjectDir would have computed
		// before deletion.
		tmpResolved, _ := filepath.EvalSymlinks(tmp)
		encoded := encodeClaudeProjectDir(filepath.Join(tmpResolved, runsDir, r.dirName))
		projectDir := filepath.Join(home, claudeProjectsDir, encoded)
		_, err := os.Stat(projectDir)
		if r.preserve {
			if os.IsNotExist(err) {
				t.Errorf("project dir for taken-over run %s was deleted; resume would break", r.runID)
			}
		} else {
			if !os.IsNotExist(err) {
				t.Errorf("project dir for non-preserved run %s should have been removed (err=%v)", r.runID, err)
			}
		}
		_ = dir
	}
}

// TestCleanupWithOptions_SkipClaudeProjectCleanup is the startup-DB-error
// path: when ListTakenOverRunIDs fails we can't tell which project dirs
// to preserve, so we skip ALL of them. Worktree dirs and bare-repo
// pruning should still run — those leaks compound fast and aren't
// session-state-sensitive. Without this option, a query failure would
// either leak hundreds of MB of worktree dirs (previous behavior:
// skip-everything) or risk wiping a real takeover's JSONL (initial
// behavior: empty preserve set + nuke).
func TestCleanupWithOptions_SkipClaudeProjectCleanup(t *testing.T) {
	tmp := t.TempDir()
	home := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("HOME", home)

	// One worktree dir + a corresponding ~/.claude/projects entry.
	wtDir := filepath.Join(tmp, runsDir, "run-skip")
	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(wtDir)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	encoded := encodeClaudeProjectDir(resolved)
	projectDir := filepath.Join(home, claudeProjectsDir, encoded)
	jsonlPath := filepath.Join(projectDir, "session.jsonl")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	if err := os.WriteFile(jsonlPath, []byte("data"), 0644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	CleanupWithOptions(CleanupOptions{SkipClaudeProjectCleanup: true})

	// Worktree dir gone — the leak we care about.
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Errorf("worktree dir should have been removed even with SkipClaudeProjectCleanup (err=%v)", err)
	}
	// Project dir preserved — we don't know if it belongs to a
	// takeover, so we err on the side of "leave it alone."
	if _, err := os.Stat(jsonlPath); err != nil {
		t.Errorf("session JSONL should have been preserved with SkipClaudeProjectCleanup (err=%v)", err)
	}
}

// TestMaterializeSessionForTakeover_CopiesJSONL is the regression
// guard for the "No conversation found with session ID" bug Claude
// Code raises when `claude --resume <id>` is run from a directory
// whose ~/.claude/projects/<encoded-cwd>/<id>.jsonl file doesn't
// exist. Claude Code keys session storage by encoded cwd, so without
// copying the JSONL across the user can't resume from the takeover
// dir even though the conversation history lives intact under the
// agent's original cwd encoding.
func TestMaterializeSessionForTakeover_CopiesJSONL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	oldCwd := filepath.Join(t.TempDir(), "agent-was-here")
	if err := os.MkdirAll(oldCwd, 0755); err != nil {
		t.Fatalf("mkdir oldCwd: %v", err)
	}
	newCwd := filepath.Join(t.TempDir(), "takeover-dest")
	if err := os.MkdirAll(newCwd, 0755); err != nil {
		t.Fatalf("mkdir newCwd: %v", err)
	}

	// Pre-create the source JSONL at where Claude Code would have
	// written it: ~/.claude/projects/<encoded-oldCwd>/<sessionID>.jsonl.
	sessionID := "abc123-def456"
	resolved := ResolveClaudeProjectCwd(oldCwd)
	srcEncoded := encodeClaudeProjectDir(resolved)
	srcDir := filepath.Join(home, claudeProjectsDir, srcEncoded)
	if err := os.MkdirAll(srcDir, 0700); err != nil {
		t.Fatalf("mkdir source project: %v", err)
	}
	jsonlContent := `{"type":"system","subtype":"init","session_id":"` + sessionID + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(srcDir, sessionID+".jsonl"), []byte(jsonlContent), 0644); err != nil {
		t.Fatalf("write source jsonl: %v", err)
	}

	if err := MaterializeSessionForTakeover(resolved, newCwd, sessionID); err != nil {
		t.Fatalf("MaterializeSessionForTakeover: %v", err)
	}

	// Destination JSONL must exist with the same content.
	newResolved := ResolveClaudeProjectCwd(newCwd)
	destEncoded := encodeClaudeProjectDir(newResolved)
	destPath := filepath.Join(home, claudeProjectsDir, destEncoded, sessionID+".jsonl")
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("read dest jsonl: %v", err)
	}
	if string(got) != jsonlContent {
		t.Errorf("dest content = %q, want %q", string(got), jsonlContent)
	}
}

// TestEncodeClaudeProjectDir_ReplacesDots is the headline regression
// test for the encoding bug. Empirically (Claude Code 2.1.119) every
// '/' AND every '.' in the resolved cwd becomes '-'. Triage Factory's
// own paths almost always contain dots — the takeover destination is
// ~/.triagefactory/takeovers/run-<id> — so a slash-only encoding
// silently misses Claude Code's actual lookup path and resume fails
// with "No conversation found." This test pins down both characters
// so the rule can't quietly regress.
func TestEncodeClaudeProjectDir_ReplacesDots(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/private/tmp/repro-orig", "-private-tmp-repro-orig"},
		{"/private/tmp/dot.in.middle", "-private-tmp-dot-in-middle"},
		{"/private/tmp/v1.2.3-rc", "-private-tmp-v1-2-3-rc"},
		// The case that actually broke in production: leading dot in
		// `.triagefactory` produces a `--` (two dashes in a row) where
		// the slash-after-dot used to live. Slash-only would have
		// produced just `-.triagefactory-...`.
		{"/Users/aidan/.triagefactory/takeovers/run-x", "-Users-aidan--triagefactory-takeovers-run-x"},
		{"/home/user/.triagefactory/takeovers/run-x", "-home-user--triagefactory-takeovers-run-x"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := encodeClaudeProjectDir(tc.in)
			if got != tc.want {
				t.Errorf("encodeClaudeProjectDir(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestMaterializeSessionForTakeover_MissingSource fails loudly when the
// source JSONL doesn't exist. Spawner.Takeover relies on this so it
// can roll back rather than mark the run taken_over with a non-
// resumable destination.
func TestMaterializeSessionForTakeover_MissingSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	oldCwd := filepath.Join(t.TempDir(), "no-jsonl-here")
	if err := os.MkdirAll(oldCwd, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	resolved := ResolveClaudeProjectCwd(oldCwd)

	err := MaterializeSessionForTakeover(resolved, t.TempDir(), "missing-session")
	if err == nil {
		t.Fatal("expected error when source JSONL is missing")
	}
	if !strings.Contains(err.Error(), "source JSONL") {
		t.Errorf("error %q should mention source JSONL", err.Error())
	}
}

// TestMaterializeSessionForTakeover_EmptySessionID rejects calls with
// no session id (defensive — should be caught upstream by Takeover's
// validation, but cheap to verify here).
func TestMaterializeSessionForTakeover_EmptySessionID(t *testing.T) {
	err := MaterializeSessionForTakeover("/tmp/x", "/tmp/y", "")
	if err == nil {
		t.Fatal("expected error on empty session id")
	}
}

// TestRemoveAt_HandlesNonCanonicalPath guards review-comment fix #3:
// callers like CopyForTakeover's overlay path and abortTakeover hold
// the source worktree path explicitly. If they used Remove(runID)
// (which derives the path from the canonical /tmp layout), they'd
// silently target the wrong directory whenever the source is
// elsewhere — leaking the actual source on disk and possibly
// destroying an unrelated runID's canonical dir.
//
// RemoveAt takes the path explicitly so this can't happen.
func TestRemoveAt_HandlesNonCanonicalPath(t *testing.T) {
	noncanonicalSrc := filepath.Join(t.TempDir(), "elsewhere", "wt-x")
	if err := os.MkdirAll(noncanonicalSrc, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Drop a marker file inside so we can verify the right dir is gone.
	if err := os.WriteFile(filepath.Join(noncanonicalSrc, "marker"), []byte("x"), 0644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	if err := RemoveAt(noncanonicalSrc, "run-x"); err != nil {
		t.Fatalf("RemoveAt: %v", err)
	}
	if _, err := os.Stat(noncanonicalSrc); !os.IsNotExist(err) {
		t.Errorf("non-canonical src should have been removed (err=%v)", err)
	}
}

// TestRemoveAt_EmptyPath is a no-op — guards against a caller passing
// an empty path (e.g., a no-worktree run that somehow reached this
// code path) from accidentally trying to RemoveAll("").
func TestRemoveAt_EmptyPath(t *testing.T) {
	if err := RemoveAt("", "run-y"); err != nil {
		t.Errorf("RemoveAt with empty path should be a no-op, got: %v", err)
	}
}

// TestCleanupWithOptions_NilPreserveSet is safe (no panic) and behaves
// like the legacy Cleanup() — every orphan's project dir gets nuked.
// Map reads on nil maps return the zero value in Go, so the index
// expression `opts.PreserveClaudeProjectFor[runID]` returns false and
// every run is treated as non-preserved.
func TestCleanupWithOptions_NilPreserveSet(t *testing.T) {
	tmp := t.TempDir()
	home := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("HOME", home)

	runDir := filepath.Join(tmp, runsDir, "run-nil")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("CleanupWithOptions panicked on nil PreserveClaudeProjectFor: %v", r)
		}
	}()
	CleanupWithOptions(CleanupOptions{}) // PreserveClaudeProjectFor is nil

	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Errorf("run dir should have been removed (err=%v)", err)
	}
}

// TestRemoveClaudeProjectDirUnderTakeover_RemovesEntryUnderBase verifies
// the happy path: a takeover-base-rooted cwd whose ~/.claude/projects
// entry exists is removed.
func TestRemoveClaudeProjectDirUnderTakeover_RemovesEntryUnderBase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	base := t.TempDir() // simulates ~/.triagefactory/takeovers
	cwd := filepath.Join(base, "run-abcd")
	if err := os.MkdirAll(cwd, 0755); err != nil {
		t.Fatalf("mkdir cwd: %v", err)
	}

	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}
	projectDir := filepath.Join(home, claudeProjectsDir, encodeClaudeProjectDir(resolved))
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "session.jsonl"), []byte("x"), 0644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	RemoveClaudeProjectDirUnderTakeover(cwd, base)

	if _, err := os.Stat(projectDir); !os.IsNotExist(err) {
		t.Errorf("project dir should have been removed (err=%v)", err)
	}
}

// TestRemoveClaudeProjectDirUnderTakeover_RefusesOutsideBase verifies the
// safety rail: a cwd outside the takeover base must NOT have its project
// dir removed, even if the home/projects entry exists.
func TestRemoveClaudeProjectDirUnderTakeover_RefusesOutsideBase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	base := t.TempDir()
	other := t.TempDir() // some unrelated path NOT under base
	cwd := filepath.Join(other, "run-evil")
	if err := os.MkdirAll(cwd, 0755); err != nil {
		t.Fatalf("mkdir cwd: %v", err)
	}

	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}
	projectDir := filepath.Join(home, claudeProjectsDir, encodeClaudeProjectDir(resolved))
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}

	RemoveClaudeProjectDirUnderTakeover(cwd, base)

	if _, err := os.Stat(projectDir); err != nil {
		t.Errorf("project dir was removed despite cwd being outside takeover base (err=%v)", err)
	}
}

// TestRemoveClaudeProjectDirUnderTakeover_EmptyArgs is a no-op when
// either argument is empty — guards against an unconfigured takeover
// dir or a release call with a missing worktree_path.
func TestRemoveClaudeProjectDirUnderTakeover_EmptyArgs(t *testing.T) {
	// No-op: should not panic, should not touch anything. Hard to
	// observe directly other than not-crashing.
	RemoveClaudeProjectDirUnderTakeover("", "/some/base")
	RemoveClaudeProjectDirUnderTakeover("/some/cwd", "")
	RemoveClaudeProjectDirUnderTakeover("", "")
}

// TestRemoveClaudeProjectDirForResolved_RemovesEntryWhenCwdGone is the
// load-bearing case for the release path: by the time we want to remove
// the projects entry, RemoveAt has already destroyed the cwd. The
// resolved-path variant must work even though EvalSymlinks(cwd) would
// fail.
func TestRemoveClaudeProjectDirForResolved_RemovesEntryWhenCwdGone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Create the cwd just long enough to capture the resolved path,
	// then remove it to mimic the post-RemoveAt state. The encoded
	// projects-dir name is what we're really testing — it has to be
	// derivable without re-resolving the cwd.
	tmp := t.TempDir()
	cwd := filepath.Join(tmp, "run-gone")
	if err := os.MkdirAll(cwd, 0755); err != nil {
		t.Fatalf("mkdir cwd: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}
	projectDir := filepath.Join(home, claudeProjectsDir, encodeClaudeProjectDir(resolved))
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "session.jsonl"), []byte("x"), 0644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	// Remove the cwd — exactly the state Release is in after RemoveAt.
	if err := os.RemoveAll(cwd); err != nil {
		t.Fatalf("remove cwd: %v", err)
	}

	RemoveClaudeProjectDirForResolved(resolved)

	if _, err := os.Stat(projectDir); !os.IsNotExist(err) {
		t.Errorf("project dir should have been removed despite cwd being gone (err=%v)", err)
	}
}
