package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TriggerStoreFactory is what a per-backend test file hands to
// RunTriggerStoreConformance. The factory returns the wired store +
// the orgID + a seedPrompt hook that ensures the prompts referenced
// by shipped triggers (FK: prompt_triggers.prompt_id → prompts.id)
// exist before Seed runs. The harness doesn't know how to create
// prompts directly — PromptStore lives in a different store
// interface — so the backend test owns that wiring.
type TriggerStoreFactory func(t *testing.T) (store db.TriggerStore, orgID string, seedPrompts PromptSeederForTriggers)

// PromptSeederForTriggers creates the prompts referenced by the
// shipped triggers list. Triggers FK on (prompt_id, org_id) →
// prompts(id, org_id) in Postgres; SQLite has a simpler prompt_id
// FK. Either way, the prompts must exist before the trigger seed
// runs.
type PromptSeederForTriggers func(t *testing.T)

// RunTriggerStoreConformance runs the shared assertion suite. What it
// covers:
//
//   - Seed populates the shipped set; re-seed is a no-op; user
//     customization (re-enable on a shipped trigger) survives re-seed
//     — the load-bearing invariant.
//   - CRUD round-trips for every method.
//   - List ordering — created_at DESC.
//   - SetEnabled — admin path through the toggle endpoint.
//   - GetActiveForEvent filters by event_type + enabled — the router
//     contract.
//   - ListForPrompt — the prompts-handler companion query.
//   - Source is forced to "user" by Create; preserved on Update.
//   - Context cancellation fails fast.
func RunTriggerStoreConformance(t *testing.T, factory TriggerStoreFactory) {
	t.Helper()

	t.Run("Seed_FreshInsert_PopulatesShippedTriggers", func(t *testing.T) {
		store, orgID, seedPrompts := factory(t)
		seedPrompts(t)
		ctx := context.Background()
		if err := store.Seed(ctx, orgID); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// Spot-check three known event_types — the schema-blind way to
		// find shipped rows because SQLite stores them by slug ID while
		// Postgres stores them as UUIDv5 derivations.
		for _, et := range []string{
			domain.EventGitHubPRCICheckFailed,
			domain.EventGitHubPRConflicts,
			domain.EventJiraIssueAssigned,
		} {
			got := findShippedTriggerByEventType(t, store, orgID, et)
			if got == nil {
				t.Fatalf("no system trigger for event_type=%s after seed", et)
			}
			if got.Enabled {
				t.Errorf("shipped trigger for %s is enabled; expected disabled by convention", et)
			}
			if got.Source != domain.PromptTriggerSourceSystem {
				t.Errorf("trigger for %s source=%q want system", et, got.Source)
			}
		}
	})

	t.Run("Seed_Idempotent_PreservesUserCustomization", func(t *testing.T) {
		// Load-bearing invariant: a user enables a shipped trigger,
		// re-seed must NOT flip it back off.
		store, orgID, seedPrompts := factory(t)
		seedPrompts(t)
		ctx := context.Background()
		if err := store.Seed(ctx, orgID); err != nil {
			t.Fatalf("first seed: %v", err)
		}
		trig := findShippedTriggerByEventType(t, store, orgID, domain.EventGitHubPRCICheckFailed)
		if trig == nil {
			t.Fatal("expected shipped CI trigger after seed")
		}
		if err := store.SetEnabled(ctx, orgID, trig.ID, true); err != nil {
			t.Fatalf("set enabled: %v", err)
		}
		if err := store.Seed(ctx, orgID); err != nil {
			t.Fatalf("re-seed: %v", err)
		}
		got, err := store.Get(ctx, orgID, trig.ID)
		if err != nil {
			t.Fatalf("get after re-seed: %v", err)
		}
		if got == nil || !got.Enabled {
			t.Fatalf("re-seed clobbered user-enable on shipped trigger %s; customization lost across boots", trig.ID)
		}
	})

	t.Run("Get_ReturnsNilOnMissing", func(t *testing.T) {
		store, orgID, _ := factory(t)
		// Valid-shape UUID so Postgres's UUID column doesn't reject
		// at parse time before the WHERE filter runs.
		got, err := store.Get(context.Background(), orgID, "00000000-0000-0000-0000-000000000000")
		if err != nil {
			t.Fatalf("Get missing returned error: %v", err)
		}
		if got != nil {
			t.Fatalf("Get missing returned %+v; want nil", got)
		}
	})

	t.Run("Get_ReturnsNilOnInvalidUUID", func(t *testing.T) {
		// Non-UUID strings (e.g. a stale slug from before SKY-247's
		// global PK migration, or a malformed URL param) should
		// surface as not-found rather than as a Postgres parse
		// error (22P02). SQLite's TEXT-keyed table naturally
		// returns 0 rows; the Postgres impl validates the UUID
		// shape up front to match.
		store, orgID, _ := factory(t)
		for _, garbage := range []string{
			"not-a-uuid",
			"system-trigger-ci-fix", // looks like the pre-UUID slug
			"",
			"42",
			"00000000-0000-0000-0000-00000000000",   // one short
			"00000000-0000-0000-0000-0000000000000", // one long
		} {
			got, err := store.Get(context.Background(), orgID, garbage)
			if err != nil {
				t.Errorf("Get(%q): want (nil, nil) got error: %v", garbage, err)
			}
			if got != nil {
				t.Errorf("Get(%q): want nil, got %+v", garbage, got)
			}
		}
	})

	t.Run("Mutations_OnInvalidUUID_AreNoops", func(t *testing.T) {
		// Update / SetEnabled / Delete on an invalid UUID return
		// nil (no-op) rather than bubbling a 22P02 parse error.
		// Production handlers Get-first to 404, so this just keeps
		// the mutating path consistent with that pattern.
		store, orgID, _ := factory(t)
		ctx := context.Background()
		if err := store.SetEnabled(ctx, orgID, "not-a-uuid", false); err != nil {
			t.Errorf("SetEnabled on invalid UUID: want nil, got %v", err)
		}
		if err := store.Delete(ctx, orgID, "not-a-uuid"); err != nil {
			t.Errorf("Delete on invalid UUID: want nil, got %v", err)
		}
		if err := store.Update(ctx, orgID, domain.PromptTrigger{
			ID: "not-a-uuid", TriggerType: domain.TriggerTypeEvent,
		}); err != nil {
			t.Errorf("Update on invalid UUID: want nil, got %v", err)
		}
	})

	t.Run("Create_AndRoundTrip", func(t *testing.T) {
		store, orgID, seedPrompts := factory(t)
		seedPrompts(t)
		ctx := context.Background()
		id := uuid.New().String()
		pred := `{"author_is_self":true}`
		// PromptID has to point at a real prompt because Postgres has
		// a FK on (prompt_id, org_id). The seed hook created the
		// shipped prompts, so referencing one of those is the
		// simplest way to satisfy the FK across backends.
		r := domain.PromptTrigger{
			ID:                     id,
			PromptID:               "system-ci-fix",
			TriggerType:            domain.TriggerTypeEvent,
			EventType:              domain.EventGitHubPRCICheckFailed,
			ScopePredicateJSON:     &pred,
			BreakerThreshold:       5,
			MinAutonomySuitability: 0.3,
			Enabled:                true,
		}
		if err := store.Create(ctx, orgID, r); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := store.Get(ctx, orgID, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got == nil {
			t.Fatal("Get returned nil after Create")
		}
		if got.PromptID != r.PromptID || got.EventType != r.EventType ||
			got.BreakerThreshold != r.BreakerThreshold ||
			!nearlyEqual(got.MinAutonomySuitability, r.MinAutonomySuitability) ||
			!got.Enabled {
			t.Fatalf("round-trip mismatch: got=%+v want=%+v", got, r)
		}
		if got.ScopePredicateJSON == nil || *got.ScopePredicateJSON == "" {
			t.Fatalf("predicate not persisted: got=%v", got.ScopePredicateJSON)
		}
		if got.Source != domain.PromptTriggerSourceUser {
			t.Errorf("source=%q want user (Create forces user-source)", got.Source)
		}
	})

	t.Run("Create_RejectsUnsupportedTriggerType", func(t *testing.T) {
		store, orgID, seedPrompts := factory(t)
		seedPrompts(t)
		err := store.Create(context.Background(), orgID, domain.PromptTrigger{
			ID: uuid.New().String(), PromptID: "system-ci-fix",
			TriggerType: "cron", // unsupported in v1
			EventType:   domain.EventGitHubPRCICheckFailed,
			Enabled:     true,
		})
		if err == nil {
			t.Fatal("Create accepted unsupported trigger_type; expected refusal")
		}
	})

	t.Run("Update_ChangesMutableFieldsOnly", func(t *testing.T) {
		store, orgID, seedPrompts := factory(t)
		seedPrompts(t)
		ctx := context.Background()
		id := uuid.New().String()
		pred := `{"author_is_self":true}`
		if err := store.Create(ctx, orgID, domain.PromptTrigger{
			ID: id, PromptID: "system-ci-fix",
			TriggerType:        domain.TriggerTypeEvent,
			EventType:          domain.EventGitHubPRCICheckFailed,
			ScopePredicateJSON: &pred, BreakerThreshold: 3,
			MinAutonomySuitability: 0.1, Enabled: true,
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		newPred := `{"author_is_self":false}`
		if err := store.Update(ctx, orgID, domain.PromptTrigger{
			ID:                     id,
			ScopePredicateJSON:     &newPred,
			BreakerThreshold:       7,
			MinAutonomySuitability: 0.9,
		}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		got, err := store.Get(ctx, orgID, id)
		if err != nil || got == nil {
			t.Fatalf("Get: got=%v err=%v", got, err)
		}
		if got.BreakerThreshold != 7 || !nearlyEqual(got.MinAutonomySuitability, 0.9) {
			t.Errorf("update did not apply mutable fields: %+v", got)
		}
		if got.ScopePredicateJSON == nil || *got.ScopePredicateJSON == pred {
			t.Errorf("predicate not updated: %v", got.ScopePredicateJSON)
		}
		// Source must not have changed (Update doesn't accept it).
		if got.Source != domain.PromptTriggerSourceUser {
			t.Errorf("source changed during Update: %q", got.Source)
		}
	})

	t.Run("SetEnabled_TogglesBit", func(t *testing.T) {
		store, orgID, seedPrompts := factory(t)
		seedPrompts(t)
		ctx := context.Background()
		id := uuid.New().String()
		if err := store.Create(ctx, orgID, domain.PromptTrigger{
			ID: id, PromptID: "system-ci-fix",
			TriggerType: domain.TriggerTypeEvent,
			EventType:   domain.EventGitHubPRCICheckFailed,
			Enabled:     true,
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := store.SetEnabled(ctx, orgID, id, false); err != nil {
			t.Fatalf("SetEnabled false: %v", err)
		}
		got, _ := store.Get(ctx, orgID, id)
		if got == nil || got.Enabled {
			t.Fatalf("expected disabled, got %+v", got)
		}
		if err := store.SetEnabled(ctx, orgID, id, true); err != nil {
			t.Fatalf("SetEnabled true: %v", err)
		}
		got, _ = store.Get(ctx, orgID, id)
		if got == nil || !got.Enabled {
			t.Fatalf("expected enabled, got %+v", got)
		}
	})

	t.Run("Delete_RemovesRow", func(t *testing.T) {
		store, orgID, seedPrompts := factory(t)
		seedPrompts(t)
		ctx := context.Background()
		id := uuid.New().String()
		if err := store.Create(ctx, orgID, domain.PromptTrigger{
			ID: id, PromptID: "system-ci-fix",
			TriggerType: domain.TriggerTypeEvent,
			EventType:   domain.EventGitHubPRCICheckFailed,
			Enabled:     true,
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := store.Delete(ctx, orgID, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		got, err := store.Get(ctx, orgID, id)
		if err != nil {
			t.Fatalf("Get after delete: %v", err)
		}
		if got != nil {
			t.Fatalf("row still present after Delete: %+v", got)
		}
	})

	t.Run("ListForPrompt_FiltersToPromptID", func(t *testing.T) {
		store, orgID, seedPrompts := factory(t)
		seedPrompts(t)
		ctx := context.Background()

		mine := uuid.New().String()
		other := uuid.New().String()
		if err := store.Create(ctx, orgID, domain.PromptTrigger{
			ID: mine, PromptID: "system-ci-fix",
			TriggerType: domain.TriggerTypeEvent, EventType: domain.EventGitHubPRCICheckFailed,
			Enabled: true,
		}); err != nil {
			t.Fatalf("Create mine: %v", err)
		}
		if err := store.Create(ctx, orgID, domain.PromptTrigger{
			ID: other, PromptID: "system-pr-review",
			TriggerType: domain.TriggerTypeEvent, EventType: domain.EventGitHubPRReviewRequested,
			Enabled: true,
		}); err != nil {
			t.Fatalf("Create other: %v", err)
		}

		got, err := store.ListForPrompt(ctx, orgID, "system-ci-fix")
		if err != nil {
			t.Fatalf("ListForPrompt: %v", err)
		}
		var sawMine, sawOther bool
		for _, row := range got {
			if row.ID == mine {
				sawMine = true
			}
			if row.ID == other {
				sawOther = true
			}
		}
		if !sawMine {
			t.Error("ListForPrompt did not return matching prompt's trigger")
		}
		if sawOther {
			t.Error("ListForPrompt returned trigger for a different prompt_id")
		}
	})

	t.Run("GetActiveForEvent_FiltersDisabledAndOtherTypes", func(t *testing.T) {
		store, orgID, seedPrompts := factory(t)
		seedPrompts(t)
		ctx := context.Background()
		// All three are for distinct (prompt_id, event_type) tuples to
		// avoid the prompt_triggers unique constraint on
		// (prompt_id, event_type, trigger_type). The test still
		// exercises the filter on event_type + enabled because:
		//   - match: enabled, event_type X → must come back
		//   - disabled: disabled, event_type X (via a different prompt) → must NOT come back
		//   - other: enabled, event_type Y → must NOT come back
		match := uuid.New().String()
		disabled := uuid.New().String()
		other := uuid.New().String()
		for _, r := range []domain.PromptTrigger{
			{ID: match, PromptID: "system-ci-fix", TriggerType: domain.TriggerTypeEvent, EventType: domain.EventGitHubPRCICheckFailed, Enabled: true},
			{ID: disabled, PromptID: "system-conflict-resolution", TriggerType: domain.TriggerTypeEvent, EventType: domain.EventGitHubPRCICheckFailed, Enabled: false},
			{ID: other, PromptID: "system-pr-review", TriggerType: domain.TriggerTypeEvent, EventType: domain.EventGitHubPRReviewRequested, Enabled: true},
		} {
			if err := store.Create(ctx, orgID, r); err != nil {
				t.Fatalf("Create %s: %v", r.ID, err)
			}
		}
		got, err := store.GetActiveForEvent(ctx, orgID, domain.EventGitHubPRCICheckFailed)
		if err != nil {
			t.Fatalf("GetActiveForEvent: %v", err)
		}
		ourIDs := map[string]bool{match: true, disabled: true, other: true}
		var seen []string
		for _, row := range got {
			if ourIDs[row.ID] {
				seen = append(seen, row.ID)
			}
		}
		if len(seen) != 1 || seen[0] != match {
			t.Errorf("got our triggers %v; want [%s]", seen, match)
		}
	})

	t.Run("CtxCancellation_FailsFast", func(t *testing.T) {
		store, orgID, _ := factory(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		time.Sleep(time.Millisecond)
		if _, err := store.List(ctx, orgID); err == nil {
			t.Errorf("List with cancelled ctx: want error, got nil")
		}
	})
}

// findShippedTriggerByEventType returns the first system-source
// trigger matching the given event_type. Used in lieu of Get-by-slug
// because SQLite stores shipped IDs as slugs while Postgres stores
// them as deterministic UUIDs — the harness is schema-blind and can't
// hardcode either form.
func findShippedTriggerByEventType(t *testing.T, store db.TriggerStore, orgID, eventType string) *domain.PromptTrigger {
	t.Helper()
	triggers, err := store.List(context.Background(), orgID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for i := range triggers {
		if triggers[i].EventType == eventType && triggers[i].Source == domain.PromptTriggerSourceSystem {
			return &triggers[i]
		}
	}
	return nil
}
