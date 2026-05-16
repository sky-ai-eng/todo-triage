package dbtest

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// jsonEqual compares two JSON strings for semantic equality.
// Postgres stores metadata_json as JSONB and re-serializes it with
// canonical whitespace on read (`{"k": "v"}` instead of the
// caller-supplied `{"k":"v"}`); SQLite returns the bytes verbatim.
// The store contract is "metadata_json round-trips as JSON," not
// "byte-identical." Use this for every metadata assertion in the
// conformance suite so both backends pass the same checks.
func jsonEqual(t *testing.T, got, want string) bool {
	t.Helper()
	if got == want {
		return true
	}
	var gotV, wantV any
	if err := json.Unmarshal([]byte(got), &gotV); err != nil {
		t.Errorf("json.Unmarshal got=%q: %v", got, err)
		return false
	}
	if err := json.Unmarshal([]byte(want), &wantV); err != nil {
		t.Errorf("json.Unmarshal want=%q: %v", want, err)
		return false
	}
	return reflect.DeepEqual(gotV, wantV)
}

// EventStoreFactory is what a per-backend test file hands to
// RunEventStoreConformance. Returns:
//   - the wired EventStore impl,
//   - the orgID to pass to every call,
//   - an EventStoreSeeder the harness uses to drop entity rows
//     (events FK to entities and the backends seed the entity graph
//     differently — SQLite is open, Postgres needs the full org +
//     auth user FK chain).
type EventStoreFactory func(t *testing.T) (store db.EventStore, orgID string, seed EventStoreSeeder)

// EventStoreSeeder is a bag of callbacks the conformance suite uses
// to stage fixture rows the EventStore doesn't own. Each backend
// implements them against its own SQL.
type EventStoreSeeder struct {
	// Entity inserts an active GitHub PR entity and returns its ID.
	// suffix is appended to source_id so multiple seeds per subtest
	// don't collide on the (source, source_id) unique index.
	Entity func(t *testing.T, suffix string) string
}

