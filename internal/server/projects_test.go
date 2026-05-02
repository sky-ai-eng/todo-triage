package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func TestProjectCreate_Happy(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/projects", map[string]any{
		"name":         "Triage Factory",
		"description":  "Local-first triage UI",
		"pinned_repos": []string{"sky-ai-eng/triage-factory"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	var got domain.Project
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID == "" {
		t.Error("expected generated id")
	}
	if got.Name != "Triage Factory" {
		t.Errorf("name = %q", got.Name)
	}
	if len(got.PinnedRepos) != 1 || got.PinnedRepos[0] != "sky-ai-eng/triage-factory" {
		t.Errorf("pinned_repos = %v", got.PinnedRepos)
	}
}

func TestProjectCreate_RejectsEmptyName(t *testing.T) {
	s := newTestServer(t)
	for _, name := range []string{"", "   ", "\t"} {
		rec := doJSON(t, s, http.MethodPost, "/api/projects", map[string]any{"name": name})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("name=%q status = %d, want 400", name, rec.Code)
		}
	}
}

func TestProjectCreate_RejectsBadPinnedRepoSlugs(t *testing.T) {
	s := newTestServer(t)
	bad := [][]string{
		{""},
		{"  "},
		{"justaword"},
		{"too/many/slashes"},
		{"/missing-owner"},
		{"missing-repo/"},
	}
	for _, repos := range bad {
		rec := doJSON(t, s, http.MethodPost, "/api/projects", map[string]any{
			"name":         "P",
			"pinned_repos": repos,
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("repos=%v status = %d, want 400", repos, rec.Code)
		}
	}
}

func TestProjectGet_404OnMissing(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodGet, "/api/projects/no-such-id", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestProjectList_EmptyReturnsArray(t *testing.T) {
	// The handler must return `[]`, not `null` — a frontend that
	// .map()s the response would crash on null.
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodGet, "/api/projects", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "[]" {
		t.Errorf("body = %q, want []", body)
	}
}

func TestProjectPatch_PartialFieldsLeaveOthersUnchanged(t *testing.T) {
	s := newTestServer(t)
	id, err := db.CreateProject(s.db, domain.Project{
		Name:        "Original",
		Description: "Original description",
		PinnedRepos: []string{"a/b"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"description": "Updated description",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got domain.Project
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Name != "Original" {
		t.Errorf("name changed unexpectedly: %q", got.Name)
	}
	if got.Description != "Updated description" {
		t.Errorf("description = %q", got.Description)
	}
	if len(got.PinnedRepos) != 1 || got.PinnedRepos[0] != "a/b" {
		t.Errorf("pinned_repos changed unexpectedly: %v", got.PinnedRepos)
	}
}

// TestProjectPatch_PinnedReposExplicitEmptyClears confirms a client
// can clear pinned_repos by sending []. The pointer-typed *[]string
// distinguishes "absent (leave alone)" from "explicit empty (clear)";
// without that distinction the handler couldn't tell the cases apart.
func TestProjectPatch_PinnedReposExplicitEmptyClears(t *testing.T) {
	s := newTestServer(t)
	id, _ := db.CreateProject(s.db, domain.Project{Name: "P", PinnedRepos: []string{"a/b"}})

	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"pinned_repos": []string{},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	got, _ := db.GetProject(s.db, id)
	if len(got.PinnedRepos) != 0 {
		t.Errorf("pinned_repos should be empty, got %v", got.PinnedRepos)
	}
}

func TestProjectPatch_RejectsEmptyName(t *testing.T) {
	s := newTestServer(t)
	id, _ := db.CreateProject(s.db, domain.Project{Name: "P"})
	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{"name": "  "})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestProjectPatch_404OnMissing(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPatch, "/api/projects/no-such-id", map[string]any{"name": "X"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestProjectDelete_Happy(t *testing.T) {
	s := newTestServer(t)
	id, _ := db.CreateProject(s.db, domain.Project{Name: "doomed"})

	rec := doJSON(t, s, http.MethodDelete, "/api/projects/"+id, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	got, _ := db.GetProject(s.db, id)
	if got != nil {
		t.Errorf("project still readable after delete")
	}
}

func TestProjectDelete_404OnMissing(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodDelete, "/api/projects/no-such-id", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestProjectDelete_RemovesKnowledgeDir verifies the handler clears
// the on-disk knowledge dir. Tested via a fake HOME so we don't
// touch the real ~/.triagefactory.
func TestProjectDelete_RemovesKnowledgeDir(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	s := newTestServer(t)
	id, _ := db.CreateProject(s.db, domain.Project{Name: "with-files"})

	dir := filepath.Join(tempHome, ".triagefactory", "projects", id, "knowledge-base")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("notes"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	rec := doJSON(t, s, http.MethodDelete, "/api/projects/"+id, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}

	stillThere := filepath.Join(tempHome, ".triagefactory", "projects", id)
	if _, err := os.Stat(stillThere); !os.IsNotExist(err) {
		t.Errorf("knowledge dir not removed: stat err = %v", err)
	}
}

// TestProjectDelete_MissingKnowledgeDir_NoError pins the
// "delete is best-effort on disk" contract: a project with no
// on-disk artifacts must still 204, not 500.
func TestProjectDelete_MissingKnowledgeDir_NoError(t *testing.T) {
	s := newTestServer(t)
	id, _ := db.CreateProject(s.db, domain.Project{Name: "no-files"})
	rec := doJSON(t, s, http.MethodDelete, "/api/projects/"+id, nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

// TestProjectDelete_CleanupWarningRedactsPath pins the path-leak
// fix: when on-disk cleanup fails, the X-Cleanup-Warning header
// must be a generic message, not rmErr.Error() (which would
// include absolute paths and OS-specific detail). Forces failure
// by dropping write perms on the parent dir so RemoveAll can't
// clear the contents.
func TestProjectDelete_CleanupWarningRedactsPath(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	s := newTestServer(t)
	id, _ := db.CreateProject(s.db, domain.Project{Name: "padded"})

	dir := filepath.Join(tempHome, ".triagefactory", "projects", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret-path-leak.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Drop write perms on the dir so RemoveAll fails to delete the
	// child. Restore in cleanup so t.TempDir's own cleanup works.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	rec := doJSON(t, s, http.MethodDelete, "/api/projects/"+id, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	warning := rec.Header().Get("X-Cleanup-Warning")
	if warning == "" {
		t.Fatal("expected X-Cleanup-Warning header, got empty")
	}
	if strings.Contains(warning, tempHome) || strings.Contains(warning, dir) {
		t.Errorf("warning leaks filesystem path: %q", warning)
	}
	if strings.Contains(warning, "secret-path-leak.md") {
		t.Errorf("warning leaks filename: %q", warning)
	}
}

func TestValidatePinnedRepos_Slugs(t *testing.T) {
	good := [][]string{
		nil,
		{},
		{"a/b"},
		{"sky-ai-eng/triage-factory", "owner/repo"},
	}
	for _, repos := range good {
		if _, errMsg := validatePinnedRepos(repos); errMsg != "" {
			t.Errorf("repos=%v should pass, got %q", repos, errMsg)
		}
	}
	bad := [][]string{
		{""},
		{"  "},
		{"justaword"},
		{"a/b/c"},
		{"/x"},
		{"x/"},
	}
	for _, repos := range bad {
		if _, errMsg := validatePinnedRepos(repos); errMsg == "" {
			t.Errorf("repos=%v should reject", repos)
		}
	}
}

// TestValidatePinnedRepos_NormalizesWhitespace pins the
// trim-and-persist contract: validation strips whitespace AND the
// caller persists the trimmed slugs. Without normalization,
// " owner/repo " would pass (validator trims) but get stored
// padded, breaking later lookups by slug.
func TestValidatePinnedRepos_NormalizesWhitespace(t *testing.T) {
	in := []string{"  owner/repo  ", "\tother/repo\n"}
	out, errMsg := validatePinnedRepos(in)
	if errMsg != "" {
		t.Fatalf("expected pass, got %q", errMsg)
	}
	want := []string{"owner/repo", "other/repo"}
	if len(out) != len(want) {
		t.Fatalf("len = %d, want %d", len(out), len(want))
	}
	for i, w := range want {
		if out[i] != w {
			t.Errorf("[%d] = %q, want %q", i, out[i], w)
		}
	}
}

// TestProjectCreate_PaddedSlugsStoredTrimmed is the end-to-end
// regression: padded input from a client must round-trip back as
// trimmed. Without the normalization fix this test fails because
// the original padded string gets persisted.
func TestProjectCreate_PaddedSlugsStoredTrimmed(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/projects", map[string]any{
		"name":         "P",
		"pinned_repos": []string{"  owner/repo  "},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	var got domain.Project
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.PinnedRepos) != 1 || got.PinnedRepos[0] != "owner/repo" {
		t.Errorf("pinned_repos = %v, want [\"owner/repo\"]", got.PinnedRepos)
	}
}

func TestProjectPatch_PaddedSlugsStoredTrimmed(t *testing.T) {
	s := newTestServer(t)
	id, _ := db.CreateProject(s.db, domain.Project{Name: "P"})
	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"pinned_repos": []string{" \tonly/one  "},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got, _ := db.GetProject(s.db, id)
	if len(got.PinnedRepos) != 1 || got.PinnedRepos[0] != "only/one" {
		t.Errorf("pinned_repos = %v, want [\"only/one\"]", got.PinnedRepos)
	}
}
