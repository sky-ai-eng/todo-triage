package db

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func TestCreateGetProject_Roundtrip(t *testing.T) {
	database := newTestDB(t)
	id, err := CreateProject(database, domain.Project{
		Name:             "Triage Factory",
		Description:      "Local-first triage UI",
		PinnedRepos:      []string{"sky-ai-eng/triage-factory", "sky-ai-eng/sky"},
		CuratorSessionID: "sess-123",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == "" {
		t.Fatal("expected generated id, got empty")
	}

	got, err := GetProject(database, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("expected project, got nil")
	}
	if got.Name != "Triage Factory" {
		t.Errorf("name = %q", got.Name)
	}
	if got.Description != "Local-first triage UI" {
		t.Errorf("description = %q", got.Description)
	}
	if got.CuratorSessionID != "sess-123" {
		t.Errorf("session id = %q", got.CuratorSessionID)
	}
	if len(got.PinnedRepos) != 2 || got.PinnedRepos[0] != "sky-ai-eng/triage-factory" {
		t.Errorf("pinned_repos = %v", got.PinnedRepos)
	}
	if got.SummaryStale {
		t.Errorf("summary_stale should default false, got true")
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps not set: created=%v updated=%v", got.CreatedAt, got.UpdatedAt)
	}
}

func TestCreateProject_EmptyPinnedRepos_RoundtripsAsArray(t *testing.T) {
	// Regression guard: nil pinned_repos slice should serialize as
	// `[]` and deserialize back to a non-nil empty slice. A null
	// would surprise frontend code that expects to .map() the field.
	database := newTestDB(t)
	id, err := CreateProject(database, domain.Project{Name: "Empty"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := GetProject(database, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PinnedRepos == nil {
		t.Fatal("PinnedRepos roundtripped as nil; should be []")
	}
	if len(got.PinnedRepos) != 0 {
		t.Errorf("expected empty slice, got %v", got.PinnedRepos)
	}
}

// TestCreateProject_HonorsCallerSuppliedID pins the documented
// behavior: an explicit ID on the input is preserved (useful for
// tests / seed scripts), while an empty ID triggers server-side
// uuid generation. The HTTP handler always passes empty, so
// API clients can't supply an arbitrary ID.
func TestCreateProject_HonorsCallerSuppliedID(t *testing.T) {
	database := newTestDB(t)
	id, err := CreateProject(database, domain.Project{ID: "fixed-id-for-test", Name: "P"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id != "fixed-id-for-test" {
		t.Errorf("returned id = %q, want %q", id, "fixed-id-for-test")
	}
	got, _ := GetProject(database, "fixed-id-for-test")
	if got == nil {
		t.Fatal("project not found at caller-supplied id")
	}
}

func TestGetProject_MissingReturnsNil(t *testing.T) {
	database := newTestDB(t)
	got, err := GetProject(database, "no-such-id")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing id, got %+v", got)
	}
}

func TestListProjects_OrderedByNameCaseInsensitive(t *testing.T) {
	database := newTestDB(t)
	for _, name := range []string{"zeta", "Alpha", "beta", "Charlie"} {
		if _, err := CreateProject(database, domain.Project{Name: name}); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}
	got, err := ListProjects(database)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"Alpha", "beta", "Charlie", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("length = %d, want %d", len(got), len(want))
	}
	for i, p := range got {
		if p.Name != want[i] {
			t.Errorf("[%d] = %q, want %q", i, p.Name, want[i])
		}
	}
}

func TestListProjects_EmptyReturnsEmptySlice(t *testing.T) {
	// Empty result must be []domain.Project{}, never nil — the JSON
	// handler relies on this so the API always returns `[]` instead
	// of `null` for an empty project list.
	database := newTestDB(t)
	got, err := ListProjects(database)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got == nil {
		t.Fatal("ListProjects returned nil; should be []")
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestUpdateProject_PreservesCreatedAtBumpsUpdatedAt(t *testing.T) {
	database := newTestDB(t)
	id, err := CreateProject(database, domain.Project{Name: "Project"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	original, _ := GetProject(database, id)

	mutated := *original
	mutated.Description = "now described"
	mutated.PinnedRepos = []string{"a/b"}
	mutated.SummaryStale = true
	if err := UpdateProject(database, mutated); err != nil {
		t.Fatalf("update: %v", err)
	}

	updated, _ := GetProject(database, id)
	if !updated.CreatedAt.Equal(original.CreatedAt) {
		t.Errorf("created_at changed: was %v, now %v", original.CreatedAt, updated.CreatedAt)
	}
	if !updated.UpdatedAt.After(original.UpdatedAt) && !updated.UpdatedAt.Equal(original.UpdatedAt) {
		t.Errorf("updated_at not bumped: was %v, now %v", original.UpdatedAt, updated.UpdatedAt)
	}
	if updated.Description != "now described" {
		t.Errorf("description = %q", updated.Description)
	}
	if !updated.SummaryStale {
		t.Errorf("summary_stale = false; expected true")
	}
}

func TestUpdateProject_MissingReturnsNoRows(t *testing.T) {
	database := newTestDB(t)
	err := UpdateProject(database, domain.Project{ID: "no-such-id", Name: "x"})
	if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows, got %v", err)
	}
}

// TestCreateGetProject_TrackerColumnsRoundtrip pins the SELECT/Scan
// alignment for the SKY-217 tracker columns. The two new columns
// (jira_project_key, linear_project_key) live between pinned_repos
// and created_at in the Scan order; if a future change shifts that
// order without updating both sides, this test catches it instead
// of letting the bug surface as a runtime parse failure on the
// timestamps that follow.
func TestCreateGetProject_TrackerColumnsRoundtrip(t *testing.T) {
	database := newTestDB(t)
	id, err := CreateProject(database, domain.Project{
		Name:             "Tracked",
		JiraProjectKey:   "SKY",
		LinearProjectKey: "tf-2026",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := GetProject(database, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("project not found")
	}
	if got.JiraProjectKey != "SKY" {
		t.Errorf("jira_project_key roundtrip: got %q, want SKY", got.JiraProjectKey)
	}
	if got.LinearProjectKey != "tf-2026" {
		t.Errorf("linear_project_key roundtrip: got %q, want tf-2026", got.LinearProjectKey)
	}
	// Surrounding fields must still parse — a misaligned Scan would
	// stuff the tracker key into one of these instead.
	if got.Name != "Tracked" {
		t.Errorf("name = %q (Scan misalignment?)", got.Name)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at zero — Scan likely landed a string in a time field")
	}
}

// TestUpdateProject_TrackerColumnsRoundtrip exercises the same alignment
// on the UPDATE path. A PATCH that changes only tracker fields must
// land in the right columns, and the read-back has to reflect them.
func TestUpdateProject_TrackerColumnsRoundtrip(t *testing.T) {
	database := newTestDB(t)
	id, err := CreateProject(database, domain.Project{Name: "drift"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _ := GetProject(database, id)
	got.JiraProjectKey = "OPS"
	got.LinearProjectKey = "ops-board"
	if err := UpdateProject(database, *got); err != nil {
		t.Fatalf("update: %v", err)
	}
	after, _ := GetProject(database, id)
	if after.JiraProjectKey != "OPS" {
		t.Errorf("jira_project_key = %q after update, want OPS", after.JiraProjectKey)
	}
	if after.LinearProjectKey != "ops-board" {
		t.Errorf("linear_project_key = %q after update, want ops-board", after.LinearProjectKey)
	}
	// Clear-the-tracker path: empty strings must persist as empty,
	// not as NULL-rendered placeholder text.
	after.JiraProjectKey = ""
	after.LinearProjectKey = ""
	if err := UpdateProject(database, *after); err != nil {
		t.Fatalf("clear update: %v", err)
	}
	cleared, _ := GetProject(database, id)
	if cleared.JiraProjectKey != "" || cleared.LinearProjectKey != "" {
		t.Errorf("clear roundtrip: jira=%q linear=%q (want both empty)",
			cleared.JiraProjectKey, cleared.LinearProjectKey)
	}
}

// TestListProjects_TrackerColumnsRoundtrip exercises the LIST path
// since it has its own SELECT statement (separate from GetProject).
// A column-order mismatch between the two SELECTs would only surface
// here, not in the get-roundtrip test above.
func TestListProjects_TrackerColumnsRoundtrip(t *testing.T) {
	database := newTestDB(t)
	if _, err := CreateProject(database, domain.Project{
		Name:             "list-me",
		JiraProjectKey:   "LIST",
		LinearProjectKey: "list-linear",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	projects, err := ListProjects(database)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("got %d projects", len(projects))
	}
	if projects[0].JiraProjectKey != "LIST" || projects[0].LinearProjectKey != "list-linear" {
		t.Errorf("list roundtrip: jira=%q linear=%q",
			projects[0].JiraProjectKey, projects[0].LinearProjectKey)
	}
}

// TestProject_SpecAuthorshipPromptID_Roundtrip pins the SKY-221
// column on the create/get and update/get paths. The column is
// nullable in storage; an empty Go-string roundtrips as empty.
// FK to prompts.id is ON DELETE SET NULL — exercised by deleting the
// referenced prompt and confirming the project's pointer clears.
func TestProject_SpecAuthorshipPromptID_Roundtrip(t *testing.T) {
	database := newTestDB(t)

	if err := SeedPrompt(database, domain.Prompt{
		ID: "test-spec-prompt", Name: "Test Spec", Body: "x", Source: "test",
	}); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}

	id, err := CreateProject(database, domain.Project{
		Name:                   "p",
		SpecAuthorshipPromptID: "test-spec-prompt",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _ := GetProject(database, id)
	if got.SpecAuthorshipPromptID != "test-spec-prompt" {
		t.Errorf("after create: got %q, want test-spec-prompt", got.SpecAuthorshipPromptID)
	}

	// Update path.
	got.SpecAuthorshipPromptID = ""
	if err := UpdateProject(database, *got); err != nil {
		t.Fatalf("update: %v", err)
	}
	cleared, _ := GetProject(database, id)
	if cleared.SpecAuthorshipPromptID != "" {
		t.Errorf("after clear: got %q, want empty", cleared.SpecAuthorshipPromptID)
	}

	// FK ON DELETE SET NULL: re-point, delete the prompt, confirm null.
	cleared.SpecAuthorshipPromptID = "test-spec-prompt"
	if err := UpdateProject(database, *cleared); err != nil {
		t.Fatalf("re-set: %v", err)
	}
	if err := DeletePrompt(database, "test-spec-prompt"); err != nil {
		t.Fatalf("delete prompt: %v", err)
	}
	after, _ := GetProject(database, id)
	if after.SpecAuthorshipPromptID != "" {
		t.Errorf("after FK cascade: got %q, want empty (ON DELETE SET NULL)", after.SpecAuthorshipPromptID)
	}
}

func TestDeleteProject_Roundtrip(t *testing.T) {
	database := newTestDB(t)
	id, _ := CreateProject(database, domain.Project{Name: "doomed"})

	if err := DeleteProject(database, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, _ := GetProject(database, id)
	if got != nil {
		t.Errorf("project still readable after delete: %+v", got)
	}

	// Second delete returns ErrNoRows so the handler can map to 404.
	err := DeleteProject(database, id)
	if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows on second delete, got %v", err)
	}
}

// TestDeleteProject_NullsOutEntityProjectID is the load-bearing FK
// behavior: deleting a project must not delete the entities it
// covered, just untag them. Pinned to the SQL-level FK declaration
// — no application code clears project_id manually.
func TestDeleteProject_NullsOutEntityProjectID(t *testing.T) {
	database := newTestDB(t)
	projectID, _ := CreateProject(database, domain.Project{Name: "P1"})

	entity, _, err := FindOrCreateEntity(database, "github", "owner/repo#1", "pr", "T", "https://example.com/1")
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	if _, err := database.Exec(`UPDATE entities SET project_id = ? WHERE id = ?`, projectID, entity.ID); err != nil {
		t.Fatalf("tag entity: %v", err)
	}

	if err := DeleteProject(database, projectID); err != nil {
		t.Fatalf("delete project: %v", err)
	}

	var projectIDOut sql.NullString
	if err := database.QueryRow(`SELECT project_id FROM entities WHERE id = ?`, entity.ID).Scan(&projectIDOut); err != nil {
		t.Fatalf("read entity back: %v", err)
	}
	if projectIDOut.Valid {
		t.Errorf("entity.project_id should be NULL after project delete, got %q", projectIDOut.String)
	}

	// Entity row itself must survive the cascade — losing the
	// entity would lose every event/task/run hanging off it.
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM entities WHERE id = ?`, entity.ID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("entity row missing after project delete; got count=%d", count)
	}
}

// TestProjectsTable_PinnedReposIsJSONArray pins the storage shape:
// pinned_repos is stored as a JSON array string, never null. The
// frontend reads this directly via /api/projects so a shape regression
// would break the projects view.
func TestProjectsTable_PinnedReposIsJSONArray(t *testing.T) {
	database := newTestDB(t)
	id, _ := CreateProject(database, domain.Project{Name: "p", PinnedRepos: []string{"a/b", "c/d"}})
	var raw string
	if err := database.QueryRow(`SELECT pinned_repos FROM projects WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !strings.HasPrefix(raw, "[") || !strings.HasSuffix(raw, "]") {
		t.Errorf("pinned_repos column = %q; expected JSON array", raw)
	}
	if !strings.Contains(raw, `"a/b"`) || !strings.Contains(raw, `"c/d"`) {
		t.Errorf("pinned_repos column missing entries: %q", raw)
	}
}
