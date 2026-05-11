package postgres_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestSwipeStore_Postgres runs the shared SwipeStore conformance suite
// against the Postgres impl. Wired against AdminDB so creator_user_id
// resolution can fall back to org.owner_user_id without needing JWT
// claims plumbed on every subtest (the production claims path is
// covered separately by pgtest's RLS suite — D5 will exercise the
// request-scoped path end-to-end).
func TestSwipeStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunSwipeStoreConformance(t, func(t *testing.T) (db.SwipeStore, string, dbtest.TaskSeederForSwipes, dbtest.TaskReaderForSwipes) {
		t.Helper()
		h.Reset(t)
		orgID, userID := seedPgOrgAndUserForSwipes(t, h)
		stores := pgstore.New(h.AdminDB, h.AdminDB)

		seed := func(t *testing.T) string {
			t.Helper()
			return seedPgTaskForSwipes(t, h.AdminDB, orgID, userID)
		}
		read := func(t *testing.T, taskID string) (string, time.Time) {
			t.Helper()
			return readPgTask(t, h.AdminDB, taskID)
		}
		return stores.Swipes, orgID, seed, read
	})
}

func seedPgOrgAndUserForSwipes(t *testing.T, h *pgtest.Harness) (orgID, userID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	email := fmt.Sprintf("swipe-conf-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Swipe Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Swipe Conformance Org "+orgID[:8], "swipe-"+orgID[:8], userID,
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID,
	); err != nil {
		t.Fatalf("seed org_membership: %v", err)
	}
	return orgID, userID
}

func seedPgTaskForSwipes(t *testing.T, conn *sql.DB, orgID, userID string) string {
	t.Helper()
	now := time.Now().UTC()
	entityID := uuid.New().String()
	taskID := uuid.New().String()
	eventID := uuid.New().String()
	sourceID := fmt.Sprintf("swipe-conformance-%d", now.UnixNano())

	if _, err := conn.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Swipe Conformance', 'https://example/x', '{}'::jsonb, $4)
	`, entityID, orgID, sourceID, now); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, 'github:pr:opened', '', '{}'::jsonb, $4)
	`, eventID, orgID, entityID, now); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status, created_at)
		VALUES ($1, $2, $3, $4, 'github:pr:opened', '', $5, 'queued', 'pending', $6)
	`, taskID, orgID, userID, entityID, eventID, now); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return taskID
}

func readPgTask(t *testing.T, conn *sql.DB, taskID string) (string, time.Time) {
	t.Helper()
	var status string
	var snoozeUntil sql.NullTime
	if err := conn.QueryRow(`SELECT status, snooze_until FROM tasks WHERE id = $1`, taskID).Scan(&status, &snoozeUntil); err != nil {
		t.Fatalf("readPgTask %s: %v", taskID, err)
	}
	if snoozeUntil.Valid {
		return status, snoozeUntil.Time
	}
	return status, time.Time{}
}
