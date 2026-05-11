package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// createPromptForTest is the raw-SQL replacement for the deleted
// CreatePrompt free-function. Used by tests in package db that need
// to seed a prompt row but can't import internal/db/sqlite (cycle:
// the sqlite package depends on db). Tests outside this package use
// stores.Prompts.Create directly.
func createPromptForTest(t *testing.T, database *sql.DB, p domain.Prompt) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := database.Exec(`
		INSERT INTO prompts (id, name, body, source, allowed_tools, usage_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 0, ?, ?)
	`, p.ID, p.Name, p.Body, p.Source, p.AllowedTools, now, now); err != nil {
		t.Fatalf("createPromptForTest %s: %v", p.ID, err)
	}
}

// deletePromptForTest mirrors the deleted DeletePrompt free-function
// for in-package tests.
func deletePromptForTest(t *testing.T, database *sql.DB, id string) {
	t.Helper()
	if _, err := database.Exec(`DELETE FROM prompts WHERE id = ?`, id); err != nil {
		t.Fatalf("deletePromptForTest %s: %v", id, err)
	}
}

// getPromptForTest mirrors the deleted GetPrompt free-function for
// in-package tests. Returns nil when missing rather than ErrNoRows.
func getPromptForTest(t *testing.T, database *sql.DB, id string) *domain.Prompt {
	t.Helper()
	var p domain.Prompt
	err := database.QueryRow(`
		SELECT id, name, body, source, allowed_tools, usage_count, created_at, updated_at
		FROM prompts WHERE id = ?
	`, id).Scan(&p.ID, &p.Name, &p.Body, &p.Source, &p.AllowedTools, &p.UsageCount, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		t.Fatalf("getPromptForTest %s: %v", id, err)
	}
	return &p
}
