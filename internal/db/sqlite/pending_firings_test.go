package sqlite_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestPendingFiringsStore_SQLite runs the shared conformance suite
// against the SQLite PendingFiringsStore impl. Each subtest gets a
// fresh in-memory DB; the seeder closure builds entity/task/trigger/
// event/run fixtures inline with raw SQL since the schema's NOT NULL
// columns all carry DEFAULTs that the local sentinel constants
// satisfy.
func TestPendingFiringsStore_SQLite(t *testing.T) {
	dbtest.RunPendingFiringsStoreConformance(t, func(t *testing.T) (db.PendingFiringsStore, string, dbtest.PendingFiringsSeeder) {
		t.Helper()
		conn := newSQLiteForPendingFiringsTest(t)
		stores := sqlitestore.New(conn)
		return stores.PendingFirings, runmode.LocalDefaultOrgID, newSQLitePendingFiringsSeeder(conn)
	})
}

// TestPendingFiringsStore_SQLite_RejectsNonLocalOrg pins the
// assertLocalOrg guard on every method. The runs/tasks-shaped methods
// (HasActiveAutoRunForEntity, EntityCanFireImmediately) also fire the
// guard before touching the data — important because their queries
// would otherwise return false for any orgID by joining away the rows.
func TestPendingFiringsStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := newSQLiteForPendingFiringsTest(t)
	stores := sqlitestore.New(conn)

	const bogusOrg = "11111111-1111-1111-1111-111111111111"
	ctx := t.Context()

	if _, err := stores.PendingFirings.Enqueue(ctx, bogusOrg, runmode.LocalDefaultUserID, "e", "t", "tr", "ev"); err == nil {
		t.Errorf("Enqueue with non-local orgID should error")
	}
	if _, err := stores.PendingFirings.PopForEntity(ctx, bogusOrg, "e"); err == nil {
		t.Errorf("PopForEntity with non-local orgID should error")
	}
	if err := stores.PendingFirings.MarkFired(ctx, bogusOrg, 1, "r"); err == nil {
		t.Errorf("MarkFired with non-local orgID should error")
	}
	if err := stores.PendingFirings.MarkSkipped(ctx, bogusOrg, 1, "reason"); err == nil {
		t.Errorf("MarkSkipped with non-local orgID should error")
	}
	if _, err := stores.PendingFirings.HasActiveAutoRunForEntity(ctx, bogusOrg, "e"); err == nil {
		t.Errorf("HasActiveAutoRunForEntity with non-local orgID should error")
	}
	if _, err := stores.PendingFirings.HasPendingForEntity(ctx, bogusOrg, "e"); err == nil {
		t.Errorf("HasPendingForEntity with non-local orgID should error")
	}
	if _, err := stores.PendingFirings.EntityCanFireImmediately(ctx, bogusOrg, "e"); err == nil {
		t.Errorf("EntityCanFireImmediately with non-local orgID should error")
	}
	if _, err := stores.PendingFirings.ListEntitiesWithPending(ctx, bogusOrg); err == nil {
		t.Errorf("ListEntitiesWithPending with non-local orgID should error")
	}
	if _, err := stores.PendingFirings.ListForEntity(ctx, bogusOrg, "e"); err == nil {
		t.Errorf("ListForEntity with non-local orgID should error")
	}
}

func newSQLiteForPendingFiringsTest(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return conn
}

