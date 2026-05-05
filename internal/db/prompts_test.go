package db

import (
	"testing"
	"time"

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

	var (
		hashBefore    string
		appliedBefore time.Time
	)
	if err := database.QueryRow(
		`SELECT content_hash, applied_at FROM system_prompt_versions WHERE prompt_id = 'system-y'`,
	).Scan(&hashBefore, &appliedBefore); err != nil {
		t.Fatalf("read version row: %v", err)
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

	// The version row must not be touched when the prompt is user-modified:
	// otherwise we'd claim the new shipped hash was "applied" even though
	// we deliberately skipped writing it.
	var (
		hashAfter    string
		appliedAfter time.Time
	)
	if err := database.QueryRow(
		`SELECT content_hash, applied_at FROM system_prompt_versions WHERE prompt_id = 'system-y'`,
	).Scan(&hashAfter, &appliedAfter); err != nil {
		t.Fatalf("read version row after seed: %v", err)
	}
	if hashAfter != hashBefore {
		t.Fatalf("content_hash changed for user-modified prompt: before=%s after=%s", hashBefore, hashAfter)
	}
	if !appliedAfter.Equal(appliedBefore) {
		t.Fatalf("applied_at changed for user-modified prompt: before=%s after=%s", appliedBefore, appliedAfter)
	}
}

func TestSeedOrUpdateSystemPrompt_OverwritesLegacyPromptWithoutVersionRow(t *testing.T) {
	database := newTestDB(t)

	if _, err := database.Exec(
		`INSERT INTO prompts (id, name, body, source) VALUES (?, ?, ?, ?)`,
		"system-z",
		"Legacy",
		"legacy body",
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
	if p.Body != "v2" {
		t.Fatalf("body=%q want v2", p.Body)
	}
	if p.Name != "Z" {
		t.Fatalf("name=%q want Z", p.Name)
	}

	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM system_prompt_versions WHERE prompt_id = 'system-z'`).Scan(&count); err != nil {
		t.Fatalf("count version rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("version rows=%d want 1 after overwriting legacy prompt", count)
	}
}

func TestSeedOrUpdateSystemPrompt_UpdatesOnMetadataChange(t *testing.T) {
	database := newTestDB(t)

	if err := SeedOrUpdateSystemPrompt(database, domain.Prompt{ID: "system-m", Name: "Old Name", Body: "same body", Source: "system"}); err != nil {
		t.Fatalf("seed v1: %v", err)
	}
	var hashBefore string
	if err := database.QueryRow(`SELECT content_hash FROM system_prompt_versions WHERE prompt_id = 'system-m'`).Scan(&hashBefore); err != nil {
		t.Fatalf("read version row: %v", err)
	}

	// Sleep so any churn would produce a strictly-greater timestamp.
	time.Sleep(5 * time.Millisecond)

	if err := SeedOrUpdateSystemPrompt(database, domain.Prompt{ID: "system-m", Name: "New Name", Body: "same body", Source: "system"}); err != nil {
		t.Fatalf("seed with renamed name: %v", err)
	}

	p, err := GetPrompt(database, "system-m")
	if err != nil {
		t.Fatalf("get prompt: %v", err)
	}
	if p.Name != "New Name" {
		t.Fatalf("name=%q want New Name; metadata-only change should be applied", p.Name)
	}

	var hashAfter string
	if err := database.QueryRow(`SELECT content_hash FROM system_prompt_versions WHERE prompt_id = 'system-m'`).Scan(&hashAfter); err != nil {
		t.Fatalf("read version row after: %v", err)
	}
	if hashAfter == hashBefore {
		t.Fatalf("content_hash unchanged after name change; hash must cover metadata")
	}
}

// Reseeding identical content must be a true no-op: prompts.updated_at is
// what the UI orders by, so bumping it on every startup would constantly
// shuffle system prompts to the top. system_prompt_versions.applied_at is
// also load-bearing for "did the new shipped hash actually get applied".
func TestSeedOrUpdateSystemPrompt_NoChurnWhenContentUnchanged(t *testing.T) {
	database := newTestDB(t)

	if err := SeedOrUpdateSystemPrompt(database, domain.Prompt{ID: "system-q", Name: "Q", Body: "v1", Source: "system"}); err != nil {
		t.Fatalf("seed v1: %v", err)
	}

	var (
		updatedBefore time.Time
		appliedBefore time.Time
	)
	if err := database.QueryRow(`SELECT updated_at FROM prompts WHERE id = 'system-q'`).Scan(&updatedBefore); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	if err := database.QueryRow(
		`SELECT applied_at FROM system_prompt_versions WHERE prompt_id = 'system-q'`,
	).Scan(&appliedBefore); err != nil {
		t.Fatalf("read applied_at: %v", err)
	}

	// Sleep so any churn would produce a strictly-greater timestamp.
	time.Sleep(5 * time.Millisecond)

	if err := SeedOrUpdateSystemPrompt(database, domain.Prompt{ID: "system-q", Name: "Q", Body: "v1", Source: "system"}); err != nil {
		t.Fatalf("reseed v1: %v", err)
	}

	var (
		updatedAfter time.Time
		appliedAfter time.Time
	)
	if err := database.QueryRow(`SELECT updated_at FROM prompts WHERE id = 'system-q'`).Scan(&updatedAfter); err != nil {
		t.Fatalf("read updated_at after: %v", err)
	}
	if err := database.QueryRow(
		`SELECT applied_at FROM system_prompt_versions WHERE prompt_id = 'system-q'`,
	).Scan(&appliedAfter); err != nil {
		t.Fatalf("read applied_at after: %v", err)
	}

	if !updatedAfter.Equal(updatedBefore) {
		t.Fatalf("prompts.updated_at churned on identical reseed: before=%s after=%s", updatedBefore, updatedAfter)
	}
	if !appliedAfter.Equal(appliedBefore) {
		t.Fatalf("system_prompt_versions.applied_at churned on identical reseed: before=%s after=%s", appliedBefore, appliedAfter)
	}
}
