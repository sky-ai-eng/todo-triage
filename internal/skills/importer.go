package skills

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ImportResult summarizes what happened during an import run.
type ImportResult struct {
	Scanned  int      `json:"scanned"`
	Imported int      `json:"imported"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors,omitempty"`
}

// ImportAll discovers and imports Claude Code skill files from both
// personal (~/.claude/skills/) and project-scoped (./.claude/skills/) locations.
func ImportAll(database *sql.DB) ImportResult {
	var result ImportResult

	home, err := os.UserHomeDir()
	if err != nil {
		result.Errors = append(result.Errors, "could not determine home dir: "+err.Error())
		return result
	}

	// Search paths: personal + project-scoped (relative to current working dir)
	searchDirs := []string{
		filepath.Join(home, ".claude", "skills"),
		filepath.Join(".claude", "skills"),
	}

	seenDirs := make(map[string]struct{})
	seenFiles := make(map[string]struct{})

	for _, dir := range searchDirs {
		normalizedDir, err := normalizePath(dir)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("normalize dir %s: %v", dir, err))
			continue
		}
		if _, ok := seenDirs[normalizedDir]; ok {
			// If the project-scoped dir resolves to the same location as the
			// personal dir (e.g. cwd is $HOME), only scan it once.
			continue
		}
		seenDirs[normalizedDir] = struct{}{}

		pattern := filepath.Join(dir, "*", "SKILL.md")
		matches, err := filepath.Glob(pattern)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("glob %s: %v", pattern, err))
			continue
		}

		for _, path := range matches {
			normalizedPath, err := normalizePath(path)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("normalize file %s: %v", path, err))
				continue
			}
			if _, ok := seenFiles[normalizedPath]; ok {
				result.Skipped++
				continue
			}
			seenFiles[normalizedPath] = struct{}{}

			result.Scanned++
			if err := importSkillFile(database, path, normalizedPath); err != nil {
				if err == errSkillUnchanged || err == errSkillDuplicate {
					result.Skipped++
				} else {
					result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", path, err))
				}
			} else {
				result.Imported++
			}
		}
	}

	hiddenDuplicates, err := hideDuplicateImportedPrompts(database)
	if err != nil {
		result.Errors = append(result.Errors, "deduplicate imported prompts: "+err.Error())
	} else if hiddenDuplicates > 0 {
		log.Printf("[skills] deduplicated %d already-imported skills (same name/body)", hiddenDuplicates)
	}

	if result.Imported > 0 {
		log.Printf("[skills] imported %d skills (%d scanned, %d skipped)", result.Imported, result.Scanned, result.Skipped)
	}

	return result
}

var (
	errSkillUnchanged = fmt.Errorf("skill unchanged")
	errSkillDuplicate = fmt.Errorf("duplicate imported skill")
)

func importSkillFile(database *sql.DB, path string, canonicalPath string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	content := string(data)
	name, description, body := parseSkillFile(content, path)

	// Deterministic ID from the canonical file path so re-imports are idempotent
	// even when the same file is discovered as relative/absolute or via symlink.
	id := promptIDForPath(canonicalPath)

	// Check if already exists
	existing, err := db.GetPrompt(database, id)
	if err != nil {
		return err
	}
	if existing != nil {
		// Update body/name if the file changed
		if existing.Body == body && existing.Name == name {
			return errSkillUnchanged
		}
		if err := db.UpdatePrompt(database, id, name, body); err != nil {
			return err
		}
		log.Printf("[skills] updated %q from %s", name, path)
		return nil
	}

	// Skip creating a duplicate imported prompt if the same skill body/name is
	// already present from another path.
	duplicateID, err := findVisibleImportedPromptByContent(database, name, body)
	if err != nil {
		return err
	}
	if duplicateID != "" {
		log.Printf("[skills] skipped duplicate %q from %s (already imported as %s)", name, path, duplicateID)
		return errSkillDuplicate
	}

	prompt := domain.Prompt{
		ID:     id,
		Name:   name,
		Body:   body,
		Source: "imported",
	}

	if err := db.CreatePrompt(database, prompt); err != nil {
		return err
	}

	log.Printf("[skills] imported %q from %s (description: %s)", name, path, description)
	return nil
}

