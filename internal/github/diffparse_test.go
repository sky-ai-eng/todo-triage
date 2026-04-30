package github

import (
	"testing"
)

// intSetEqual compares two map[int]bool values for equality.
func intSetEqual(a, b map[int]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// --- parsePatchLines ---

func TestParsePatchLines_BasicPatch(t *testing.T) {
	// Added, deleted, and context lines — only added and context are commentable.
	patch := "@@ -1,4 +1,4 @@\n line1\n-old line\n+new line\n line3\n line4"
	got := parsePatchLines(patch)
	want := map[int]bool{1: true, 2: true, 3: true, 4: true}
	if !intSetEqual(got, want) {
		t.Errorf("parsePatchLines basic = %v, want %v", got, want)
	}
}

func TestParsePatchLines_TrailingNewlineNotCounted(t *testing.T) {
	// A patch ending with \n produces a trailing empty string from strings.Split.
	// That empty string must NOT be recorded as a commentable line.
	patch := "@@ -1,2 +1,2 @@\n line1\n+added\n"
	got := parsePatchLines(patch)
	want := map[int]bool{1: true, 2: true}
	if !intSetEqual(got, want) {
		t.Errorf("parsePatchLines trailing newline = %v, want %v", got, want)
	}
	if got[3] {
		t.Error("line 3 should not be commentable (trailing empty string from split)")
	}
}

func TestParsePatchLines_NoNewlineAtEndOfFile(t *testing.T) {
	// The "\ No newline at end of file" marker must not be counted as a line.
	patch := "@@ -1,1 +1,1 @@\n-old\n+new\n\\ No newline at end of file"
	got := parsePatchLines(patch)
	want := map[int]bool{1: true}
	if !intSetEqual(got, want) {
		t.Errorf("parsePatchLines no-newline marker = %v, want %v", got, want)
	}
}

func TestParsePatchLines_EmptyPatch(t *testing.T) {
	got := parsePatchLines("")
	if len(got) != 0 {
		t.Errorf("parsePatchLines empty = %v, want empty map", got)
	}
}

func TestParsePatchLines_OnlyDeletions(t *testing.T) {
	// All removed lines; new side has no content.
	patch := "@@ -1,3 +0,0 @@\n-line1\n-line2\n-line3"
	got := parsePatchLines(patch)
	if len(got) != 0 {
		t.Errorf("parsePatchLines only deletions = %v, want empty", got)
	}
}

func TestParsePatchLines_MultipleHunks(t *testing.T) {
	// Two hunks at different positions in the file.
	patch := "@@ -1,3 +1,3 @@\n ctx1\n-del1\n+add1\n@@ -10,3 +10,3 @@\n ctx10\n-del10\n+add10"
	got := parsePatchLines(patch)
	// Hunk 1: new-side lines 1 (ctx1), 2 (add1)
	// Hunk 2: new-side lines 10 (ctx10), 11 (add10)
	want := map[int]bool{1: true, 2: true, 10: true, 11: true}
	if !intSetEqual(got, want) {
		t.Errorf("parsePatchLines multiple hunks = %v, want %v", got, want)
	}
}

func TestParsePatchLines_OnlyAdditions(t *testing.T) {
	// New file — every line is an addition.
	patch := "@@ -0,0 +1,3 @@\n+line1\n+line2\n+line3"
	got := parsePatchLines(patch)
	want := map[int]bool{1: true, 2: true, 3: true}
	if !intSetEqual(got, want) {
		t.Errorf("parsePatchLines only additions = %v, want %v", got, want)
	}
}

func TestParsePatchLines_ContextOnly(t *testing.T) {
	// Pure context (unchanged) lines are still commentable.
	patch := "@@ -5,3 +5,3 @@\n line5\n line6\n line7"
	got := parsePatchLines(patch)
	want := map[int]bool{5: true, 6: true, 7: true}
	if !intSetEqual(got, want) {
		t.Errorf("parsePatchLines context only = %v, want %v", got, want)
	}
}

// --- DiffLinesFromPatches ---

func TestDiffLinesFromPatches_SingleFile(t *testing.T) {
	files := []PRFile{
		{Filename: "a.go", Patch: "@@ -1,2 +1,2 @@\n ctx\n+added\n"},
	}
	got := DiffLinesFromPatches(files)
	if !got["a.go"][1] || !got["a.go"][2] {
		t.Errorf("a.go: expected lines 1,2 commentable, got %v", got["a.go"])
	}
	if got["a.go"][3] {
		t.Error("a.go: line 3 should not be commentable (trailing newline)")
	}
}

func TestDiffLinesFromPatches_MultipleFiles(t *testing.T) {
	files := []PRFile{
		{Filename: "a.go", Patch: "@@ -1,1 +1,1 @@\n+lineA\n"},
		{Filename: "b.go", Patch: "@@ -1,1 +1,1 @@\n+lineB\n"},
	}
	got := DiffLinesFromPatches(files)
	if _, ok := got["a.go"]; !ok {
		t.Error("expected a.go in result")
	}
	if _, ok := got["b.go"]; !ok {
		t.Error("expected b.go in result")
	}
	if !got["a.go"][1] {
		t.Errorf("a.go line 1 should be commentable: %v", got["a.go"])
	}
	if !got["b.go"][1] {
		t.Errorf("b.go line 1 should be commentable: %v", got["b.go"])
	}
}

func TestDiffLinesFromPatches_BinaryFileNoPatch(t *testing.T) {
	// Binary files have no patch — should produce an empty (but present) entry.
	files := []PRFile{
		{Filename: "image.png", Patch: ""},
	}
	got := DiffLinesFromPatches(files)
	if _, ok := got["image.png"]; !ok {
		t.Error("expected image.png key to be present even with empty patch")
	}
	if len(got["image.png"]) != 0 {
		t.Errorf("expected no commentable lines for binary file, got %v", got["image.png"])
	}
}

func TestDiffLinesFromPatches_EmptyFileList(t *testing.T) {
	got := DiffLinesFromPatches(nil)
	if len(got) != 0 {
		t.Errorf("expected empty result for nil files, got %v", got)
	}
}

// --- DiffLines ---

func TestDiffLines_BasicDiff(t *testing.T) {
	diff := "diff --git a/foo.go b/foo.go\n@@ -1,4 +1,4 @@\n context\n-old\n+new\n line3\n line4"
	got := DiffLines(diff)
	want := map[int]bool{1: true, 2: true, 3: true, 4: true}
	if !intSetEqual(got["foo.go"], want) {
		t.Errorf("DiffLines basic = %v, want %v", got["foo.go"], want)
	}
}

func TestDiffLines_TrailingNewlineNotCounted(t *testing.T) {
	// A diff ending with \n must not add a spurious line via the trailing empty string.
	diff := "diff --git a/foo.go b/foo.go\n@@ -1,2 +1,2 @@\n context\n+new\n"
	got := DiffLines(diff)
	want := map[int]bool{1: true, 2: true}
	if !intSetEqual(got["foo.go"], want) {
		t.Errorf("DiffLines trailing newline = %v, want %v", got["foo.go"], want)
	}
	if got["foo.go"][3] {
		t.Error("line 3 should not be commentable (trailing empty string from split)")
	}
}

func TestDiffLines_MultipleFiles(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n@@ -1,1 +1,1 @@\n+lineA\ndiff --git a/b.go b/b.go\n@@ -1,1 +1,1 @@\n+lineB"
	got := DiffLines(diff)
	if _, ok := got["a.go"]; !ok {
		t.Error("expected a.go in result")
	}
	if _, ok := got["b.go"]; !ok {
		t.Error("expected b.go in result")
	}
	if !got["a.go"][1] {
		t.Errorf("a.go line 1 should be commentable: %v", got["a.go"])
	}
	if !got["b.go"][1] {
		t.Errorf("b.go line 1 should be commentable: %v", got["b.go"])
	}
}

func TestDiffLines_EmptyDiff(t *testing.T) {
	got := DiffLines("")
	if len(got) != 0 {
		t.Errorf("DiffLines empty = %v, want empty map", got)
	}
}
