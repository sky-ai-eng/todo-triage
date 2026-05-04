package curator

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// skillDirName is the per-session subdirectory Claude Code scans for
// project-scoped skills. Claude Code globs `<cwd>/.claude/skills/*/SKILL.md`
// at session start and (best-effort) on resume, so writing into this
// path before each dispatch is what makes the per-project ticket-spec
// guidance available to the model as a discoverable skill.
const skillDirName = "ticket-spec"

// materializeSpecSkill writes <cwd>/.claude/skills/<skillDirName>/SKILL.md
// containing the body of the project's effective spec-authorship prompt.
// Resolution order: project's `spec_authorship_prompt_id`, then the
// seeded `domain.SystemTicketSpecPromptID`. Either path falling through
// to a missing/empty prompt logs a warning and removes any prior
// SKILL.md rather than failing the dispatch — the Curator should still
// answer the user's message even without the skill, and a stale file
// from a previous resolution would otherwise keep feeding the agent
// out-of-date guidance.
//
// We always overwrite. The prompt body can change between turns (the
// user edits it on the Prompts page or swaps which prompt the project
// points at) and the user expects the Curator's next turn to pick up
// the change without a session reset. Writing fresh on every dispatch
// is the cheapest way to honor that.
func materializeSpecSkill(database *sql.DB, project *domain.Project, cwd string) error {
	if project == nil {
		return nil
	}
	prompt, err := resolveSpecPrompt(database, project)
	if err != nil {
		return err
	}
	dir := filepath.Join(cwd, ".claude", "skills", skillDirName)
	path := filepath.Join(dir, "SKILL.md")

	if prompt == nil || strings.TrimSpace(prompt.Body) == "" {
		// No usable prompt — clear any stale SKILL.md from a previous
		// dispatch so the agent doesn't keep applying guidance that no
		// longer matches the project's current configuration.
		log.Printf("[curator] no spec-authorship prompt resolved for project %s; clearing stale skill if any", project.ID)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale SKILL.md: %w", err)
		}
		return nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create skill dir: %w", err)
	}
	contents := renderSkillFile(prompt.Name, prompt.Body)
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		return fmt.Errorf("write SKILL.md: %w", err)
	}
	return nil
}

// resolveSpecPrompt returns the project's chosen prompt or the seeded
// default. A NULL/empty pointer on the project, or a pointer to a
// deleted prompt, both fall through to the default — the schema's
// ON DELETE SET NULL drops dangling FKs but a stale in-memory project
// row could still carry the deleted id, so we re-check.
func resolveSpecPrompt(database *sql.DB, project *domain.Project) (*domain.Prompt, error) {
	if project.SpecAuthorshipPromptID != "" {
		p, err := db.GetPrompt(database, project.SpecAuthorshipPromptID)
		if err != nil {
			return nil, fmt.Errorf("load configured spec prompt: %w", err)
		}
		if p != nil {
			return p, nil
		}
		log.Printf("[curator] project %s references missing spec prompt %s; falling back to system default",
			project.ID, project.SpecAuthorshipPromptID)
	}
	p, err := db.GetPrompt(database, domain.SystemTicketSpecPromptID)
	if err != nil {
		return nil, fmt.Errorf("load default spec prompt: %w", err)
	}
	return p, nil
}

// renderSkillFile wraps the prompt body in the YAML frontmatter shape
// Claude Code expects (`name:` + `description:`). The description is a
// short trigger sentence — it's what the model reads to decide whether
// the skill is relevant to the current turn, not the full guidance.
//
// We keep the description constant rather than deriving it from the
// prompt body so swapping prompts doesn't break the model's discovery
// heuristic — the user can edit *what* a well-specced ticket means
// without also having to write a Claude-Code-flavored skill descriptor.
func renderSkillFile(promptName, body string) string {
	const description = "Apply when the user asks you to draft, file, or write up a ticket for this project. Defines the format and standards for well-specced tickets."
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: ")
	b.WriteString(skillDirName)
	b.WriteString("\n")
	b.WriteString("description: ")
	b.WriteString(description)
	b.WriteString("\n---\n\n")
	if strings.TrimSpace(promptName) != "" {
		b.WriteString("<!-- Sourced from prompt: ")
		b.WriteString(strings.TrimSpace(promptName))
		b.WriteString(" -->\n\n")
	}
	b.WriteString(strings.TrimSpace(body))
	b.WriteString("\n")
	return b.String()
}
