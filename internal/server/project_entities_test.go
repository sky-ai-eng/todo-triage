package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestProjectEntities_FiltersByProjectAndState — only entities
// assigned to this project AND active should appear. Closed entities
// in the project, and active entities in other projects, must be
// excluded.
func TestProjectEntities_FiltersByProjectAndState(t *testing.T) {
	s := newTestServer(t)
	seedConfiguredRepo(t, s, "owner", "repo")
	pid, err := db.CreateProject(s.db, domain.Project{Name: "P", PinnedRepos: []string{"owner/repo"}})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	other, err := db.CreateProject(s.db, domain.Project{Name: "Other", PinnedRepos: []string{"owner/repo"}})
	if err != nil {
		t.Fatal(err)
	}

	mine := mustEntity(t, s.db, "github", "owner/repo#1", "pr", "mine")
	if err := db.AssignEntityProject(s.db, mine.ID, &pid, "rationale-1"); err != nil {
		t.Fatal(err)
	}
	closedMine := mustEntity(t, s.db, "github", "owner/repo#2", "pr", "closed-mine")
	if err := db.AssignEntityProject(s.db, closedMine.ID, &pid, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkEntityClosed(s.db, closedMine.ID); err != nil {
		t.Fatal(err)
	}
	otherProj := mustEntity(t, s.db, "github", "owner/repo#3", "pr", "other-project")
	if err := db.AssignEntityProject(s.db, otherProj.ID, &other, ""); err != nil {
		t.Fatal(err)
	}
	unassigned := mustEntity(t, s.db, "github", "owner/repo#4", "pr", "unassigned")

	rec := doJSON(t, s, http.MethodGet, "/api/projects/"+pid+"/entities", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Entities []projectEntity `json:"entities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	gotIDs := map[string]projectEntity{}
	for _, e := range resp.Entities {
		gotIDs[e.ID] = e
	}
	if _, ok := gotIDs[mine.ID]; !ok {
		t.Errorf("expected `mine` in response")
	}
	if _, ok := gotIDs[closedMine.ID]; ok {
		t.Errorf("closed entity should be filtered out")
	}
	if _, ok := gotIDs[otherProj.ID]; ok {
		t.Errorf("other-project entity should be filtered out")
	}
	if _, ok := gotIDs[unassigned.ID]; ok {
		t.Errorf("unassigned entity should be filtered out")
	}
	if got := gotIDs[mine.ID].ClassificationRationale; got != "rationale-1" {
		t.Errorf("classification_rationale = %q, want rationale-1", got)
	}
}

// TestProjectEntities_NotFoundProject — unknown project id returns 404
// rather than a 200 with empty list, so the frontend can distinguish
// "no entities here" from "this project is gone."
func TestProjectEntities_NotFoundProject(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodGet, "/api/projects/missing-id/entities", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
