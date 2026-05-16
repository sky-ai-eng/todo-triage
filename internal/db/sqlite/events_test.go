package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestEventStore_SQLite runs the shared conformance suite against
// the SQLite EventStore impl. Each subtest opens a fresh in-memory
// DB so events state doesn't leak between assertions. Hook listeners
// are torn down via t.Cleanup inside the suite so cross-subtest
// hooks can't observe earlier events.
func TestEventStore_SQLite(t *testing.T) {
	dbtest.RunEventStoreConformance(t, func(t *testing.T) (db.EventStore, string, dbtest.EventStoreSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		seed := dbtest.EventStoreSeeder{
			Entity: func(t *testing.T, suffix string) string {
				t.Helper()
				return seedSQLiteEntityForEvents(t, conn, suffix)
			},
		}
		return stores.Events, runmode.LocalDefaultOrg, seed
	})
}

// TestEventStore_SQLite_RejectsNonLocalOrg pins the assertLocalOrg
// gate at every method entry. Non-default org IDs are a confused
// caller (they think they're in multi mode); reject loudly so the
// silent default-org fallthrough never happens.
func TestEventStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	const badOrg = "11111111-1111-1111-1111-111111111111"
	evt := domain.Event{EventType: domain.EventGitHubPROpened}
	if _, err := stores.Events.Record(ctx, badOrg, evt); err == nil {
		t.Error("Record(non-local org) should error")
	}
	if _, err := stores.Events.RecordSystem(ctx, badOrg, evt); err == nil {
		t.Error("RecordSystem(non-local org) should error")
	}
	if _, err := stores.Events.LatestForEntityTypeAndDedupKey(ctx, badOrg, "any", "any", ""); err == nil {
		t.Error("Latest(non-local org) should error")
	}
	if _, err := stores.Events.GetMetadataSystem(ctx, badOrg, "any"); err == nil {
		t.Error("GetMetadataSystem(non-local org) should error")
	}
}

// TestEventStore_SQLite_HookDeferredUntilCommit pins the SKY-305
// rollback-safety invariant: SetOnEventRecorded must not fire for
// Record calls inside a WithTx that rolls back. Without this, the
// LifetimeDistinctCounter cache observes an event the DB never
// persisted and stays inflated until restart hydration.
func TestEventStore_SQLite_HookDeferredUntilCommit(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)

	var mu sync.Mutex
	var fired []domain.Event
	db.SetOnEventRecorded(func(evt domain.Event) {
		mu.Lock()
		defer mu.Unlock()
		fired = append(fired, evt)
	})
	t.Cleanup(func() { db.SetOnEventRecorded(nil) })

	entityID := seedSQLiteEntityForEvents(t, conn, "hook-deferred")
	eid := entityID

	// Rollback path: fn returns an error after recording an event.
	// Hook must NOT fire.
	rollbackErr := errors.New("intentional rollback")
	err := stores.Tx.WithTx(context.Background(), runmode.LocalDefaultOrg, runmode.LocalDefaultUserID,
		func(tx db.TxStores) error {
			if _, err := tx.Events.Record(context.Background(), runmode.LocalDefaultOrg, domain.Event{
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
		t.Errorf("hook fired %d times after rollback; want 0 — counter would observe a row the DB never persisted", len(fired))
	}
	mu.Unlock()

	// Commit path: same Record, fn returns nil. Hook must fire
	// exactly once after the commit.
	err = stores.Tx.WithTx(context.Background(), runmode.LocalDefaultOrg, runmode.LocalDefaultUserID,
		func(tx db.TxStores) error {
			_, err := tx.Events.Record(context.Background(), runmode.LocalDefaultOrg, domain.Event{
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

// seedSQLiteEntityForEvents inserts an active GitHub PR entity for
// EventStore conformance fixtures. Direct INSERT (not the
// EntityStore) so the conformance harness stays close to the
// SwipeStore/RepoStore seeding pattern — small, schema-coupled, and
// independent of other stores' behavior changes.
func seedSQLiteEntityForEvents(t *testing.T, conn *sql.DB, suffix string) string {
	t.Helper()
	id := uuid.New().String()
	now := time.Now().UTC()
	sourceID := fmt.Sprintf("events-conformance-%s-%d", suffix, now.UnixNano())
	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES (?, 'github', ?, 'pr', 'Events Conformance', 'https://example/x', '{}', ?)
	`, id, sourceID, now); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	return id
}
