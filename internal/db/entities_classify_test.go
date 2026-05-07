package db

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func TestAssignEntityProject_RoundTrips(t *testing.T) {
	database := newTestDB(t)

	entity, _, err := FindOrCreateEntity(database, "github", "owner/repo#1", "pr", "T", "https://x/1")
	if err != nil {
		t.Fatalf("entity: %v", err)
	}

	// Pre-condition: GetEntity returns nil ProjectID and unclassified.
	got, err := GetEntity(database, entity.ID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.ProjectID != nil {
		t.Errorf("project_id should be nil before assign, got %q", *got.ProjectID)
	}

	pid, err := CreateProject(database, domain.Project{ID: "p1", Name: "P1"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	if err := AssignEntityProject(database, entity.ID, &pid, "winning rationale"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	got, err = GetEntity(database, entity.ID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.ProjectID == nil || *got.ProjectID != pid {
		gotStr := "<nil>"
		if got.ProjectID != nil {
			gotStr = *got.ProjectID
		}
		t.Errorf("project_id = %s, want %s", gotStr, pid)
	}
	if got.ClassificationRationale != "winning rationale" {
		t.Errorf("rationale = %q, want %q", got.ClassificationRationale, "winning rationale")
	}
}

func TestAssignEntityProject_NilStampsClassified(t *testing.T) {
	// SKY-220: an entity that scored below threshold gets project_id=NULL
	// AND classified_at set, so re-polls don't re-fire classification.
	database := newTestDB(t)

	entity, _, err := FindOrCreateEntity(database, "github", "owner/repo#2", "pr", "T", "https://x/2")
	if err != nil {
		t.Fatalf("entity: %v", err)
	}

	// Confirm the row is in the unclassified queue first.
	pre, err := ListUnclassifiedEntities(database)
	if err != nil {
		t.Fatalf("ListUnclassifiedEntities: %v", err)
	}
	if !containsEntity(pre, entity.ID) {
		t.Fatalf("entity should be unclassified before AssignEntityProject")
	}

	// Below-threshold path: nil project_id, but the helper should still
	// stamp classified_at so the runner doesn't re-fire next cycle.
	// Rationale comes from the runner-up project so the UI can surface
	// "closest match was X at N/100."
	if err := AssignEntityProject(database, entity.ID, nil, "closest match: Auth at 45/100"); err != nil {
		t.Fatalf("assign nil: %v", err)
	}

	post, err := ListUnclassifiedEntities(database)
	if err != nil {
		t.Fatalf("ListUnclassifiedEntities post: %v", err)
	}
	if containsEntity(post, entity.ID) {
		t.Errorf("entity still in unclassified queue after AssignEntityProject(nil) — classified_at not stamped")
	}

	// Rationale is preserved on the row even though project_id is NULL.
	got, err := GetEntity(database, entity.ID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.ClassificationRationale != "closest match: Auth at 45/100" {
		t.Errorf("rationale = %q, want %q", got.ClassificationRationale, "closest match: Auth at 45/100")
	}
}

func TestListUnclassifiedEntities_ExcludesAssignedAndClosed(t *testing.T) {
	database := newTestDB(t)

	unassigned, _, err := FindOrCreateEntity(database, "github", "owner/repo#10", "pr", "T", "https://x/10")
	if err != nil {
		t.Fatal(err)
	}
	assigned, _, err := FindOrCreateEntity(database, "github", "owner/repo#11", "pr", "T", "https://x/11")
	if err != nil {
		t.Fatal(err)
	}
	closed, _, err := FindOrCreateEntity(database, "github", "owner/repo#12", "pr", "T", "https://x/12")
	if err != nil {
		t.Fatal(err)
	}

	pid, _ := CreateProject(database, domain.Project{ID: "px", Name: "X"})
	if err := AssignEntityProject(database, assigned.ID, &pid, ""); err != nil {
		t.Fatal(err)
	}
	if err := MarkEntityClosed(database, closed.ID); err != nil {
		t.Fatal(err)
	}

	out, err := ListUnclassifiedEntities(database)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !containsEntity(out, unassigned.ID) {
		t.Errorf("unassigned entity missing from list")
	}
	if containsEntity(out, assigned.ID) {
		t.Errorf("assigned entity should be excluded")
	}
	if containsEntity(out, closed.ID) {
		t.Errorf("closed entity should be excluded")
	}
}

func containsEntity(entities []domain.Entity, id string) bool {
	for _, e := range entities {
		if e.ID == id {
			return true
		}
	}
	return false
}
