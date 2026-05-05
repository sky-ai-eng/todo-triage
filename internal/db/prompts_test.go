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
