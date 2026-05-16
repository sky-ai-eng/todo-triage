package curator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func TestMaterializeSpecSkill_WritesProjectChoice(t *testing.T) {
	database := newTestDB(t)
	seedTestPrompt(t, database, domain.Prompt{
		ID: "custom-spec", Name: "Custom Spec",
		Body: "# custom guidance\nfollow this", Source: "user",
	})

	cwd := t.TempDir()
	project := &domain.Project{ID: "p1", SpecAuthorshipPromptID: "custom-spec"}
	if err := materializeSpecSkill(t.Context(), sqlitestore.New(database), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, project, cwd); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	skillPath := filepath.Join(cwd, ".claude", "skills", "ticket-spec", "SKILL.md")
	data, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read skill file: %v", err)
	}
	contents := string(data)
	if !strings.HasPrefix(contents, "---\nname: ticket-spec\n") {
		t.Errorf("missing frontmatter; got:\n%s", contents)
	}
	if !strings.Contains(contents, "description: ") {
		t.Error("missing description in frontmatter")
	}
	if !strings.Contains(contents, "follow this") {
		t.Error("project's chosen prompt body not present in SKILL.md")
	}
	if !strings.Contains(contents, "Custom Spec") {
		t.Error("source prompt name comment missing")
	}
}

func TestMaterializeSpecSkill_FallsBackToSystemDefault(t *testing.T) {
	database := newTestDB(t)
	seedTestPrompt(t, database, domain.Prompt{
		ID:     domain.SystemTicketSpecPromptID,
		Name:   "System Default",
		Body:   "default ticket guidance",
		Source: "system",
	})

	cwd := t.TempDir()
	// Empty SpecAuthorshipPromptID → fall through to system default.
	project := &domain.Project{ID: "p1"}
	if err := materializeSpecSkill(t.Context(), sqlitestore.New(database), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, project, cwd); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cwd, ".claude", "skills", "ticket-spec", "SKILL.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "default ticket guidance") {
		t.Error("fallback to system default did not write its body")
	}
}

func TestMaterializeSpecSkill_StaleReferenceFallsBack(t *testing.T) {
	// Project carries a prompt ID that no longer exists (e.g. a stale
	// in-memory copy from before the prompt was deleted, or a
	// replication lag in some future world). Materialization should
	// fall back to the seeded default rather than failing.
	database := newTestDB(t)
	seedTestPrompt(t, database, domain.Prompt{
		ID: domain.SystemTicketSpecPromptID, Name: "Default", Body: "fallback body", Source: "system",
	})

	cwd := t.TempDir()
	project := &domain.Project{ID: "p1", SpecAuthorshipPromptID: "ghost-id-that-does-not-exist"}
	if err := materializeSpecSkill(t.Context(), sqlitestore.New(database), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, project, cwd); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cwd, ".claude", "skills", "ticket-spec", "SKILL.md"))
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	if !strings.Contains(string(data), "fallback body") {
		t.Error("expected fallback to system default body when configured id is stale")
	}
}

func TestMaterializeSpecSkill_OverwritesOnEachCall(t *testing.T) {
	// Pin the no-reset-needed contract: each dispatch rewrites the
	// SKILL.md so the user can change the prompt body or swap which
	// prompt the project points at, and the next dispatch picks it up.
	database := newTestDB(t)
	seedTestPrompt(t, database, domain.Prompt{
		ID: "v1", Name: "v1", Body: "first version", Source: "user",
	})
	seedTestPrompt(t, database, domain.Prompt{
		ID: "v2", Name: "v2", Body: "second version", Source: "user",
	})

	cwd := t.TempDir()
	skillPath := filepath.Join(cwd, ".claude", "skills", "ticket-spec", "SKILL.md")
	project := &domain.Project{ID: "p1", SpecAuthorshipPromptID: "v1"}
	if err := materializeSpecSkill(t.Context(), sqlitestore.New(database), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, project, cwd); err != nil {
		t.Fatalf("first: %v", err)
	}
	first, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read SKILL.md after first dispatch: %v", err)
	}
	if !strings.Contains(string(first), "first version") {
		t.Fatal("first dispatch did not write v1 body")
	}

	// Swap project's prompt; next dispatch should overwrite.
	project.SpecAuthorshipPromptID = "v2"
	if err := materializeSpecSkill(t.Context(), sqlitestore.New(database), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, project, cwd); err != nil {
		t.Fatalf("second: %v", err)
	}
	second, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read SKILL.md after second dispatch: %v", err)
	}
	if strings.Contains(string(second), "first version") {
		t.Error("v1 body still present after swap to v2")
	}
	if !strings.Contains(string(second), "second version") {
		t.Error("v2 body not written after swap")
	}
}

