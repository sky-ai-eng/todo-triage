package dbtest

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// priorityTolerance accommodates the Postgres `default_priority REAL`
// column's single-precision rounding (0.7 stored as 0.69999998...).
// SQLite's REAL is 8-byte and exact, but the test runs against both
// backends — fail only when the gap exceeds REAL precision, not on
// the bit-exact mismatch the Postgres round-trip surfaces.
const priorityTolerance = 1e-5

func nearlyEqual(a, b float64) bool {
	return math.Abs(a-b) <= priorityTolerance
}

// findShippedRuleByEventType returns the first system-source rule
// matching the given event_type. Used in lieu of Get-by-slug because
// SQLite stores shipped IDs as slugs while Postgres stores them as
// deterministic UUIDs — the conformance harness is schema-blind and
// can't hardcode either form.
func findShippedRuleByEventType(t *testing.T, store db.TaskRuleStore, orgID, eventType string) *domain.TaskRule {
	t.Helper()
	rules, err := store.List(context.Background(), orgID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for i := range rules {
		if rules[i].EventType == eventType && rules[i].Source == domain.TaskRuleSourceSystem {
			return &rules[i]
		}
	}
	return nil
}

// TaskRuleStoreFactory is what a per-backend test file hands to
// RunTaskRuleStoreConformance. The factory returns the wired store +
// the orgID to pass to every method. TaskRuleStore owns its own table
// — no seeder hook is needed because every assertion either uses the
// shipped Seed() set or creates fresh rules via Create.
type TaskRuleStoreFactory func(t *testing.T) (store db.TaskRuleStore, orgID string)

// RunTaskRuleStoreConformance runs the shared assertion suite against
// any db.TaskRuleStore impl. What this covers (and why):
//
//   - Seed populates the shipped set; re-seed is a no-op; a user's
//     disable on a system rule survives re-seed (the load-bearing
//     invariant that makes "disabled by user" durable across boots).
//   - CRUD round-trips — minimal "the SQL parses + behaves" checks
//     for every method.
//   - List ordering — sort_order ASC then name ASC, as the UI relies on.
//   - Reorder applies the new indices in order; rules absent from the
//     ID list keep their current sort_order.
//   - GetEnabledForEvent filters by event_type AND enabled — the
//     router contract that decides whether a card gets created.
//   - Context cancellation fails fast.
func RunTaskRuleStoreConformance(t *testing.T, factory TaskRuleStoreFactory) {
	t.Helper()

	t.Run("Seed_FreshInsert_PopulatesShippedRules", func(t *testing.T) {
		store, orgID := factory(t)
		ctx := context.Background()
		if err := store.Seed(ctx, orgID); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// Spot-check three known system event_types to prove the seed
		// wrote real rows. Look up by event_type+source rather than
		// by hardcoded ID — SQLite keeps the slug as the row id while
		// Postgres uses a UUID derivation, and the harness is
		// schema-blind.
		for _, et := range []string{
			domain.EventGitHubPRCICheckFailed,
			domain.EventGitHubPRReviewChangesRequested,
			domain.EventJiraIssueAssigned,
		} {
			got := findShippedRuleByEventType(t, store, orgID, et)
			if got == nil {
				t.Fatalf("no system rule for event_type=%s after seed", et)
			}
			if !got.Enabled {
				t.Errorf("rule for %s starts disabled; shipped rules should be enabled", et)
			}
		}
	})

	t.Run("Seed_Idempotent_DoesNotOverwriteUserDisable", func(t *testing.T) {
		// The load-bearing invariant: if a user disables a shipped
		// rule, the next boot's Seed must NOT flip it back on.
		// SQLite uses INSERT OR IGNORE; Postgres uses ON CONFLICT DO
		// NOTHING. Both must pass this check.
		store, orgID := factory(t)
		ctx := context.Background()
		if err := store.Seed(ctx, orgID); err != nil {
			t.Fatalf("first seed: %v", err)
		}
		rule := findShippedRuleByEventType(t, store, orgID, domain.EventGitHubPRCICheckFailed)
		if rule == nil {
			t.Fatal("expected shipped CI rule after seed")
		}
		if err := store.SetEnabled(ctx, orgID, rule.ID, false); err != nil {
			t.Fatalf("set disabled: %v", err)
		}
		if err := store.Seed(ctx, orgID); err != nil {
			t.Fatalf("re-seed: %v", err)
		}
		got, err := store.Get(ctx, orgID, rule.ID)
		if err != nil {
			t.Fatalf("get after re-seed: %v", err)
		}
		if got == nil || got.Enabled {
			t.Fatalf("re-seed resurrected user-disabled rule %s; user customization lost across boots", rule.ID)
		}
	})

	t.Run("Get_ReturnsNilOnMissing", func(t *testing.T) {
		store, orgID := factory(t)
		// Use a syntactically-valid UUID — Postgres's UUID column
		// would reject "does-not-exist" at the parse layer before
		// the row-filter ever ran, and we want to assert the
		// not-found contract not the parse error.
		got, err := store.Get(context.Background(), orgID, "00000000-0000-0000-0000-000000000000")
		if err != nil {
			t.Fatalf("Get missing returned error: %v", err)
		}
		if got != nil {
			t.Fatalf("Get missing returned %+v; want nil", got)
		}
	})

	t.Run("Create_AndRoundTrip", func(t *testing.T) {
		store, orgID := factory(t)
		ctx := context.Background()
		id := uuid.New().String()
		pred := `{"author_is_self":true}`
		r := domain.TaskRule{
			ID:                 id,
			EventType:          domain.EventGitHubPRCICheckFailed,
			ScopePredicateJSON: &pred,
			Enabled:            true,
			Name:               "Test rule",
			DefaultPriority:    0.7,
			SortOrder:          42,
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
		if got.Name != r.Name || got.EventType != r.EventType ||
			!nearlyEqual(got.DefaultPriority, r.DefaultPriority) || got.SortOrder != r.SortOrder ||
			!got.Enabled {
			t.Fatalf("round-trip mismatch: got=%+v want=%+v", got, r)
		}
		if got.ScopePredicateJSON == nil || *got.ScopePredicateJSON == "" {
			t.Fatalf("predicate not persisted: got=%v", got.ScopePredicateJSON)
		}
		if got.Source != domain.TaskRuleSourceUser {
			t.Errorf("source=%q want user (Create forces user-source)", got.Source)
		}
	})

	t.Run("Create_MatchAllPredicate_StoresAsNull", func(t *testing.T) {
		// Match-all rules (no predicate) must round-trip as nil so
		// the router's predicate-evaluator hits the empty-string
		// branch instead of trying to parse "". This is the
		// behavior the seed's review-requested rule depends on.
		store, orgID := factory(t)
		ctx := context.Background()
		id := uuid.New().String()
		r := domain.TaskRule{
			ID:                 id,
			EventType:          domain.EventGitHubPRReviewRequested,
			ScopePredicateJSON: nil,
			Enabled:            true,
			Name:               "Match-all rule",
			DefaultPriority:    0.5,
		}
		if err := store.Create(ctx, orgID, r); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := store.Get(ctx, orgID, id)
		if err != nil || got == nil {
			t.Fatalf("Get: got=%v err=%v", got, err)
		}
		if got.ScopePredicateJSON != nil {
			t.Errorf("ScopePredicateJSON=%q want nil for match-all rule", *got.ScopePredicateJSON)
		}
	})

	t.Run("Update_ChangesMutableFields", func(t *testing.T) {
		store, orgID := factory(t)
		ctx := context.Background()
		id := uuid.New().String()
		pred := `{"author_is_self":true}`
		if err := store.Create(ctx, orgID, domain.TaskRule{
			ID: id, EventType: domain.EventGitHubPRCICheckFailed,
			ScopePredicateJSON: &pred, Enabled: true,
			Name: "Original", DefaultPriority: 0.5, SortOrder: 0,
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		newPred := `{"author_is_self":false}`
		if err := store.Update(ctx, orgID, domain.TaskRule{
			ID: id, EventType: domain.EventGitHubPRCICheckFailed,
			ScopePredicateJSON: &newPred, Enabled: false,
			Name: "Renamed", DefaultPriority: 0.9, SortOrder: 7,
		}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		got, err := store.Get(ctx, orgID, id)
		if err != nil || got == nil {
			t.Fatalf("Get: got=%v err=%v", got, err)
		}
		if got.Name != "Renamed" || got.Enabled || !nearlyEqual(got.DefaultPriority, 0.9) || got.SortOrder != 7 {
			t.Errorf("update did not apply: %+v", got)
		}
		if got.ScopePredicateJSON == nil || *got.ScopePredicateJSON == pred {
			t.Errorf("predicate not updated: %v", got.ScopePredicateJSON)
		}
	})

	t.Run("SetEnabled_TogglesBit", func(t *testing.T) {
		store, orgID := factory(t)
		ctx := context.Background()
		id := uuid.New().String()
		if err := store.Create(ctx, orgID, domain.TaskRule{
			ID: id, EventType: domain.EventGitHubPRCICheckFailed, Enabled: true, Name: "x", DefaultPriority: 0.5,
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
		store, orgID := factory(t)
		ctx := context.Background()
		id := uuid.New().String()
		if err := store.Create(ctx, orgID, domain.TaskRule{
			ID: id, EventType: domain.EventGitHubPRCICheckFailed, Enabled: true, Name: "x", DefaultPriority: 0.5,
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

	t.Run("List_OrdersBySortOrderThenName", func(t *testing.T) {
		// Build rules with deliberately interleaved sort_order so the
		// query's ORDER BY can't accidentally pass by relying on
		// insertion order.
		store, orgID := factory(t)
		ctx := context.Background()
		want := []struct {
			id, name string
			sort     int
		}{
			{uuid.New().String(), "Alpha", 1},
			{uuid.New().String(), "Beta", 0},
			{uuid.New().String(), "Gamma", 1}, // tie on sort_order → name ASC breaks it
		}
		for _, w := range want {
			if err := store.Create(ctx, orgID, domain.TaskRule{
				ID: w.id, EventType: domain.EventGitHubPRCICheckFailed, Enabled: true,
				Name: w.name, DefaultPriority: 0.5, SortOrder: w.sort,
			}); err != nil {
				t.Fatalf("Create %s: %v", w.name, err)
			}
		}
		got, err := store.List(ctx, orgID)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		// Expected order: Beta (sort=0), Alpha (sort=1, A<G), Gamma (sort=1).
		// Filter to just our test rules in case the harness factory
		// pre-seeded shipped rules into this orgID.
		var names []string
		ours := map[string]bool{want[0].id: true, want[1].id: true, want[2].id: true}
		for _, r := range got {
			if ours[r.ID] {
				names = append(names, r.Name)
			}
		}
		wantOrder := []string{"Beta", "Alpha", "Gamma"}
		if len(names) != len(wantOrder) {
			t.Fatalf("List returned %d of our rules; want %d (got names=%v)", len(names), len(wantOrder), names)
		}
		for i := range names {
			if names[i] != wantOrder[i] {
				t.Errorf("order[%d]=%q want %q (full=%v)", i, names[i], wantOrder[i], names)
			}
		}
	})

	t.Run("Reorder_AppliesNewIndices", func(t *testing.T) {
		store, orgID := factory(t)
		ctx := context.Background()
		var ids []string
		for i := 0; i < 3; i++ {
			id := uuid.New().String()
			ids = append(ids, id)
			if err := store.Create(ctx, orgID, domain.TaskRule{
				ID: id, EventType: domain.EventGitHubPRCICheckFailed, Enabled: true,
				Name: id, DefaultPriority: 0.5, SortOrder: i,
			}); err != nil {
				t.Fatalf("Create: %v", err)
			}
		}
		// Reverse the order.
		newOrder := []string{ids[2], ids[1], ids[0]}
		if err := store.Reorder(ctx, orgID, newOrder); err != nil {
			t.Fatalf("Reorder: %v", err)
		}
		for i, id := range newOrder {
			got, _ := store.Get(ctx, orgID, id)
			if got == nil {
				t.Fatalf("missing %s after reorder", id)
			}
			if got.SortOrder != i {
				t.Errorf("%s SortOrder=%d want %d", id, got.SortOrder, i)
			}
		}
	})

	t.Run("GetEnabledForEvent_FiltersDisabledAndOtherTypes", func(t *testing.T) {
		store, orgID := factory(t)
		ctx := context.Background()
		// Three rules: same event-type enabled, same event-type
		// disabled, different event-type enabled. Only the first
		// should come back.
		match := uuid.New().String()
		disabled := uuid.New().String()
		other := uuid.New().String()
		for _, r := range []domain.TaskRule{
			{ID: match, EventType: domain.EventGitHubPRCICheckFailed, Enabled: true, Name: "match", DefaultPriority: 0.5},
			{ID: disabled, EventType: domain.EventGitHubPRCICheckFailed, Enabled: false, Name: "disabled", DefaultPriority: 0.5},
			{ID: other, EventType: domain.EventGitHubPRReviewRequested, Enabled: true, Name: "other-type", DefaultPriority: 0.5},
		} {
			if err := store.Create(ctx, orgID, r); err != nil {
				t.Fatalf("Create %s: %v", r.Name, err)
			}
		}
		got, err := store.GetEnabledForEvent(ctx, orgID, domain.EventGitHubPRCICheckFailed)
		if err != nil {
			t.Fatalf("GetEnabledForEvent: %v", err)
		}
		// Filter to our rules to keep the test robust against
		// shipped-rule pre-seeding.
		ourIDs := map[string]bool{match: true, disabled: true, other: true}
		var seen []string
		for _, r := range got {
			if ourIDs[r.ID] {
				seen = append(seen, r.ID)
			}
		}
		if len(seen) != 1 || seen[0] != match {
			t.Errorf("got our rules %v; want [%s]", seen, match)
		}
	})

	t.Run("CtxCancellation_FailsFast", func(t *testing.T) {
		store, orgID := factory(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		time.Sleep(time.Millisecond)
		if _, err := store.List(ctx, orgID); err == nil {
			t.Errorf("List with cancelled ctx: want error, got nil")
		}
	})
}
