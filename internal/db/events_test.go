package db

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestLatestEventForEntityTypeAndDedupKey_ReturnsMostRecentMatch pins
// the drag-to-delegate handler's anchor: synthesized tasks need a real
// event row to set primary_event_id, and the most recent matching
// event is the right choice (older events for the same key may have
// already been resolved). dedup_key="" matches non-discriminator
// events (the common case).
func TestLatestEventForEntityTypeAndDedupKey_ReturnsMostRecentMatch(t *testing.T) {
	database := newTestDB(t)
	a := makeEntity(t, database, 1)
	b := makeEntity(t, database, 2)

	// Older matching event for entity A.
	olderID := recordEvent(t, database, a.ID, domain.EventGitHubPRCICheckPassed)
	// Different type for entity A — must not be returned.
	recordEvent(t, database, a.ID, domain.EventGitHubPRReviewApproved)
	// Latest matching event for entity A.
	latestID := recordEvent(t, database, a.ID, domain.EventGitHubPRCICheckPassed)
	// Same type for entity B — must not be returned.
	recordEvent(t, database, b.ID, domain.EventGitHubPRCICheckPassed)

	got, err := LatestEventForEntityTypeAndDedupKey(database, a.ID, domain.EventGitHubPRCICheckPassed, "")
	if err != nil {
		t.Fatalf("LatestEventForEntityTypeAndDedupKey: %v", err)
	}
	if got == nil {
		t.Fatal("got nil, want event")
	}
	if got.ID != latestID {
		t.Errorf("ID = %s, want %s (latest match), older match was %s", got.ID, latestID, olderID)
	}
}

// TestLatestEventForEntityTypeAndDedupKey_NoMatchReturnsNil —
// defensive: the handler refuses to synthesize a task when the
// entity has never had an event of that type. Confirm nil rather
// than an error.
func TestLatestEventForEntityTypeAndDedupKey_NoMatchReturnsNil(t *testing.T) {
	database := newTestDB(t)
	a := makeEntity(t, database, 1)
	recordEvent(t, database, a.ID, domain.EventGitHubPROpened)

	got, err := LatestEventForEntityTypeAndDedupKey(database, a.ID, domain.EventGitHubPRMerged, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil (no matching event)", got)
	}
}

// TestLatestEventForEntityTypeAndDedupKey_DistinguishesDedupKey is the
// regression for the "label_added bug then label_added help-wanted"
// case: filtering on event_type alone and rejecting a dedup_key
// mismatch after the fact would 400 the dragged "bug" chip whenever
// "help wanted" fired more recently. Pushing dedup_key into the WHERE
// clause picks the right anchor regardless of sibling order.
func TestLatestEventForEntityTypeAndDedupKey_DistinguishesDedupKey(t *testing.T) {
	database := newTestDB(t)
	a := makeEntity(t, database, 1)

	bugID, err := RecordEvent(database, domain.Event{
		EntityID: &a.ID, EventType: domain.EventGitHubPRLabelAdded, DedupKey: "bug",
	})
	if err != nil {
		t.Fatalf("record bug event: %v", err)
	}
	if _, err := RecordEvent(database, domain.Event{
		EntityID: &a.ID, EventType: domain.EventGitHubPRLabelAdded, DedupKey: "help wanted",
	}); err != nil {
		t.Fatalf("record help-wanted event: %v", err)
	}

	got, err := LatestEventForEntityTypeAndDedupKey(database, a.ID, domain.EventGitHubPRLabelAdded, "bug")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got == nil || got.ID != bugID {
		t.Errorf("got %v, want event %s (bug match) — dedup_key filter must not return the more-recent help-wanted event", got, bugID)
	}
}