func TestMaterializeSpecSkill_NoPromptDoesNotError(t *testing.T) {
	// Neither the project's chosen prompt nor the system default exist.
	// Materialization should log + skip rather than failing the dispatch.
	database := newTestDB(t)
	cwd := t.TempDir()
	project := &domain.Project{ID: "p1"}
	if err := materializeSpecSkill(t.Context(), sqlitestore.New(database), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, project, cwd); err != nil {
		t.Fatalf("expected nil error when no prompt available, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".claude", "skills", "ticket-spec", "SKILL.md")); !os.IsNotExist(err) {
		t.Error("expected no SKILL.md written when no prompt resolved")
	}
}

// TestMaterializeSpecSkill_NoPromptClearsStaleFile pins the new
// regression: a previous dispatch wrote SKILL.md, then the user
// emptied the configured prompt's body (or deleted both the project
// override and the system default). The next dispatch must remove the
// stale file so the agent doesn't keep applying outdated guidance.
func TestMaterializeSpecSkill_NoPromptClearsStaleFile(t *testing.T) {
	database := newTestDB(t)
	seedTestPrompt(t, database, domain.Prompt{
		ID: "v1", Name: "v1", Body: "active body", Source: "user",
	})

	cwd := t.TempDir()
	project := &domain.Project{ID: "p1", SpecAuthorshipPromptID: "v1"}
	if err := materializeSpecSkill(t.Context(), sqlitestore.New(database), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, project, cwd); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	skillPath := filepath.Join(cwd, ".claude", "skills", "ticket-spec", "SKILL.md")
	if _, err := os.Stat(skillPath); err != nil {
		t.Fatalf("expected SKILL.md after first dispatch: %v", err)
	}

	// User repoints the project at a non-existent prompt and there's
	// no system default seeded. Resolution should fail through to the
	// no-prompt branch.
	project.SpecAuthorshipPromptID = "ghost"
	if err := materializeSpecSkill(t.Context(), sqlitestore.New(database), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, project, cwd); err != nil {
		t.Fatalf("second dispatch: %v", err)
	}
	if _, err := os.Stat(skillPath); !os.IsNotExist(err) {
		t.Errorf("expected stale SKILL.md to be removed when no prompt resolves, stat err=%v", err)
	}
}

func TestMaterializeJiraFormattingSkill_WritesBuiltInSkill(t *testing.T) {
	cwd := t.TempDir()
	if err := materializeJiraFormattingSkill(cwd); err != nil {
		t.Fatalf("materialize jira formatting skill: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cwd, ".claude", "skills", jiraFormattingSkillDirName, "SKILL.md"))
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	contents := string(data)
	if !strings.Contains(contents, "name: jira-formatting") {
		t.Error("expected jira-formatting frontmatter name")
	}
	if !strings.Contains(contents, "h2. Heading") {
		t.Error("expected heading guidance")
	}
	if !strings.Contains(contents, "{{variable_name}}") {
		t.Error("expected inline code guidance")
	}
	if !strings.Contains(contents, "{code}") {
		t.Error("expected code block guidance")
	}
}
