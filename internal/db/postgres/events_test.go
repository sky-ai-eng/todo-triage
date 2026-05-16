package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestEventStore_Postgres runs the shared conformance suite against
// the Postgres EventStore impl. Wires both pools against AdminDB
// (BYPASSRLS) so the behavior tests stay independent of the auth
// path; the cross-org isolation + RLS tests below exercise the
// org_id filter and the policy directly.
func TestEventStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB)

	dbtest.RunEventStoreConformance(t, func(t *testing.T) (db.EventStore, string, dbtest.EventStoreSeeder) {
		t.Helper()
		h.Reset(t)
		orgID := seedPgOrgForEvents(t, h)
		seed := dbtest.EventStoreSeeder{
			Entity: func(t *testing.T, suffix string) string {
				t.Helper()
				return seedPgEntityForEvents(t, h, orgID, suffix)
			},
		}
		return stores.Events, orgID, seed
	})
}

// TestEventStore_Postgres_CrossOrgLeakage pins the defense-in-depth
// org_id filter on every read + write path. RLS via events_all also
// enforces this in production; the org_id = $N clause in each query
// is the belt to RLS's suspenders that fires regardless of whether
// the admin pool bypasses RLS.
func TestEventStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB)

	orgA := seedPgOrgForEvents(t, h)
	orgB := seedPgOrgForEvents(t, h)
	ctx := context.Background()

	entityA := seedPgEntityForEvents(t, h, orgA, "cross-org-A")
	// Record an event in orgA via the admin-pool RecordSystem path —
	// the router's SKY-305 call site uses this admin variant.
	eid := entityA
	evtID, err := stores.Events.RecordSystem(ctx, orgA, domain.Event{
		EntityID:     &eid,
		EventType:    domain.EventGitHubPRCICheckPassed,
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("RecordSystem orgA: %v", err)
	}

	// Latest filtered by orgB must NOT see orgA's row.
	got, err := stores.Events.LatestForEntityTypeAndDedupKey(ctx, orgB, entityA, domain.EventGitHubPRCICheckPassed, "")
	if err != nil {
		t.Fatalf("Latest cross-org: %v", err)
	}
	if got != nil {
		t.Errorf("Latest under orgB returned orgA's row %s", got.ID)
	}

	// GetMetadataSystem filtered by orgB must also miss.
	meta, err := stores.Events.GetMetadataSystem(ctx, orgB, evtID)
	if err != nil {
		t.Fatalf("GetMetadataSystem cross-org: %v", err)
	}
	if meta != "" {
		t.Errorf("GetMetadataSystem under orgB returned %q; want empty", meta)
	}
}