// newSQLitePendingFiringsSeeder returns a closure-bound seeder bag.
// Every Tuple call inserts a fresh (entity, task, event_handler, event)
// chain so dedup keys stay distinct across subtests.
func newSQLitePendingFiringsSeeder(conn *sql.DB) dbtest.PendingFiringsSeeder {
	tuple := func(t *testing.T) dbtest.PendingFiringsTuple {
		t.Helper()
		suf := uuid.New().String()[:8]
		entityID := "e-" + suf
		eventID := "ev-" + suf
		taskID := "t-" + suf
		triggerID := "tr-" + suf
		promptID := "p-" + suf

		// entity: synthetic id keeps the (source, source_id)
		// UNIQUE happy across subtests.
		if _, err := conn.Exec(`
			INSERT INTO entities (id, source, source_id, kind, title, url)
			VALUES (?, 'github', ?, 'pr', 'Test PR', '')
		`, entityID, "owner/repo#"+suf); err != nil {
			t.Fatalf("seed entity: %v", err)
		}

		// prompt: triggers + runs both FK to prompts(id).
		// source='user' requires creator_user_id non-null per the
		// prompts_system_has_no_creator CHECK.
		if _, err := conn.Exec(`
			INSERT INTO prompts (id, name, body, source, creator_user_id, team_id)
			VALUES (?, 'Test', 'x', 'user', ?, ?)
		`, promptID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID); err != nil {
			t.Fatalf("seed prompt: %v", err)
		}

		// event: pending_firings.triggering_event_id FKs to events(id).
		// event_type uses a real catalog entry so the
		// REFERENCES events_catalog(id) FK is satisfied.
		if _, err := conn.Exec(`
			INSERT INTO events (id, entity_id, event_type, dedup_key)
			VALUES (?, ?, ?, '')
		`, eventID, entityID, domain.EventGitHubPRCICheckFailed); err != nil {
			t.Fatalf("seed event: %v", err)
		}

		// task: pending_firings.task_id FKs to tasks(id). Defaults
		// cover org_id/team_id/creator_user_id/visibility.
		if _, err := conn.Exec(`
			INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status)
			VALUES (?, ?, ?, '', ?, 'queued', 'pending')
		`, taskID, entityID, domain.EventGitHubPRCICheckFailed, eventID); err != nil {
			t.Fatalf("seed task: %v", err)
		}

		// event_handler (trigger kind): FK target of
		// pending_firings.trigger_id. The kind-specific CHECK requires
		// triggers to set prompt_id + breaker_threshold +
		// min_autonomy_suitability and to leave the rule-only columns
		// (name, default_priority, sort_order) NULL.
		if _, err := conn.Exec(`
			INSERT INTO event_handlers (id, kind, event_type, prompt_id, breaker_threshold, min_autonomy_suitability, enabled, source, creator_user_id)
			VALUES (?, 'trigger', ?, ?, 4, 0, 1, 'user', ?)
		`, triggerID, domain.EventGitHubPRCICheckFailed, promptID, runmode.LocalDefaultUserID); err != nil {
			t.Fatalf("seed trigger: %v", err)
		}

		return dbtest.PendingFiringsTuple{
			EntityID:  entityID,
			TaskID:    taskID,
			TriggerID: triggerID,
			EventID:   eventID,
			UserID:    runmode.LocalDefaultUserID,
		}
	}

	insertRun := func(t *testing.T, taskID, triggerType, status string) string {
		t.Helper()
		runID := "r-" + uuid.New().String()[:8]
		// runs.trigger_id is nullable; manual runs leave it empty.
		// prompts(id) FK: reuse the test's prompt via a subquery on
		// the task's primary_event_id is overkill — just pick any
		// prompt row that exists in this DB. The task seeder above
		// created one with a deterministic suffix, but we don't know
		// the suffix here. Insert a one-off prompt scoped to this run.
		promptID := "p-run-" + runID
		if _, err := conn.Exec(`INSERT INTO prompts (id, name, body, source, creator_user_id, team_id) VALUES (?, 'r', 'x', 'user', ?, ?)`,
			promptID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID); err != nil {
			t.Fatalf("seed run prompt: %v", err)
		}
		var triggerBind any
		if triggerType == "event" {
			// runs.trigger_id REFERENCES event_handlers(id). For event
			// runs we need a real trigger row; reuse a synthetic one.
			trigID := "trig-run-" + runID
			if _, err := conn.Exec(`
				INSERT INTO event_handlers (id, kind, event_type, prompt_id, breaker_threshold, min_autonomy_suitability, enabled, source, creator_user_id)
				VALUES (?, 'trigger', ?, ?, 4, 0, 1, 'user', ?)
			`, trigID, domain.EventGitHubPRCICheckFailed, promptID, runmode.LocalDefaultUserID); err != nil {
				t.Fatalf("seed run trigger: %v", err)
			}
			triggerBind = trigID
		} else {
			triggerBind = nil
		}
		// runs_creator_matches_trigger_type: trigger_type='event' rows
		// must have creator_user_id NULL; trigger_type='manual' rows
		// must have it set. The DEFAULT clause lays down the sentinel
		// regardless, so override explicitly for event runs.
		var creatorBind any
		if triggerType == "event" {
			creatorBind = nil
		} else {
			creatorBind = runmode.LocalDefaultUserID
		}
		if _, err := conn.Exec(`
			INSERT INTO runs (id, task_id, prompt_id, trigger_id, trigger_type, status, started_at, creator_user_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, runID, taskID, promptID, triggerBind, triggerType, status, time.Now(), creatorBind); err != nil {
			t.Fatalf("seed run: %v", err)
		}
		return runID
	}

	return dbtest.PendingFiringsSeeder{
		Tuple: tuple,
		ActiveAutoRun: func(t *testing.T, taskID string) string {
			return insertRun(t, taskID, "event", "running")
		},
		TerminalAutoRun: func(t *testing.T, taskID string) string {
			return insertRun(t, taskID, "event", "completed")
		},
		ManualRun: func(t *testing.T, taskID string) string {
			return insertRun(t, taskID, "manual", "running")
		},
	}
}
