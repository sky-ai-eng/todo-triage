package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// servePRFixture returns a handler that responds to the four URLs
// GetPR hits. The PR body comes from prJSON; reviews and comments
// endpoints all return empty arrays. Anything else fails the test —
// surfacing accidental new GetPR call sites that would silently 404
// with the rest of the unit tests still passing.
func servePRFixture(t *testing.T, prJSON string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/42") ||
			strings.HasSuffix(r.URL.Path, "/pulls/7") ||
			strings.HasSuffix(r.URL.Path, "/pulls/99"):
			_, _ = w.Write([]byte(prJSON))
		case strings.Contains(r.URL.Path, "/pulls/") && (strings.HasSuffix(r.URL.Path, "/reviews") || strings.HasSuffix(r.URL.Path, "/comments")):
			_, _ = w.Write([]byte("[]"))
		case strings.Contains(r.URL.Path, "/issues/") && strings.HasSuffix(r.URL.Path, "/comments"):
			_, _ = w.Write([]byte("[]"))
		default:
			t.Errorf("unexpected URL: %s", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
}

// TestGetPR_ForkPR_ParsesHeadAndBaseCloneURLs locks down the parsing
// of base.repo.clone_url and head.repo.clone_url. setupGitHub depends
// on the upstream URL coming from base.repo.clone_url (anything else
// would point the bare's origin at a fork) and on head.repo.clone_url
// for fork-tracking configuration. If the GitHub API ever moves
// these fields or the parser regresses, this test catches it before
// every PR delegation starts pushing to the wrong place.
func TestGetPR_ForkPR_ParsesHeadAndBaseCloneURLs(t *testing.T) {
	prJSON := `{
		"number": 42,
		"title": "Fork PR",
		"state": "open",
		"head": {
			"ref": "feature-branch",
			"sha": "abc123",
			"repo": {"clone_url": "https://github.com/contributor/forked-repo.git"}
		},
		"base": {
			"ref": "main",
			"repo": {"clone_url": "https://github.com/upstream-owner/upstream-repo.git"}
		}
	}`
	srv := httptest.NewServer(servePRFixture(t, prJSON))
	t.Cleanup(srv.Close)

	pr, err := clientAgainst(srv.URL).GetPR("upstream-owner", "upstream-repo", 42, false)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if pr.CloneURL != "https://github.com/contributor/forked-repo.git" {
		t.Errorf("CloneURL (head fork URL) = %q, want %q", pr.CloneURL, "https://github.com/contributor/forked-repo.git")
	}
	if pr.BaseCloneURL != "https://github.com/upstream-owner/upstream-repo.git" {
		t.Errorf("BaseCloneURL (upstream URL) = %q, want %q", pr.BaseCloneURL, "https://github.com/upstream-owner/upstream-repo.git")
	}
	if pr.HeadRef != "feature-branch" {
		t.Errorf("HeadRef = %q, want %q", pr.HeadRef, "feature-branch")
	}
	if pr.BaseRef != "main" {
		t.Errorf("BaseRef = %q, want %q", pr.BaseRef, "main")
	}
}

// TestGetPR_OwnRepoPR_HeadAndBaseEqual verifies that when head.repo
// and base.repo point at the same repo, both clone URLs come back
// identical. The spawner uses this equality to decide whether to
// skip the fork-tracking setup; if the parser ever fails to populate
// one of them, the spawner would treat an own-repo PR as a fork
// (or vice versa) and configure pushes incorrectly.
func TestGetPR_OwnRepoPR_HeadAndBaseEqual(t *testing.T) {
	prJSON := `{
		"number": 7,
		"title": "Own PR",
		"state": "open",
		"head": {
			"ref": "my-feature",
			"sha": "def456",
			"repo": {"clone_url": "https://github.com/me/myrepo.git"}
		},
		"base": {
			"ref": "main",
			"repo": {"clone_url": "https://github.com/me/myrepo.git"}
		}
	}`
	srv := httptest.NewServer(servePRFixture(t, prJSON))
	t.Cleanup(srv.Close)

	pr, err := clientAgainst(srv.URL).GetPR("me", "myrepo", 7, false)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if pr.CloneURL == "" || pr.BaseCloneURL == "" {
		t.Fatalf("expected both URLs populated; got CloneURL=%q BaseCloneURL=%q", pr.CloneURL, pr.BaseCloneURL)
	}
	if pr.CloneURL != pr.BaseCloneURL {
		t.Errorf("own-repo PR: head and base clone URLs should be equal; got CloneURL=%q BaseCloneURL=%q", pr.CloneURL, pr.BaseCloneURL)
	}
}

// TestGetPR_DeletedFork_BaseStillPopulated covers the GitHub edge
// case where head.repo is null because the contributor's fork was
// deleted. The parser must leave CloneURL empty (not panic on the
// null) AND still populate BaseCloneURL and HeadRef so deleted-fork
// PRs can still be recognized and handled using the base repository
// metadata, including creating a read-only worktree when needed.
func TestGetPR_DeletedFork_BaseStillPopulated(t *testing.T) {
	prJSON := `{
		"number": 99,
		"title": "Deleted-fork PR",
		"state": "closed",
		"head": {
			"ref": "deleted-branch",
			"sha": "fff999",
			"repo": null
		},
		"base": {
			"ref": "main",
			"repo": {"clone_url": "https://github.com/me/myrepo.git"}
		}
	}`
	srv := httptest.NewServer(servePRFixture(t, prJSON))
	t.Cleanup(srv.Close)

	pr, err := clientAgainst(srv.URL).GetPR("me", "myrepo", 99, false)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if pr.CloneURL != "" {
		t.Errorf("CloneURL should be empty when head.repo is null; got %q", pr.CloneURL)
	}
	if pr.BaseCloneURL != "https://github.com/me/myrepo.git" {
		t.Errorf("BaseCloneURL = %q, want %q (must survive deleted-fork)", pr.BaseCloneURL, "https://github.com/me/myrepo.git")
	}
	if pr.HeadRef != "deleted-branch" {
		t.Errorf("HeadRef = %q, want %q (head.ref still parseable when repo is null)", pr.HeadRef, "deleted-branch")
	}
}

func makePRFilesList(count int, prefix string) []map[string]any {
	files := make([]map[string]any, count)
	for i := range files {
		files[i] = map[string]any{
			"filename":  fmt.Sprintf("%s_file_%d.go", prefix, i),
			"status":    "modified",
			"additions": 1,
			"deletions": 1,
			"patch":     "@@ -1,1 +1,1 @@\n+new\n",
		}
	}
	return files
}

func TestGetPRFiles_SinglePage(t *testing.T) {
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		data, _ := json.Marshal(makePRFilesList(50, "p1"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	got, err := c.GetPRFiles("owner", "repo", 1)
	if err != nil {
		t.Fatalf("GetPRFiles: %v", err)
	}
	if len(got) != 50 {
		t.Errorf("expected 50 files, got %d", len(got))
	}
	if callCount != 1 {
		t.Errorf("expected 1 API call, got %d", callCount)
	}
}

func TestGetPRFiles_MultiPage(t *testing.T) {
	// page 1: 100, page 2: 100, page 3: 30 → 230 total, 3 calls
	pageHits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page == "" {
			page = "1"
		}
		pageHits[page]++

		var count int
		switch page {
		case "1":
			count = 100
		case "2":
			count = 100
		case "3":
			count = 30
		default:
			t.Errorf("unexpected page %s requested", page)
			http.Error(w, "unexpected page", http.StatusBadRequest)
			return
		}

		data, _ := json.Marshal(makePRFilesList(count, "p"+page))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	got, err := c.GetPRFiles("owner", "repo", 1)
	if err != nil {
		t.Fatalf("GetPRFiles multi-page: %v", err)
	}
	if len(got) != 230 {
		t.Errorf("expected 230 files, got %d", len(got))
	}
	for _, pg := range []string{"1", "2", "3"} {
		if pageHits[pg] != 1 {
			t.Errorf("expected 1 hit for page %s, got %d", pg, pageHits[pg])
		}
	}
}

func TestGetPRFiles_CapAt1000(t *testing.T) {
	// Every page returns 100 files; should stop after 10 pages (1000 total).
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		data, _ := json.Marshal(makePRFilesList(100, fmt.Sprintf("call%d", callCount)))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	got, err := c.GetPRFiles("owner", "repo", 1)
	if err != nil {
		t.Fatalf("GetPRFiles cap: %v", err)
	}
	if len(got) != maxPRFiles {
		t.Errorf("expected %d files (cap), got %d", maxPRFiles, len(got))
	}
	if callCount != maxPRFiles/100 {
		t.Errorf("expected %d API calls, got %d", maxPRFiles/100, callCount)
	}
}

func TestGetPRFiles_ErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	_, err := c.GetPRFiles("owner", "repo", 1)
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
}

func TestGetPRFiles_SecondPageError(t *testing.T) {
	// First page succeeds, second page fails — error should propagate.
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			data, _ := json.Marshal(makePRFilesList(100, "p1"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)
			return
		}
		http.Error(w, `{"message":"rate limited"}`, http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	_, err := c.GetPRFiles("owner", "repo", 1)
	if err == nil {
		t.Fatal("expected error when second page fails, got nil")
	}
}
