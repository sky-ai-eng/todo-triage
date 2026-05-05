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
// Version state is recorded in system_prompt_versions for idempotent reseeding.
func SeedOrUpdateSystemPrompt(db *sql.DB, p domain.Prompt) error {
	if p.Source == "" {
		p.Source = "system"
	}
	now := time.Now()
	h := sha256.Sum256([]byte(p.Body))
	hash := hex.EncodeToString(h[:])

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM prompts WHERE id = ?`, p.ID).Scan(&exists); err != nil {
		return fmt.Errorf("check prompt existence: %w", err)
	}
	if exists == 0 {
		if _, err := tx.Exec(`
			INSERT INTO prompts (id, name, body, source, usage_count, user_modified, created_at, updated_at)
			VALUES (?, ?, ?, ?, 0, 0, ?, ?)
		`, p.ID, p.Name, p.Body, p.Source, now, now); err != nil {
			return err
		}
	} else {
		var userModified int
		if err := tx.QueryRow(`SELECT user_modified FROM prompts WHERE id = ?`, p.ID).Scan(&userModified); err != nil {
			return fmt.Errorf("read user_modified: %w", err)
		}
		if userModified == 0 {
			if _, err := tx.Exec(`
				UPDATE prompts
				SET name = ?, body = ?, source = ?, updated_at = ?
				WHERE id = ?
			`, p.Name, p.Body, p.Source, now, p.ID); err != nil {
				return err
			}
		}
	}

	if _, err := tx.Exec(`
		INSERT INTO system_prompt_versions (prompt_id, content_hash, applied_at)
		VALUES (?, ?, ?)
		ON CONFLICT(prompt_id) DO UPDATE SET
			content_hash = excluded.content_hash,
			applied_at = excluded.applied_at
	`, p.ID, hash, now); err != nil {
		return fmt.Errorf("upsert system prompt version: %w", err)
	}

	return tx.Commit()
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