// TestEventStore_Postgres_RLS_AppPoolRequiresClaims pins the
// events_all policy on the app pool: a Record without
// request.jwt.claims set is refused (tf.current_org_id() returns NULL
// → policy predicate fails), and the same Record inside a claims-set
// tx for the matching org succeeds while a tx claims-set for a
// different org is denied.
func TestEventStore_Postgres_RLS_AppPoolRequiresClaims(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgA := seedPgOrgForEvents(t, h)
	orgB := seedPgOrgForEvents(t, h)
	aliceA := mustOwnerUserForOrg(t, h, orgA)
	bobB := mustOwnerUserForOrg(t, h, orgB)
	entityA := seedPgEntityForEvents(t, h, orgA, "rls-app")
	eid := entityA

	// Subtest 1: without claims (app pool, no WithUser), Record is
	// refused — tf.current_org_id() is NULL inside a fresh tf_app
	// connection so the events_all USING/WITH CHECK predicate fails.
	t.Run("no_claims_refuses_record", func(t *testing.T) {
		// Construct a Store wired only against AppDB so Record routes
		// through the RLS-active pool with no claims set.
		stores := pgstore.New(h.AppDB, h.AppDB)
		_, err := stores.Events.Record(ctx, orgA, domain.Event{
			EntityID:  &eid,
			EventType: domain.EventGitHubPROpened,
		})
		if err == nil {
			t.Fatal("Record without JWT claims should have been refused by RLS")
		}
		// Postgres surfaces the row-level-security violation as code
		// 42501 (insufficient_privilege). Pin the code so a noisy
		// error message change doesn't silently weaken the test.
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("expected RLS violation (42501), got %v", err)
		}
	})

	// Subtest 2: inside WithUser(aliceA, orgA, ...) — claims match the
	// row's org — Record succeeds and the row is visible to the same
	// claims principal.
	t.Run("matching_org_claims_succeeds", func(t *testing.T) {
		err := h.WithUser(t, aliceA, orgA, func(tx *sql.Tx) error {
			txStores := pgstore.NewForTx(tx)
			id, err := txStores.Events.Record(ctx, orgA, domain.Event{
				EntityID:  &eid,
				EventType: domain.EventGitHubPRMerged,
			})
			if err != nil {
				return fmt.Errorf("Record: %w", err)
			}
			got, err := txStores.Events.LatestForEntityTypeAndDedupKey(ctx, orgA, entityA, domain.EventGitHubPRMerged, "")
			if err != nil {
				return fmt.Errorf("Latest: %w", err)
			}
			if got == nil || got.ID != id {
				return fmt.Errorf("Latest = %v, want id %s", got, id)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("matching claims: %v", err)
		}
	})

	// Subtest 3: inside WithUser(bobB, orgB, ...) — claims point at a
	// different org — Record into orgA's events row is refused.
	t.Run("cross_org_claims_denied", func(t *testing.T) {
		err := h.WithUser(t, bobB, orgB, func(tx *sql.Tx) error {
			txStores := pgstore.NewForTx(tx)
			_, err := txStores.Events.Record(ctx, orgA, domain.Event{
				EntityID:  &eid,
				EventType: domain.EventGitHubPRClosed,
			})
			return err
		})
		if err == nil {
			t.Fatal("Record into mismatched org should be refused by RLS")
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code != "42501" {
			t.Errorf("expected RLS violation (42501), got code %s: %v", pgErr.Code, err)
		}
	})
}

// TestEventStore_Postgres_HookDeferredUntilCommit pins the SKY-305
// rollback-safety invariant on the Postgres WithTx path:
// SetOnEventRecorded only fires for app-pool Record calls after the
// surrounding tx commits successfully. On rollback the
// LifetimeDistinctCounter must not observe the event.
//
// RecordSystem routes through the autonomous admin pool — its hook
// fires immediately on a successful INSERT regardless of the outer
// tx's fate — and is intentionally not exercised here.
func TestEventStore_Postgres_HookDeferredUntilCommit(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID := seedPgOrgForEvents(t, h)
	ownerID := mustOwnerUserForOrg(t, h, orgID)
	entityID := seedPgEntityForEvents(t, h, orgID, "hook-deferred")
	eid := entityID

	var mu sync.Mutex
	var fired []domain.Event
	db.SetOnEventRecorded(func(evt domain.Event) {
		mu.Lock()
		defer mu.Unlock()
		fired = append(fired, evt)
	})
	t.Cleanup(func() { db.SetOnEventRecorded(nil) })

	stores := pgstore.New(h.AdminDB, h.AppDB)
	ctx := context.Background()

	// Rollback path: fn returns an error after Record. Hook must
	// NOT fire — the row was written into the tx but never
	// committed; firing would inflate the LifetimeDistinctCounter
	// with a non-existent row.
	rollbackErr := errors.New("intentional rollback")
	err := stores.Tx.WithTx(ctx, orgID, ownerID, func(tx db.TxStores) error {
		if _, err := tx.Events.Record(ctx, orgID, domain.Event{
			EntityID: &eid, EventType: domain.EventGitHubPROpened,
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
		return rollbackErr
	})
	if !errors.Is(err, rollbackErr) {
		t.Fatalf("WithTx err = %v, want %v", err, rollbackErr)
	}
	mu.Lock()
	if len(fired) != 0 {
		t.Errorf("hook fired %d times after rollback; want 0", len(fired))
	}
	mu.Unlock()

	// Confirm the row really didn't land — defense against a future
	// refactor that silently breaks rollback. Read via admin pool to
	// bypass RLS / ownerID-claims wiring.
	var count int
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE org_id = $1 AND entity_id = $2 AND event_type = $3`,
		orgID, entityID, domain.EventGitHubPROpened,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("event row count = %d after rollback; want 0", count)
	}

	// Commit path: same Record, fn returns nil. Hook fires exactly
	// once after commit, with the persisted event's ID.
	err = stores.Tx.WithTx(ctx, orgID, ownerID, func(tx db.TxStores) error {
		_, err := tx.Events.Record(ctx, orgID, domain.Event{
			EntityID: &eid, EventType: domain.EventGitHubPRMerged,
		})
		return err
	})
	if err != nil {
		t.Fatalf("WithTx commit: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 1 {
		t.Fatalf("hook fired %d times after commit; want 1", len(fired))
	}
	if fired[0].EventType != domain.EventGitHubPRMerged {
		t.Errorf("hook saw event_type=%q, want %q", fired[0].EventType, domain.EventGitHubPRMerged)
	}
}

// seedPgOrgForEvents stages (user, org, org_membership, team) so
// events.org_id FK satisfies and the entity rows the conformance
// suite seeds against this org pass the entities_all RLS predicate
// when accessed under the org's owner.
func seedPgOrgForEvents(t *testing.T, h *pgtest.Harness) string {
	t.Helper()
	orgID := uuid.New().String()
	userID := uuid.New().String()
	email := fmt.Sprintf("events-conf-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Events Conformance User",
	); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Events Org "+orgID[:8], "events-"+orgID[:8], userID,
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID,
	); err != nil {
		t.Fatalf("seed org_membership: %v", err)
	}
	seedPgDefaultTeam(t, h, orgID, userID)
	return orgID
}

// seedPgEntityForEvents inserts an active GitHub PR entity in the
// given org. Direct INSERT (not EntityStore) so the conformance
// fixture path is schema-coupled and short — matches the SwipeStore
// / RepoStore seed pattern.
func seedPgEntityForEvents(t *testing.T, h *pgtest.Harness, orgID, suffix string) string {
	t.Helper()
	id := uuid.New().String()
	now := time.Now().UTC()
	sourceID := fmt.Sprintf("events-conf-%s-%s-%d", orgID[:8], suffix, now.UnixNano())
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at, state)
		VALUES ($1, $2, 'github', $3, 'pr', 'Events Conformance', 'https://example/x', '{}'::jsonb, $4, 'active')
	`, id, orgID, sourceID, now); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	return id
}