// RunEventStoreConformance covers the EventStore contract every
// backend impl must hold:
//
//   - Record + read-back round-trips: caller-supplied IDs stay,
//     generated IDs are non-empty, OccurredAt and EntityID flow
//     through.
//   - Record fires the SetOnEventRecorded hook on success.
//   - RecordSystem fires the hook too (admin-pool path).
//   - LatestForEntityTypeAndDedupKey returns the most recent
//     matching row and discriminates correctly on dedup_key.
//   - LatestForEntityTypeAndDedupKey returns (nil, nil) on miss.
//   - GetMetadataSystem returns "" on missing event (NULL/no-row).
//   - GetMetadataSystem round-trips a real metadata string.
func RunEventStoreConformance(t *testing.T, mk EventStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Record_then_Latest_round_trips_supplied_ID", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "record-roundtrip")
		eid := entityID
		evt := domain.Event{
			ID:           "11111111-1111-1111-1111-111111111111",
			EntityID:     &eid,
			EventType:    domain.EventGitHubPRCICheckPassed,
			MetadataJSON: `{"check_name":"build"}`,
		}
		got, err := s.Record(ctx, orgID, evt)
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
		if got != evt.ID {
			t.Errorf("Record returned id=%q, want caller-supplied %q", got, evt.ID)
		}
		latest, err := s.LatestForEntityTypeAndDedupKey(ctx, orgID, entityID, domain.EventGitHubPRCICheckPassed, "")
		if err != nil || latest == nil {
			t.Fatalf("Latest: got=%v err=%v", latest, err)
		}
		if latest.ID != evt.ID {
			t.Errorf("Latest.ID = %q, want %q", latest.ID, evt.ID)
		}
		if !jsonEqual(t, latest.MetadataJSON, evt.MetadataJSON) {
			t.Errorf("Latest.MetadataJSON = %q, want JSON-equivalent to %q", latest.MetadataJSON, evt.MetadataJSON)
		}
		if latest.EntityID == nil || *latest.EntityID != entityID {
			t.Errorf("Latest.EntityID = %v, want %q", latest.EntityID, entityID)
		}
	})

	t.Run("Record_generates_UUID_when_ID_empty", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "record-gen-id")
		eid := entityID
		got, err := s.Record(ctx, orgID, domain.Event{
			EntityID:  &eid,
			EventType: domain.EventGitHubPROpened,
		})
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
		if got == "" {
			t.Error("Record returned empty id; want generated UUID")
		}
	})

	t.Run("Record_with_zero_OccurredAt_persists_null", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "record-zero-occurred")
		eid := entityID
		if _, err := s.Record(ctx, orgID, domain.Event{
			EntityID:  &eid,
			EventType: domain.EventGitHubPROpened,
			// OccurredAt left zero
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
		latest, err := s.LatestForEntityTypeAndDedupKey(ctx, orgID, entityID, domain.EventGitHubPROpened, "")
		if err != nil || latest == nil {
			t.Fatalf("Latest: got=%v err=%v", latest, err)
		}
		if !latest.OccurredAt.IsZero() {
			t.Errorf("OccurredAt = %v, want zero (NULL column)", latest.OccurredAt)
		}
	})

	t.Run("Record_with_OccurredAt_round_trips", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "record-with-occurred")
		eid := entityID
		when := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Hour)
		if _, err := s.Record(ctx, orgID, domain.Event{
			EntityID:   &eid,
			EventType:  domain.EventGitHubPROpened,
			OccurredAt: when,
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
		latest, err := s.LatestForEntityTypeAndDedupKey(ctx, orgID, entityID, domain.EventGitHubPROpened, "")
		if err != nil || latest == nil {
			t.Fatalf("Latest: got=%v err=%v", latest, err)
		}
		if !latest.OccurredAt.Equal(when) {
			t.Errorf("OccurredAt = %v, want %v", latest.OccurredAt, when)
		}
	})

	t.Run("Record_fires_SetOnEventRecorded_hook", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "record-fires-hook")
		eid := entityID

		var mu sync.Mutex
		var observed []domain.Event
		db.SetOnEventRecorded(func(evt domain.Event) {
			mu.Lock()
			defer mu.Unlock()
			observed = append(observed, evt)
		})
		t.Cleanup(func() { db.SetOnEventRecorded(nil) })

		got, err := s.Record(ctx, orgID, domain.Event{
			EntityID:  &eid,
			EventType: domain.EventGitHubPROpened,
		})
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(observed) != 1 {
			t.Fatalf("hook fired %d times, want 1", len(observed))
		}
		if observed[0].ID != got {
			t.Errorf("hook saw id=%q, Record returned id=%q", observed[0].ID, got)
		}
		if observed[0].EntityID == nil || *observed[0].EntityID != entityID {
			t.Errorf("hook saw EntityID=%v, want %q", observed[0].EntityID, entityID)
		}
	})

	t.Run("RecordSystem_fires_SetOnEventRecorded_hook", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "record-system-fires-hook")
		eid := entityID

		var mu sync.Mutex
		var observed []domain.Event
		db.SetOnEventRecorded(func(evt domain.Event) {
			mu.Lock()
			defer mu.Unlock()
			observed = append(observed, evt)
		})
		t.Cleanup(func() { db.SetOnEventRecorded(nil) })

		got, err := s.RecordSystem(ctx, orgID, domain.Event{
			EntityID:  &eid,
			EventType: domain.EventGitHubPROpened,
		})
		if err != nil {
			t.Fatalf("RecordSystem: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(observed) != 1 {
			t.Fatalf("hook fired %d times, want 1", len(observed))
		}
		if observed[0].ID != got {
			t.Errorf("hook saw id=%q, RecordSystem returned id=%q", observed[0].ID, got)
		}
	})

	t.Run("Latest_returns_most_recent_match", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "latest-most-recent")
		eid := entityID

		// Older matching event.
		olderID, err := s.Record(ctx, orgID, domain.Event{
			EntityID: &eid, EventType: domain.EventGitHubPRCICheckPassed,
		})
		if err != nil {
			t.Fatalf("record older: %v", err)
		}
		// Different type — must not be returned.
		if _, err := s.Record(ctx, orgID, domain.Event{
			EntityID: &eid, EventType: domain.EventGitHubPRReviewApproved,
		}); err != nil {
			t.Fatalf("record other-type: %v", err)
		}
		// Sleep so created_at advances past 1s resolution backends.
		time.Sleep(20 * time.Millisecond)
		latestID, err := s.Record(ctx, orgID, domain.Event{
			EntityID: &eid, EventType: domain.EventGitHubPRCICheckPassed,
		})
		if err != nil {
			t.Fatalf("record latest: %v", err)
		}

		got, err := s.LatestForEntityTypeAndDedupKey(ctx, orgID, entityID, domain.EventGitHubPRCICheckPassed, "")
		if err != nil || got == nil {
			t.Fatalf("Latest: got=%v err=%v", got, err)
		}
		if got.ID != latestID {
			t.Errorf("Latest.ID = %q, want %q (older was %q)", got.ID, latestID, olderID)
		}
	})

	t.Run("Latest_discriminates_on_dedup_key", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "latest-dedup-key")
		eid := entityID

		bugID, err := s.Record(ctx, orgID, domain.Event{
			EntityID: &eid, EventType: domain.EventGitHubPRLabelAdded, DedupKey: "bug",
		})
		if err != nil {
			t.Fatalf("record bug: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
		// More recent event on a sibling discriminator — must NOT
		// be returned when filtering by dedup_key="bug".
		if _, err := s.Record(ctx, orgID, domain.Event{
			EntityID: &eid, EventType: domain.EventGitHubPRLabelAdded, DedupKey: "help wanted",
		}); err != nil {
			t.Fatalf("record help-wanted: %v", err)
		}

		got, err := s.LatestForEntityTypeAndDedupKey(ctx, orgID, entityID, domain.EventGitHubPRLabelAdded, "bug")
		if err != nil || got == nil {
			t.Fatalf("Latest: got=%v err=%v", got, err)
		}
		if got.ID != bugID {
			t.Errorf("Latest.ID = %q, want %q (bug-key match)", got.ID, bugID)
		}
	})

	t.Run("Latest_returns_nil_on_miss", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "latest-miss")
		eid := entityID
		if _, err := s.Record(ctx, orgID, domain.Event{
			EntityID: &eid, EventType: domain.EventGitHubPROpened,
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
		got, err := s.LatestForEntityTypeAndDedupKey(ctx, orgID, entityID, domain.EventGitHubPRMerged, "")
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if got != nil {
			t.Errorf("Latest on miss should be nil, got %+v", got)
		}
	})

	t.Run("GetMetadataSystem_returns_metadata", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "get-metadata")
		eid := entityID
		const want = `{"check_name":"build"}`
		eventID, err := s.Record(ctx, orgID, domain.Event{
			EntityID:     &eid,
			EventType:    domain.EventGitHubPRCICheckFailed,
			MetadataJSON: want,
		})
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
		got, err := s.GetMetadataSystem(ctx, orgID, eventID)
		if err != nil {
			t.Fatalf("GetMetadataSystem: %v", err)
		}
		if !jsonEqual(t, got, want) {
			t.Errorf("GetMetadataSystem = %q, want JSON-equivalent to %q", got, want)
		}
	})

	t.Run("GetMetadataSystem_returns_empty_on_miss", func(t *testing.T) {
		s, orgID, _ := mk(t)
		got, err := s.GetMetadataSystem(ctx, orgID, "00000000-0000-0000-0000-000000000000")
		if err != nil {
			t.Fatalf("GetMetadataSystem: %v", err)
		}
		if got != "" {
			t.Errorf("GetMetadataSystem on miss should be empty, got %q", got)
		}
	})

	t.Run("GetMetadataSystem_empty_on_null_column", func(t *testing.T) {
		// Record an event with empty MetadataJSON — both impls treat
		// "" as a NULL column. GetMetadataSystem returns "" for both
		// "no row" and "NULL metadata" (caller can't distinguish, and
		// the caller's contract is "no metadata to match against").
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "get-metadata-null")
		eid := entityID
		eventID, err := s.Record(ctx, orgID, domain.Event{
			EntityID:  &eid,
			EventType: domain.EventGitHubPROpened,
			// MetadataJSON left empty
		})
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
		got, err := s.GetMetadataSystem(ctx, orgID, eventID)
		if err != nil {
			t.Fatalf("GetMetadataSystem: %v", err)
		}
		if got != "" {
			t.Errorf("GetMetadataSystem on NULL metadata should be empty, got %q", got)
		}
	})
}
