package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// createTriggerForTest is the raw-SQL replacement for the deleted
// SavePromptTrigger free-function. Used by tests in package db that
// need to seed a prompt_trigger row but can't import
// internal/db/sqlite (cycle: the sqlite package depends on db).
// Tests outside this package use stores.Triggers.Create directly.
//
// Always writes source='user' (matching the deleted free-function's
// implicit behavior); tests that need a system-source row should set
// it via raw SQL or call stores.Triggers.Seed.
func createTriggerForTest(t *testing.T, database *sql.DB, trig domain.PromptTrigger) {
	t.Helper()
	now := time.Now().UTC()
	source := trig.Source
	if source == "" {
		source = domain.PromptTriggerSourceUser
	}
	if _, err := database.Exec(`
		INSERT INTO prompt_triggers (id, prompt_id, trigger_type, event_type, scope_predicate_json,
		                             breaker_threshold, min_autonomy_suitability, enabled, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, trig.ID, trig.PromptID, trig.TriggerType, trig.EventType, trig.ScopePredicateJSON,
		trig.BreakerThreshold, trig.MinAutonomySuitability, trig.Enabled, source, now, now); err != nil {
		t.Fatalf("createTriggerForTest %s: %v", trig.ID, err)
	}
}

// setTriggerEnabledForTest mirrors the deleted SetTriggerEnabled
// free-function for in-package tests.
func setTriggerEnabledForTest(t *testing.T, database *sql.DB, id string, enabled bool) {
	t.Helper()
	if _, err := database.Exec(
		`UPDATE prompt_triggers SET enabled = ?, updated_at = ? WHERE id = ?`,
		enabled, time.Now().UTC(), id,
	); err != nil {
		t.Fatalf("setTriggerEnabledForTest %s: %v", id, err)
	}
}
