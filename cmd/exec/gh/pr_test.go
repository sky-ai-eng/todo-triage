package gh

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// newTestServer builds an httptest server whose handler dispatches on path suffix.
// diffHandler is called for requests that look like the diff endpoint (no /files suffix),
// filesHandler is called for requests to /files.
func newTestServer(t *testing.T, diffHandler, filesHandler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/files") {
			filesHandler(w, r)
		} else {
			diffHandler(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// jsonPRFiles encodes a slice of file maps as the response body GitHub would return
// for the PR files endpoint. Each map should have at minimum "filename" and "patch".
func jsonPRFiles(t *testing.T, files []map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(files)
	if err != nil {
		t.Fatalf("marshal PR files: %v", err)
	}
	return data
}

// TestGetDiffLines_NormalDiff verifies the happy path: the diff endpoint returns
// a valid unified diff and getDiffLines parses it into a file→commentable-lines map.
func TestGetDiffLines_NormalDiff(t *testing.T) {
	diffContent := "diff --git a/foo.go b/foo.go\n@@ -1,2 +1,2 @@\n context\n-old\n+new\n"

	srv := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(diffContent))
		},
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("files endpoint should not be called on a successful diff fetch")
			http.Error(w, "unexpected call", http.StatusInternalServerError)
		},
	)

	client := ghclient.NewClient(srv.URL, "test-token")
	result, err := getDiffLines(client, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("getDiffLines: %v", err)
	}
	if _, ok := result["foo.go"]; !ok {
		t.Errorf("expected foo.go in result, got keys: %v", keys(result))
	}
	if !result["foo.go"][1] || !result["foo.go"][2] {
		t.Errorf("expected lines 1 and 2 commentable for foo.go, got %v", result["foo.go"])
	}
}

// TestGetDiffLines_406FallsBackToFiles verifies that when the diff endpoint
// returns HTTP 406, getDiffLines falls back to GetPRFiles + DiffLinesFromPatches.
func TestGetDiffLines_406FallsBackToFiles(t *testing.T) {
	filesPayload := jsonPRFiles(t, []map[string]any{
		{
			"filename":  "a.go",
			"status":    "modified",
			"additions": 1,
			"deletions": 1,
			"patch":     "@@ -1,2 +1,2 @@\n context\n-old\n+new\n",
		},
		{
			"filename":  "b.go",
			"status":    "added",
			"additions": 2,
			"deletions": 0,
			"patch":     "@@ -0,0 +1,2 @@\n+line1\n+line2\n",
		},
	})

	srv := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"diff too large"}`, http.StatusNotAcceptable)
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(filesPayload)
		},
	)

	client := ghclient.NewClient(srv.URL, "test-token")
	result, err := getDiffLines(client, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("getDiffLines with 406 fallback: %v", err)
	}

	// a.go: context line 1, added line 2
	if _, ok := result["a.go"]; !ok {
		t.Errorf("expected a.go in fallback result, got: %v", keys(result))
	}
	if !result["a.go"][1] || !result["a.go"][2] {
		t.Errorf("expected lines 1,2 commentable for a.go, got %v", result["a.go"])
	}

	// b.go: two added lines
	if _, ok := result["b.go"]; !ok {
		t.Errorf("expected b.go in fallback result, got: %v", keys(result))
	}
	if !result["b.go"][1] || !result["b.go"][2] {
		t.Errorf("expected lines 1,2 commentable for b.go, got %v", result["b.go"])
	}
}

// TestGetDiffLines_406EmptyFileList verifies the fallback works even when the
// files endpoint returns an empty list (e.g., all files are binary-only).
func TestGetDiffLines_406EmptyFileList(t *testing.T) {
	srv := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"diff too large"}`, http.StatusNotAcceptable)
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		},
	)

	client := ghclient.NewClient(srv.URL, "test-token")
	result, err := getDiffLines(client, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("getDiffLines with 406 + empty files: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty result for empty file list, got: %v", result)
	}
}