// parseSkillFile extracts the name, description, and body from a SKILL.md file.
// Handles YAML frontmatter between --- markers.
func parseSkillFile(content, path string) (name, description, body string) {
	// Default name from directory
	dir := filepath.Base(filepath.Dir(path))
	name = dir

	// Split frontmatter
	frontmatter, markdown := splitFrontmatter(content)

	// Parse frontmatter fields
	if frontmatter != "" {
		for _, line := range strings.Split(frontmatter, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "name:") {
				val := strings.TrimSpace(strings.TrimPrefix(line, "name:"))
				if val != "" {
					name = val
				}
			}
			if strings.HasPrefix(line, "description:") {
				description = strings.TrimSpace(strings.TrimPrefix(line, "description:"))
			}
		}
	}

	// The body is the markdown content (the actual prompt/instructions)
	body = strings.TrimSpace(markdown)
	if body == "" {
		body = content // fallback: use entire file
	}

	return name, description, body
}

// splitFrontmatter splits YAML frontmatter from markdown content.
// Returns ("", content) if no frontmatter found.
func splitFrontmatter(content string) (string, string) {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "---") {
		return "", content
	}

	// Find closing ---
	rest := content[3:]
	if idx := strings.Index(rest, "\n---"); idx >= 0 {
		frontmatter := strings.TrimSpace(rest[:idx])
		body := strings.TrimSpace(rest[idx+4:])
		return frontmatter, body
	}

	return "", content
}

func normalizePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	// Fall back to absolute clean path for non-existent paths, broken links,
	// or permission errors; we still want deterministic identity.
	return filepath.Clean(abs), nil
}

func promptIDForPath(path string) string {
	return fmt.Sprintf("imported-%x", sha256.Sum256([]byte(path)))[:20]
}

func promptFingerprint(name, body string) string {
	sum := sha256.Sum256([]byte(name + "\x00" + body))
	return fmt.Sprintf("%x", sum)
}

func findVisibleImportedPromptByContent(database *sql.DB, name, body string) (string, error) {
	var id string
	err := database.QueryRow(`
		SELECT id
		FROM prompts
		WHERE source = 'imported' AND hidden = 0 AND name = ? AND body = ?
		ORDER BY updated_at DESC
		LIMIT 1
	`, name, body).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

func hideDuplicateImportedPrompts(database *sql.DB) (int, error) {
	rows, err := database.Query(`
		SELECT p.id, p.name, p.body, p.hidden,
		       (
		           SELECT COUNT(*)
		           FROM prompt_triggers t
		           WHERE t.prompt_id = p.id
		       ) AS trigger_count
		FROM prompts p
		WHERE p.source = 'imported'
		ORDER BY p.hidden ASC, trigger_count DESC, p.updated_at DESC, p.created_at DESC, p.id ASC
	`)
	if err != nil {
		return 0, err
	}

	seen := make(map[string]struct{})
	var idsToHide []string

	for rows.Next() {
		var (
			id     string
			name   string
			body   string
			hidden bool
			refs   int
		)
		if err := rows.Scan(&id, &name, &body, &hidden, &refs); err != nil {
			_ = rows.Close()
			return 0, err
		}

		fingerprint := promptFingerprint(name, body)
		if _, ok := seen[fingerprint]; !ok {
			seen[fingerprint] = struct{}{}
			continue
		}
		if hidden {
			continue
		}
		// Keep prompts that are still bound to triggers visible so bindings
		// don't point at hidden prompts. Prompt cleanup stays conservative:
		// we only hide unreferenced duplicates.
		if refs > 0 {
			continue
		}
		idsToHide = append(idsToHide, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	// Close rows before UPDATEs so single-connection SQLite test DBs don't
	// deadlock while this scan is still holding the only open connection.
	if err := rows.Close(); err != nil {
		return 0, err
	}

	hiddenCount := 0
	for _, id := range idsToHide {
		if err := db.HidePrompt(database, id); err != nil {
			return hiddenCount, err
		}
		hiddenCount++
	}

	return hiddenCount, nil
}
