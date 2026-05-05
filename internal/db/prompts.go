package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SeedOrUpdateSystemPrompt inserts a shipped prompt if missing, or updates it
// when the shipped body changes and the local row has not been user-modified.
// Version state is recorded in system_prompt_versions so subsequent seed runs
// with unchanged shipped content are no-ops (no churn to prompts.updated_at,
// which the UI orders by).
func SeedOrUpdateSystemPrompt(db *sql.DB, p domain.Prompt) error {
	if p.Source == "" {
		p.Source = "system"
	}
	now := time.Now()
	// Include name and source in the hash so shipped renames/re-sourcing trigger
	// an update even when the body is unchanged. Null bytes prevent collisions
	// between different field combinations.
	h := sha256.Sum256([]byte(p.Name + "\x00" + p.Body + "\x00" + p.Source))
	hash := hex.EncodeToString(h[:])

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var (
		exists       bool
		userModified int
	)
	switch err := tx.QueryRow(`SELECT user_modified FROM prompts WHERE id = ?`, p.ID).Scan(&userModified); {
	case err == sql.ErrNoRows:
		exists = false
	case err != nil:
		return fmt.Errorf("read prompt: %w", err)
	default:
		exists = true
	}

	if !exists {
		if _, err := tx.Exec(`
			INSERT INTO prompts (id, name, body, source, usage_count, user_modified, created_at, updated_at)
			VALUES (?, ?, ?, ?, 0, 0, ?, ?)
		`, p.ID, p.Name, p.Body, p.Source, now, now); err != nil {
			return err
		}
		if err := upsertSystemPromptVersion(tx, p.ID, hash, now); err != nil {
			return err
		}
		return tx.Commit()
	}

	// Row exists. Never touch user-modified prompts — they're intentional
	// local edits and we shouldn't claim a new shipped hash was applied.
	if userModified != 0 {
		return tx.Commit()
	}

	// Compare against the previously applied shipped hash. If it matches,
	// the shipped content hasn't changed since the last seed run and we
	// skip both writes to avoid bumping updated_at / applied_at on every
	// startup. A missing version row (legacy prompt predating version
	// tracking) falls through to the update branch and gets overwritten
	// with the current shipped content.
	var priorHash sql.NullString
	if err := tx.QueryRow(
		`SELECT content_hash FROM system_prompt_versions WHERE prompt_id = ?`,
		p.ID,
	).Scan(&priorHash); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("read prior prompt version: %w", err)
	}
	if priorHash.Valid && priorHash.String == hash {
		return tx.Commit()
	}

	if _, err := tx.Exec(`
		UPDATE prompts
		SET name = ?, body = ?, source = ?, updated_at = ?
		WHERE id = ?
	`, p.Name, p.Body, p.Source, now, p.ID); err != nil {
		return err
	}
	if err := upsertSystemPromptVersion(tx, p.ID, hash, now); err != nil {
		return err
	}
	return tx.Commit()
}

func upsertSystemPromptVersion(tx *sql.Tx, promptID, hash string, now time.Time) error {
	if _, err := tx.Exec(`
		INSERT INTO system_prompt_versions (prompt_id, content_hash, applied_at)
		VALUES (?, ?, ?)
		ON CONFLICT(prompt_id) DO UPDATE SET
			content_hash = excluded.content_hash,
			applied_at = excluded.applied_at
		WHERE system_prompt_versions.content_hash != excluded.content_hash
	`, promptID, hash, now); err != nil {
		return fmt.Errorf("upsert system prompt version: %w", err)
	}
	return nil
}

// SeedPrompt inserts a prompt if it doesn't exist.
func SeedPrompt(db *sql.DB, p domain.Prompt) error {
	// Skip if already seeded
	var exists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM prompts WHERE id = ?`, p.ID).Scan(&exists); err != nil {
		return fmt.Errorf("check prompt existence: %w", err)
	}
	if exists > 0 {
		return nil
	}

	now := time.Now()
	_, err := db.Exec(`
		INSERT INTO prompts (id, name, body, source, usage_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, 0, ?, ?)
	`, p.ID, p.Name, p.Body, p.Source, now, now)
	return err
}

// ListPrompts returns all non-hidden prompts.
func ListPrompts(db *sql.DB) ([]domain.Prompt, error) {
	rows, err := db.Query(`
		SELECT id, name, body, source, usage_count, created_at, updated_at
		FROM prompts WHERE hidden = 0 ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prompts []domain.Prompt
	for rows.Next() {
		var p domain.Prompt
		if err := rows.Scan(&p.ID, &p.Name, &p.Body, &p.Source, &p.UsageCount, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		prompts = append(prompts, p)
	}
	return prompts, rows.Err()
}

// GetPrompt returns a single prompt by ID.
func GetPrompt(db *sql.DB, id string) (*domain.Prompt, error) {
	var p domain.Prompt
	err := db.QueryRow(`
		SELECT id, name, body, source, usage_count, created_at, updated_at
		FROM prompts WHERE id = ?
	`, id).Scan(&p.ID, &p.Name, &p.Body, &p.Source, &p.UsageCount, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// CreatePrompt inserts a new prompt.
func CreatePrompt(db *sql.DB, p domain.Prompt) error {
	now := time.Now()
	_, err := db.Exec(`
		INSERT INTO prompts (id, name, body, source, usage_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, 0, ?, ?)
	`, p.ID, p.Name, p.Body, p.Source, now, now)
	return err
}

// UpdatePrompt updates a prompt's name and body.
func UpdatePrompt(db *sql.DB, id, name, body string) error {
	_, err := db.Exec(`
		UPDATE prompts SET name = ?, body = ?, user_modified = 1, updated_at = ? WHERE id = ?
	`, name, body, time.Now(), id)
	return err
}

// DeletePrompt removes a prompt and its bindings (CASCADE).
func DeletePrompt(db *sql.DB, id string) error {
	_, err := db.Exec(`DELETE FROM prompts WHERE id = ?`, id)
	return err
}

// HidePrompt soft-deletes a prompt by setting hidden = 1.
func HidePrompt(db *sql.DB, id string) error {
	_, err := db.Exec(`UPDATE prompts SET hidden = 1 WHERE id = ?`, id)
	return err
}

// UnhidePrompt restores a hidden prompt.
func UnhidePrompt(db *sql.DB, id string) error {
	_, err := db.Exec(`UPDATE prompts SET hidden = 0 WHERE id = ?`, id)
	return err
}

// IncrementPromptUsage bumps the usage_count for a prompt.
func IncrementPromptUsage(db *sql.DB, promptID string) error {
	_, err := db.Exec(`UPDATE prompts SET usage_count = usage_count + 1 WHERE id = ?`, promptID)
	return err
}
