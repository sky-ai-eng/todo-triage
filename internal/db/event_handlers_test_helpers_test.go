package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// createTriggerForTest is the raw-SQL replacement for SavePromptTrigger
// (now also for the unified event_handlers table, post-SKY-259). Used by
// tests in package db that need to seed a trigger row but can't import
// internal/db/sqlite (cycle: the sqlite package depends on db). Tests
// outside this package use stores.EventHandlers.Create directly.
//
// Always writes source='user' + kind='trigger'; the per-kind CHECK
// constraint on event_handlers enforces the trigger-only column set.
func createTriggerForTest(t *testing.T, database *sql.DB, trig domain.EventHandler) {
	t.Helper()
	now := time.Now().UTC()
	source := trig.Source
	if source == "" {
		source = domain.EventHandlerSourceUser
	}
	if trig.TriggerType == "" {
		trig.TriggerType = domain.TriggerTypeEvent
	}
	// creator_user_id is NULL for system rows, LocalDefaultUserID for
	// user rows — enforced by event_handlers_system_has_no_creator.
	var creatorUserID any
	if source == domain.EventHandlerSourceUser {
		creatorUserID = "00000000-0000-0000-0000-000000000100"
	}
	if _, err := database.Exec(`
		INSERT INTO event_handlers (id, kind, event_type, scope_predicate_json,
		                            breaker_threshold, min_autonomy_suitability,
		                            prompt_id, enabled, source, team_id, creator_user_id,
		                            created_at, updated_at)
		VALUES (?, 'trigger', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, trig.ID, trig.EventType, trig.ScopePredicateJSON,
		ptrDerefInt(trig.BreakerThreshold), ptrDerefFloat(trig.MinAutonomySuitability),
		trig.PromptID, trig.Enabled, source,
		"00000000-0000-0000-0000-000000000010", creatorUserID,
		now, now,
	); err != nil {
		t.Fatalf("createTriggerForTest %s: %v", trig.ID, err)
	}
}

func ptrDerefInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func ptrDerefFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// intPtr / floatPtr wrap literal values into pointers, used in test
// fixtures that build domain.EventHandler kind='trigger' rows (the
// per-kind fields are *int / *float64 because the columns are nullable
// at the schema level).
func intPtr(v int) *int           { return &v }
func floatPtr(v float64) *float64 { return &v }
