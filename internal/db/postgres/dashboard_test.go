package postgres_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestDashboardStore_Postgres runs the shared DashboardStore
// conformance suite against the Postgres impl. The seeder casts
// the marshaled JSON to JSONB so it lands in the typed column;
// snapshot_json is JSONB in D3.
func TestDashboardStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunDashboardStoreConformance(t, func(t *testing.T) (db.DashboardStore, string, dbtest.PRSnapshotSeederForDashboard) {
		t.Helper()
		h.Reset(t)
		orgID, _ := seedPgOrgAndUserForDashboard(t, h)
		stores := pgstore.New(h.AdminDB, h.AdminDB)
		seed := func(t *testing.T, snap domain.PRSnapshot) {
			t.Helper()
			seedPgPRSnapshot(t, h.AdminDB, orgID, snap)
		}
		return stores.Dashboard, orgID, seed
	})
}

func seedPgOrgAndUserForDashboard(t *testing.T, h *pgtest.Harness) (orgID, userID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	email := fmt.Sprintf("dash-conf-%s@test.local", userID[:8])
	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(`INSERT INTO users (id, display_name) VALUES ($1, $2)`, userID, "Dash Conformance User"); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	if _, err := h.AdminDB.Exec(`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Dash Conformance Org "+orgID[:8], "dash-"+orgID[:8], userID); err != nil {
		t.Fatalf("seed orgs: %v", err)
	}
	if _, err := h.AdminDB.Exec(`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID); err != nil {
		t.Fatalf("seed org_memberships: %v", err)
	}
	return orgID, userID
}

func seedPgPRSnapshot(t *testing.T, conn *sql.DB, orgID string, snap domain.PRSnapshot) {
	t.Helper()
	blob, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	now := time.Now().UTC()
	entityID := uuid.New().String()
	sourceID := fmt.Sprintf("dashboard-conformance-%d-%d", snap.Number, now.UnixNano())
	if _, err := conn.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at, last_polled_at)
		VALUES ($1, $2, 'github', $3, 'pr', $4, $5, $6::jsonb, $7, $7)
	`, entityID, orgID, sourceID, snap.Title, snap.URL, string(blob), now); err != nil {
		t.Fatalf("seed entity for snapshot %d: %v", snap.Number, err)
	}
}
