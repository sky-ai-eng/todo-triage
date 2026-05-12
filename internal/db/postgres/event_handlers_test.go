package postgres_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestEventHandlerStore_Postgres runs the shared conformance suite
// against the Postgres impl. AdminDB serves both pools so Seed
// (admin-only, no JWT claims at boot) and CRUD reads (app pool with
// claims) both work without per-subtest plumbing — same shape the
// other Postgres conformance tests use.
//
// The PromptSeeder inserts a system-source prompt per requested id;
// event_handlers.prompt_id has a composite FK to prompts(id, org_id),
// so trigger fixtures need real prompt rows to point at.
func TestEventHandlerStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunEventHandlerStoreConformance(t, func(t *testing.T) (db.EventHandlerStore, string, dbtest.PromptSeeder) {
		t.Helper()
		h.Reset(t)
		orgID := seedPgOrgForAgents(t, h)
		stores := pgstore.New(h.AdminDB, h.AdminDB)
		seed := func(t *testing.T, ids ...string) {
			t.Helper()
			for _, id := range ids {
				// system-source rows ship with creator_user_id NULL +
				// visibility='org' (per the system_rows_nullable_creator
				// migration). Insert via AdminDB so the prompts_insert
				// RLS doesn't block on missing JWT claims.
				if _, err := h.AdminDB.Exec(`
					INSERT INTO prompts (id, org_id, creator_user_id, visibility, source, name, body)
					VALUES ($1, $2, NULL, 'org', 'system', $3, 'test body')
					ON CONFLICT (org_id, id) DO NOTHING
				`, id, orgID, id); err != nil {
					t.Fatalf("seed prompt %s: %v", id, err)
				}
			}
		}
		return stores.EventHandlers, orgID, seed
	})
}
