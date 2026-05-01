package db

import (
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestLifetimeDistinctCounter_HydrateMatchesSQL pins the contract that
// the in-memory counter, after Hydrate, returns the same per-event-type
// counts as the canonical SQL aggregate. Production is allowed to drift
// from the DB only if the RecordEvent hook is unset for some reason —
// Hydrate is where the cache and the DB agree by construction.
func TestLifetimeDistinctCounter_HydrateMatchesSQL(t *testing.T) {
	database := newTestDB(t)
	a := makeEntity(t, database, 1)
	b := makeEntity(t, database, 2)
	c := makeEntity(t, database, 3)
	recordEvent(t, database, a.ID, domain.EventGitHubPROpened)
	recordEvent(t, database, a.ID, domain.EventGitHubPRMerged)
	recordEvent(t, database, b.ID, domain.EventGitHubPROpened)
	recordEvent(t, database, b.ID, domain.EventGitHubPRCICheckFailed)
	recordEvent(t, database, b.ID, domain.EventGitHubPRCICheckFailed) // re-entry
	recordEvent(t, database, c.ID, domain.EventGitHubPROpened)
	recordEvent(t, database, c.ID, domain.EventGitHubPRMerged)

	want, err := DistinctEntityCountsByEventTypeLifetime(database)
	if err != nil {
		t.Fatalf("SQL aggregate: %v", err)
	}

	counter := NewLifetimeDistinctCounter()
	if err := counter.Hydrate(database); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	got := counter.Snapshot()

	if len(got) != len(want) {
		t.Fatalf("len(snapshot) = %d, len(sql) = %d", len(got), len(want))
	}
	for et, n := range want {
		if got[et] != n {
			t.Errorf("snapshot[%q] = %d, sql aggregate = %d", et, got[et], n)
		}
	}
}

// TestLifetimeDistinctCounter_RecordExtendsHydrate is the "warm cache"
// path: after the initial scan the RecordEvent hook feeds new events
// in via Record, and the snapshot must reflect them without another
// DB round-trip. New entity → count goes up. Same entity at the same
// station → count stays.
func TestLifetimeDistinctCounter_RecordExtendsHydrate(t *testing.T) {
	database := newTestDB(t)
	a := makeEntity(t, database, 1)
	recordEvent(t, database, a.ID, domain.EventGitHubPROpened)

	counter := NewLifetimeDistinctCounter()
	if err := counter.Hydrate(database); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	// New entity hits the same station → count goes from 1 to 2.
	bID := "entity-b"
	counter.Record(domain.Event{
		EventType: domain.EventGitHubPROpened,
		EntityID:  &bID,
	})
	if got := counter.Snapshot()[domain.EventGitHubPROpened]; got != 2 {
		t.Errorf("after Record(new entity): count = %d, want 2", got)
	}

	// Same entity at the same station → no double-count.
	counter.Record(domain.Event{
		EventType: domain.EventGitHubPROpened,
		EntityID:  &bID,
	})
	if got := counter.Snapshot()[domain.EventGitHubPROpened]; got != 2 {
		t.Errorf("after Record(re-entry): count = %d, want 2", got)
	}

	// New event type for an existing entity → distinct entry.
	counter.Record(domain.Event{
		EventType: domain.EventGitHubPRMerged,
		EntityID:  &bID,
	})
	if got := counter.Snapshot()[domain.EventGitHubPRMerged]; got != 1 {
		t.Errorf("after Record(new event type): count = %d, want 1", got)
	}
}

// TestLifetimeDistinctCounter_RecordIgnoresSystemEvents confirms events
// with no entity_id (system poll markers, scoring sentinels) don't
// inflate any station's count when they pass through Record. Mirrors
// the SQL aggregate's `WHERE entity_id IS NOT NULL` filter — the in-
// memory path needs the same guard or every system event would
// double-count whichever event_type it carries.
func TestLifetimeDistinctCounter_RecordIgnoresSystemEvents(t *testing.T) {
	counter := NewLifetimeDistinctCounter()

	counter.Record(domain.Event{EventType: domain.EventGitHubPROpened, EntityID: nil})
	emptyID := ""
	counter.Record(domain.Event{EventType: domain.EventGitHubPROpened, EntityID: &emptyID})

	if got := counter.Snapshot()[domain.EventGitHubPROpened]; got != 0 {
		t.Errorf("system events should not count: got %d, want 0", got)
	}
}

// TestLifetimeDistinctCounter_HookCatchesDirectRecordEvent confirms
// the SetOnEventRecorded hook fires for direct RecordEvent callers
// (tracker backfill, Jira carry-over) that bypass the eventbus —
// the regression for the original bus-only-subscriber design where
// those callers silently drifted the cache from the DB until restart.
func TestLifetimeDistinctCounter_HookCatchesDirectRecordEvent(t *testing.T) {
	database := newTestDB(t)
	counter := NewLifetimeDistinctCounter()
	if err := counter.Hydrate(database); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	SetOnEventRecorded(counter.Record)
	t.Cleanup(func() { SetOnEventRecorded(nil) })

	a := makeEntity(t, database, 1)
	// Direct RecordEvent — no bus involvement at all. The hook is the
	// only path that gets the counter updated.
	recordEvent(t, database, a.ID, domain.EventGitHubPROpened)

	if got := counter.Snapshot()[domain.EventGitHubPROpened]; got != 1 {
		t.Errorf("counter[opened] = %d, want 1 (hook missed direct RecordEvent)", got)
	}
}

// TestLifetimeDistinctCounter_ConcurrentRecord exercises the mutex
// guarding the dedupe set + counter map. With N goroutines each
// calling Record for distinct entities, the final count must be N —
// any race that double-increments or drops an entry would fail this.
func TestLifetimeDistinctCounter_ConcurrentRecord(t *testing.T) {
	counter := NewLifetimeDistinctCounter()

	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			id := string(rune('a' + i%26))
			// vary by `i` not `id` so each goroutine's pair is unique.
			id = id + "_" + string(rune(i))
			counter.Record(domain.Event{
				EventType: domain.EventGitHubPROpened,
				EntityID:  &id,
			})
		}()
	}
	wg.Wait()

	if got := counter.Snapshot()[domain.EventGitHubPROpened]; got != n {
		t.Errorf("concurrent Record: count = %d, want %d", got, n)
	}
}