// TestGetDiffLines_406FilesEndpointFails verifies that when the diff endpoint
// returns 406 AND the files fallback also fails, the files error is returned.
func TestGetDiffLines_406FilesEndpointFails(t *testing.T) {
	srv := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"diff too large"}`, http.StatusNotAcceptable)
		},
		func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"internal server error"}`, http.StatusInternalServerError)
		},
	)

	client := ghclient.NewClient(srv.URL, "test-token")
	_, err := getDiffLines(client, "owner", "repo", 42)
	if err == nil {
		t.Fatal("expected error when 406 and files endpoint also fails, got nil")
	}
}

// TestGetDiffLines_OtherErrorPropagates verifies that non-406 errors from the
// diff endpoint are NOT silently swallowed — the fallback must NOT be triggered.
func TestGetDiffLines_OtherErrorPropagates(t *testing.T) {
	filesCallCount := 0
	srv := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		},
		func(w http.ResponseWriter, r *http.Request) {
			filesCallCount++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		},
	)

	client := ghclient.NewClient(srv.URL, "test-token")
	_, err := getDiffLines(client, "owner", "repo", 42)
	if err == nil {
		t.Fatal("expected error on 404 from diff endpoint, got nil")
	}
	if filesCallCount > 0 {
		t.Errorf("files endpoint should NOT be called for non-406 errors, got %d calls", filesCallCount)
	}
}

// TestGetDiffLines_406BinaryFile verifies that a 406 fallback handles PRs that
// include binary files (missing patch field) without crashing and produces
// an empty line set for those files.
func TestGetDiffLines_406BinaryFile(t *testing.T) {
	filesPayload := jsonPRFiles(t, []map[string]any{
		{
			"filename":  "image.png",
			"status":    "added",
			"additions": 0,
			"deletions": 0,
			// no "patch" field — binary file
		},
		{
			"filename":  "main.go",
			"status":    "modified",
			"additions": 1,
			"deletions": 0,
			"patch":     "@@ -1,1 +1,2 @@\n line1\n+line2\n",
		},
	})

	srv := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"diff too large"}`, http.StatusNotAcceptable)
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(filesPayload)
		},
	)

	client := ghclient.NewClient(srv.URL, "test-token")
	result, err := getDiffLines(client, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("getDiffLines binary file: %v", err)
	}

	// binary file should be present but have no commentable lines
	if _, ok := result["image.png"]; !ok {
		t.Error("expected image.png in result")
	}
	if len(result["image.png"]) != 0 {
		t.Errorf("expected no commentable lines for binary file, got %v", result["image.png"])
	}

	// text file should have correct commentable lines
	if !result["main.go"][1] || !result["main.go"][2] {
		t.Errorf("expected lines 1,2 commentable for main.go, got %v", result["main.go"])
	}
}

// TestStripClaudeCodeCitation pins the rules for trimming Claude
// Code's auto-citation off PR bodies before they hit the queue.
// The TF footer (added at submit time) is the prominent
// attribution; letting Claude Code's citation through would crowd
// it out and double-bill the PR.
func TestStripClaudeCodeCitation(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "typical Claude Code PR body",
			in:   "Summary of changes.\n\n## Test plan\n- run tests\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n",
			want: "Summary of changes.\n\n## Test plan\n- run tests",
		},
		{
			name: "no citation: passthrough",
			in:   "Summary of changes.\n\n## Test plan\n- run tests\n",
			want: "Summary of changes.\n\n## Test plan\n- run tests\n",
		},
		{
			name: "citation alone",
			in:   "🤖 Generated with [Claude Code](https://claude.com/claude-code)",
			want: "",
		},
		{
			name: "trailing whitespace before citation",
			in:   "Body.\n\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n\n\n",
			want: "Body.",
		},
		{
			name: "mid-body citation: leave alone",
			in:   "Citing 🤖 Generated with [Claude Code](https://claude.com/claude-code) in context.\n\nFinal sentence.",
			want: "Citing 🤖 Generated with [Claude Code](https://claude.com/claude-code) in context.\n\nFinal sentence.",
		},
		{
			name: "citation without leading emoji",
			in:   "Body.\n\nGenerated with [Claude Code](https://claude.com/claude-code)",
			want: "Body.",
		},
		{
			name: "empty body",
			in:   "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripClaudeCodeCitation(tc.in)
			if got != tc.want {
				t.Errorf("stripClaudeCodeCitation:\n--- got ---\n%q\n--- want ---\n%q", got, tc.want)
			}
		})
	}
}
