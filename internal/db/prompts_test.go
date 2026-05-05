package db

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func TestSeedOrUpdateSystemPrompt_UpdatesUntouchedPrompt(t *testing.T) {
	database := newTestDB(t)

	if err := SeedOrUpdateSystemPrompt(database, domain.Prompt{ID: "system-x", Name: "X", Body: "v1", Source: "system"}); err != nil {
		t.Fatalf("seed v1: %v", err)
	}
	if err := SeedOrUpdateSystemPrompt(database, domain.Prompt{ID: "system-x", Name: "X2", Body: "v2", Source: "system"}); err != nil {
		t.Fatalf("seed v2: %v", err)
	}

	p, err := GetPrompt(database, "system-x")
	if err != nil {
		t.Fatalf("get prompt: %v", err)
	}
	if p.Body != "v2" {
		t.Fatalf("body=%q want v2", p.Body)
	}
	if p.Name != "X2" {
		t.Fatalf("name=%q want X2", p.Name)
	}

	var hash string
	if err := database.QueryRow(`SELECT content_hash FROM system_prompt_versions WHERE prompt_id = 'system-x'`).Scan(&hash); err != nil {
		t.Fatalf("version row: %v", err)
	}
	if hash == "" {
		t.Fatalf("content_hash should be set")
	}
}

func TestSeedOrUpdateSystemPrompt_PreservesUserModifiedPrompt(t *testing.T) {
	database := newTestDB(t)

	if err := SeedOrUpdateSystemPrompt(database, domain.Prompt{ID: "system-y", Name: "Y", Body: "v1", Source: "system"}); err != nil {
		t.Fatalf("seed v1: %v", err)
	}
	if err := UpdatePrompt(database, "system-y", "Custom", "custom body"); err != nil {
		t.Fatalf("user update: %v", err)
	}
	if err := SeedOrUpdateSystemPrompt(database, domain.Prompt{ID: "system-y", Name: "Y", Body: "v2", Source: "system"}); err != nil {
		t.Fatalf("seed v2: %v", err)
	}

	p, err := GetPrompt(database, "system-y")
	if err != nil {
		t.Fatalf("get prompt: %v", err)
	}
	if p.Body != "custom body" {
		t.Fatalf("body=%q want custom body", p.Body)
	}
	if p.Name != "Custom" {
		t.Fatalf("name=%q want Custom", p.Name)
	}
}

func TestSeedOrUpdateSystemPrompt_PreservesLegacyEditedPromptWithoutVersionRow(t *testing.T) {
	database := newTestDB(t)

	if _, err := database.Exec(
		`INSERT INTO prompts (id, name, body, source) VALUES (?, ?, ?, ?)`,
		"system-z",
		"Legacy Custom",
		"legacy custom body",
		"system",
	); err != nil {
		t.Fatalf("insert legacy prompt: %v", err)
	}

	if err := SeedOrUpdateSystemPrompt(database, domain.Prompt{ID: "system-z", Name: "Z", Body: "v2", Source: "system"}); err != nil {
		t.Fatalf("seed v2: %v", err)
	}

	p, err := GetPrompt(database, "system-z")
	if err != nil {
		t.Fatalf("get prompt: %v", err)
	}
	if p.Body != "legacy custom body" {
		t.Fatalf("body=%q want legacy custom body", p.Body)
	}
	if p.Name != "Legacy Custom" {
		t.Fatalf("name=%q want Legacy Custom", p.Name)
	}

	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM system_prompt_versions WHERE prompt_id = 'system-z'`).Scan(&count); err != nil {
		t.Fatalf("count version rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("version rows=%d want 0 for legacy prompt without prior version tracking", count)
	}
}
