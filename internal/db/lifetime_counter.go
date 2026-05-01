package db

import (
	"database/sql"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// LifetimeDistinctCounter is an in-memory aggregate of how many distinct
// entities have ever produced an event of each event_type. Equivalent
// to DistinctEntityCountsByEventTypeLifetime, but folded incrementally
// off the event bus so the factory snapshot handler doesn't pay a full-
// table scan on every poll.
//
// Lifecycle:
//
//   - Hydrate(db) runs the underlying SQL query once at startup. Cost:
//     O(events) one-time.
//   - Record(evt) is wired to the event bus. Cost: O(1) per event,
//     guarded by a single mutex around the dedupe set + counter map.
//   - Snapshot() returns a map copy. Cost: O(types) — small constant.
//
// The dedupe set holds one entry per (event_type, entity_id) pair ever
// observed. For typical workloads (thousands of PRs × ~12 stations)
// that's well under a megabyte; we don't bother pruning.
type LifetimeDistinctCounter struct {
	mu     sync.RWMutex
	counts map[string]int      // event_type → distinct entity count
	seen   map[string]struct{} // composite key "<event_type>|<entity_id>"
}

// NewLifetimeDistinctCounter returns an empty counter. Call Hydrate
// before serving traffic so the initial snapshot reflects historical
// events; Record handles deltas after that.
func NewLifetimeDistinctCounter() *LifetimeDistinctCounter {
	return &LifetimeDistinctCounter{
		counts: map[string]int{},
		seen:   map[string]struct{}{},
	}
}

// Hydrate populates the counter from the events table. Reads the
// distinct (event_type, entity_id) pairs (rather than the SQL
// aggregate's pre-counted form) so the dedupe set ends up populated
// for subsequent Record calls.
//
// SELECT DISTINCT pushes deduplication into SQLite, so a station with
// many re-entries (e.g. a flaky CI check failing 50 times on one PR)
// returns a single row at the DB level instead of being read 50 times
// and dropped 49 times in Go. The partial index
// `idx_events_type_entity (event_type, entity_id) WHERE entity_id IS
// NOT NULL` covers both SELECT columns and the WHERE filter, so the
// query is an index-only scan with adjacency-based DISTINCT — no temp
// B-tree, no table touch.
//
// Should be called once at startup, before SetOnEventRecorded wires
// the hook — otherwise a brand-new event could land in `seen` first
// and then get re-counted by the hydrating scan.
func (c *LifetimeDistinctCounter) Hydrate(database *sql.DB) error {
	rows, err := database.Query(`
		SELECT DISTINCT event_type, entity_id
		FROM events
		WHERE entity_id IS NOT NULL
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	c.mu.Lock()
	defer c.mu.Unlock()
	for rows.Next() {
		var eventType, entityID string
		if err := rows.Scan(&eventType, &entityID); err != nil {
			return err
		}
		// SELECT DISTINCT guarantees no duplicates, but the dedupe set
		// still needs every pair populated so post-hydrate Record calls
		// can recognize re-entries.
		c.seen[eventType+"|"+entityID] = struct{}{}
		c.counts[eventType]++
	}
	return rows.Err()
}

// Record observes an event from the bus. No-op if the event has no
// entity_id (system events) or if (event_type, entity_id) was already
// counted. Safe for concurrent use.
func (c *LifetimeDistinctCounter) Record(evt domain.Event) {
	if evt.EntityID == nil || *evt.EntityID == "" {
		return
	}
	key := evt.EventType + "|" + *evt.EntityID
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.seen[key]; dup {
		return
	}
	c.seen[key] = struct{}{}
	c.counts[evt.EventType]++
}

// Snapshot returns a copy of the current per-event-type counts. The
// copy isolates callers from concurrent mutation by Record.
func (c *LifetimeDistinctCounter) Snapshot() map[string]int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]int, len(c.counts))
	for k, v := range c.counts {
		out[k] = v
	}
	return out
}
