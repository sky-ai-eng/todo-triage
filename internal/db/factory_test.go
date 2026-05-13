package db

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestListFactoryActiveRuns_MemoryMissingDerivedFromJoin pins the
// SKY-204 contract that the factory's in-flight overlay reads
// `memory_missing` as a derivation over run_memory rather than off
// the (now-removed) runs.memory_missing column. Four cases, all
// must end up with the same noncompliance signal where applicable
// despite differing on-disk shape:
//
//  1. No run_memory row at all → memory_missing=true (gate not
//     reached yet).
//  2. Row exists, agent_content NULL → memory_missing=true (the
//     terminate-then-no-write path UpsertAgentMemory writes for
//     gate-failed runs).
//  3. Row exists, agent_content = "" → memory_missing=true (legacy
//     carry-over from before SKY-204 normalized empty to NULL, or a
//     direct INSERT that bypassed UpsertAgentMemory).
//  4. Row exists, agent_content = real text → memory_missing=false.
//
// The (3) row is what motivated the NULLIF(TRIM(...), ”) derivation
// in the SELECT; without that guard the overlay would silently
// regress to "memory present" for any legacy-empty row.
func TestListFactoryActiveRuns_MemoryMissingDerivedFromJoin(t *testing.T) {
	database := newTestDB(t)
	entity := makeEntity(t, database, 200)
	eventID := recordEvent(t, database, entity.ID, domain.EventGitHubPROpened)
	if _, err := database.Exec(
		`INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES ('p_factory', 'Test', 'body', '00000000-0000-0000-0000-000000000100', '00000000-0000-0000-0000-000000000010')`,
	); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		VALUES ('t_factory', ?, ?, ?, 'queued')
	`, entity.ID, domain.EventGitHubPROpened, eventID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	for _, runID := range []string{"run_no_row", "run_null_agent", "run_empty_agent", "run_with_content"} {
		if _, err := database.Exec(`
			INSERT INTO runs (id, task_id, prompt_id, status, trigger_type)
			VALUES (?, 't_factory', 'p_factory', 'running', 'manual')
		`, runID); err != nil {
			t.Fatalf("seed run %s: %v", runID, err)
		}
	}
	if err := UpsertAgentMemory(database, "run_null_agent", entity.ID, ""); err != nil {
		t.Fatalf("upsert null memory: %v", err)
	}
	// Direct INSERT (bypassing UpsertAgentMemory) so the row carries
	// agent_content = "" rather than NULL — simulates a legacy row or
	// a future writer that doesn't go through the helper.
	if _, err := database.Exec(`
		INSERT INTO run_memory (id, run_id, entity_id, agent_content)
		VALUES ('m_empty', 'run_empty_agent', ?, '')
	`, entity.ID); err != nil {
		t.Fatalf("seed empty agent_content: %v", err)
	}
	if err := UpsertAgentMemory(database, "run_with_content", entity.ID, "agent reasoning"); err != nil {
		t.Fatalf("upsert real memory: %v", err)
	}

	runs, err := ListFactoryActiveRuns(database)
	if err != nil {
		t.Fatalf("ListFactoryActiveRuns: %v", err)
	}
	got := map[string]bool{}
	for _, fr := range runs {
		got[fr.Run.ID] = fr.Run.MemoryMissing
	}
	want := map[string]bool{
		"run_no_row":       true,
		"run_null_agent":   true,
		"run_empty_agent":  true,
		"run_with_content": false,
	}
	for id, expected := range want {
		if got[id] != expected {
			t.Errorf("run %s: memory_missing = %v, want %v", id, got[id], expected)
		}
	}
}

// recordEvent inserts a real entity-attached event for tests. Returns the
// event's UUID. Wraps RecordEvent to centralize the t.Fatalf on errors.
func recordEvent(t *testing.T, database *sql.DB, entityID, eventType string) string {
	t.Helper()
	id, err := RecordEvent(database, domain.Event{EntityID: &entityID, EventType: eventType})
	if err != nil {
		t.Fatalf("RecordEvent(%s, %s): %v", entityID, eventType, err)
	}
	return id
}

// makeEntity inserts a fresh active GitHub PR entity for tests. The
// (source, source_id) pair must be unique per test run; the i argument
// gives a stable per-test-row discriminator.
func makeEntity(t *testing.T, database *sql.DB, i int) *domain.Entity {
	t.Helper()
	e, _, err := FindOrCreateEntity(
		database, "github", fmt.Sprintf("owner/repo#%d", i), "pr",
		fmt.Sprintf("PR %d", i), fmt.Sprintf("https://github.com/owner/repo/pull/%d", i),
	)
	if err != nil {
		t.Fatalf("FindOrCreateEntity %d: %v", i, err)
	}
	return e
}

// closeEntityAt sets state='closed' and closed_at to the supplied moment,
// bypassing CloseEntity so tests can backdate the closure to land inside
// or outside the FactoryClosedGracePeriod window.
func closeEntityAt(t *testing.T, database *sql.DB, entityID string, at time.Time) {
	t.Helper()
	if _, err := database.Exec(
		`UPDATE entities SET state = 'closed', closed_at = ? WHERE id = ?`, at, entityID,
	); err != nil {
		t.Fatalf("close entity %s at %v: %v", entityID, at, err)
	}
}

// TestParseDBDatetime locks in the format coverage parseDBDatetime needs to
// handle without raising errors. The factory snapshot endpoint surfaces any
// parse failure as an HTTP 500, so a row in the events table with an
// unrecognized format silently breaks the entire view. Coverage matters.
//
// Two format families show up in the wild:
//
//   - SQLite-canonical (modernc with _time_format=sqlite, current default):
//     "2006-01-02 15:04:05.999999999-07:00", with the fractional segment
//     dropped when nanos==0 ("2026-04-27 19:02:11+00:00").
//   - Legacy Go time.String() (modernc default before _time_format=sqlite):
//     "2006-01-02 15:04:05.999999999 -0700 MST", optionally with a
//     " m=+..." monotonic clock suffix, optionally with the fractional
//     segment dropped when nanos==0.
//
// Go's time.Parse treats `.999...` as an optional fractional component,
// so a single layout matches both fractional and non-fractional inputs.
// This test pins that behavior so a future layout edit or stdlib change
// can't silently regress the no-fractional path — which would manifest
// as the factory page going blank with a 500 the moment any zero-nano
// timestamp hits the events table.
func TestParseDBDatetime(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Time // zero means "expected to error"
	}{
		// --- modernc _time_format=sqlite output ---
		{
			name: "sqlite_canonical_with_fractional",
			in:   "2026-04-27 19:02:11.123456789-07:00",
			want: time.Date(2026, 4, 27, 19, 2, 11, 123456789, time.FixedZone("", -7*3600)),
		},
		{
			name: "sqlite_canonical_no_fractional",
			in:   "2026-04-27 19:02:11-07:00",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.FixedZone("", -7*3600)),
		},
		// --- modernc legacy Go time.String() output ---
		{
			name: "go_string_with_fractional_pdt",
			in:   "2026-04-27 19:02:11.123456789 -0700 PDT",
			want: time.Date(2026, 4, 27, 19, 2, 11, 123456789, time.FixedZone("PDT", -7*3600)),
		},
		{
			name: "go_string_no_fractional_pdt",
			in:   "2026-04-27 19:02:11 -0700 PDT",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.FixedZone("PDT", -7*3600)),
		},
		{
			name: "go_string_no_fractional_utc",
			in:   "2026-04-27 19:02:11 +0000 UTC",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.UTC),
		},
		{
			name: "go_string_with_monotonic_suffix",
			in:   "2026-04-27 19:02:11.123 -0700 PDT m=+1.500",
			want: time.Date(2026, 4, 27, 19, 2, 11, 123000000, time.FixedZone("PDT", -7*3600)),
		},
		{
			name: "go_string_with_negative_monotonic",
			in:   "2026-04-27 19:02:11 -0700 PDT m=-0.250",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.FixedZone("PDT", -7*3600)),
		},
		// --- SQLite default CURRENT_TIMESTAMP ---
		{
			name: "sqlite_current_timestamp",
			in:   "2026-04-27 19:02:11",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.UTC),
		},
		// --- RFC3339 (the GitHub side, also our own RFC3339Nano writes) ---
		{
			name: "rfc3339_zulu",
			in:   "2026-04-27T19:02:11Z",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.UTC),
		},
		{
			name: "rfc3339_with_offset",
			in:   "2026-04-27T19:02:11-07:00",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.FixedZone("", -7*3600)),
		},
		// --- empty input is a non-error sentinel ---
		{
			name: "empty",
			in:   "",
			want: time.Time{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDBDatetime(tc.in)
			if err != nil {
				t.Fatalf("parseDBDatetime(%q): unexpected error: %v", tc.in, err)
			}
			// Use Equal so location/abbreviation differences don't fail the
			// test as long as the instant is the same. The legacy Go-String
			// form encodes "PDT" which Go can't fully round-trip, but the
			// underlying instant is unambiguous.
			if !got.Equal(tc.want) {
				t.Errorf("parseDBDatetime(%q) = %v, want %v (equal-instant)", tc.in, got, tc.want)
			}
		})
	}
}

// TestDistinctEntityCountsByEventTypeLifetime pins the distinct-entity
// semantic on the factory's lifetime counter: re-entries by the same
// entity to the same station (e.g., a flaky CI check failing twice on
// one PR) collapse to a single count, while distinct entities at the
// same station accumulate. Without this contract the non-terminal
// stations' "PRs that ever reached this station" reading would inflate
// to "events fired here," which the front-screen counter is explicitly
// not measuring.
func TestDistinctEntityCountsByEventTypeLifetime(t *testing.T) {
	database := newTestDB(t)

	a := makeEntity(t, database, 1)
	b := makeEntity(t, database, 2)
	c := makeEntity(t, database, 3)

	// A: opened, ci_passed, merged — three distinct types, one entity each.
	recordEvent(t, database, a.ID, domain.EventGitHubPROpened)
	recordEvent(t, database, a.ID, domain.EventGitHubPRCICheckPassed)
	recordEvent(t, database, a.ID, domain.EventGitHubPRMerged)

	// B: opened, then ci_failed twice — re-entry must NOT double-count.
	recordEvent(t, database, b.ID, domain.EventGitHubPROpened)
	recordEvent(t, database, b.ID, domain.EventGitHubPRCICheckFailed)
	recordEvent(t, database, b.ID, domain.EventGitHubPRCICheckFailed)

	// C: opened, merged.
	recordEvent(t, database, c.ID, domain.EventGitHubPROpened)
	recordEvent(t, database, c.ID, domain.EventGitHubPRMerged)

	counts, err := DistinctEntityCountsByEventTypeLifetime(database)
	if err != nil {
		t.Fatalf("DistinctEntityCountsByEventTypeLifetime: %v", err)
	}

	want := map[string]int{
		domain.EventGitHubPROpened:        3, // A, B, C
		domain.EventGitHubPRCICheckPassed: 1, // A only
		domain.EventGitHubPRCICheckFailed: 1, // B only — twin re-entry collapses
		domain.EventGitHubPRMerged:        2, // A, C
	}
	for et, n := range want {
		if counts[et] != n {
			t.Errorf("counts[%q] = %d, want %d", et, counts[et], n)
		}
	}
}

// TestDistinctEntityCountsByEventTypeLifetime_IgnoresNullEntity confirms
// system events (entity_id IS NULL — poll markers, scoring sentinels)
// don't contribute to a station's distinct-entity count. The query's
// `WHERE entity_id IS NOT NULL` clause carries this contract; without
// it, every system-tagged event would inflate every station the system
// happens to tag.
func TestDistinctEntityCountsByEventTypeLifetime_IgnoresNullEntity(t *testing.T) {
	database := newTestDB(t)

	a := makeEntity(t, database, 1)
	recordEvent(t, database, a.ID, domain.EventGitHubPROpened)

	// System-style event: entity_id is NULL. Bypass RecordEvent because
	// its API takes a string EntityID — the call below talks to the
	// schema directly, which is the same path system emitters use.
	if _, err := database.Exec(`
		INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json)
		VALUES (?, NULL, ?, '', '')
	`, "test-system-event", domain.EventGitHubPROpened); err != nil {
		t.Fatalf("insert null-entity event: %v", err)
	}

	counts, err := DistinctEntityCountsByEventTypeLifetime(database)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if counts[domain.EventGitHubPROpened] != 1 {
		t.Errorf("counts[opened] = %d, want 1 (NULL entity_id row must not count)",
			counts[domain.EventGitHubPROpened])
	}
}

// TestListFactoryEntities_GraceWindow asserts the soft-close behavior:
// entities closed within FactoryClosedGracePeriod ride the snapshot
// alongside active entities (so the chip can finish its terminal
// animation), while entities closed earlier are excluded.
func TestListFactoryEntities_GraceWindow(t *testing.T) {
	database := newTestDB(t)

	active := makeEntity(t, database, 1)
	fresh := makeEntity(t, database, 2)
	stale := makeEntity(t, database, 3)

	// Inside the grace window — should appear.
	closeEntityAt(t, database, fresh.ID, time.Now().Add(-10*time.Second))
	// Past the grace window — should not appear.
	closeEntityAt(t, database, stale.ID, time.Now().Add(-FactoryClosedGracePeriod-30*time.Second))

	rows, err := ListFactoryEntities(database, 100)
	if err != nil {
		t.Fatalf("ListFactoryEntities: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Entity.ID] = true
	}
	if !got[active.ID] {
		t.Errorf("active entity %s missing from snapshot", active.ID)
	}
	if !got[fresh.ID] {
		t.Errorf("fresh-closed entity %s missing — should ride grace window", fresh.ID)
	}
	if got[stale.ID] {
		t.Errorf("stale-closed entity %s leaked through — outside grace window", stale.ID)
	}
}

// TestListFactoryEntities_ActiveLimitIsolatedFromClosureBurst is the
// regression for the "burst of closures crowds active out" issue. With
// the split-query design, the caller-supplied `limit` should always
// yield exactly that many active entities (when at least that many
// exist), regardless of how many entities recently closed.
func TestListFactoryEntities_ActiveLimitIsolatedFromClosureBurst(t *testing.T) {
	database := newTestDB(t)

	// Two active entities — the test's "always present" baseline.
	a1 := makeEntity(t, database, 1)
	a2 := makeEntity(t, database, 2)

	// 50 freshly-closed entities, all inside the grace window. Far more
	// than the active limit (2) but inside FactoryClosedGraceLimit (64).
	closedAt := time.Now().Add(-5 * time.Second)
	closedIDs := make(map[string]bool, 50)
	for i := 0; i < 50; i++ {
		e := makeEntity(t, database, 100+i)
		closeEntityAt(t, database, e.ID, closedAt)
		closedIDs[e.ID] = true
	}

	// Pass limit=2 — same as the active count. Pre-fix, the closure
	// burst would push at least one active entity out of the snapshot.
	rows, err := ListFactoryEntities(database, 2)
	if err != nil {
		t.Fatalf("ListFactoryEntities: %v", err)
	}

	activeFound := map[string]bool{}
	closedFound := 0
	for _, r := range rows {
		if r.Entity.State == "active" {
			activeFound[r.Entity.ID] = true
		} else if closedIDs[r.Entity.ID] {
			closedFound++
		}
	}
	if !activeFound[a1.ID] || !activeFound[a2.ID] {
		t.Errorf("active limit not isolated: got active set %v, want both %s and %s",
			activeFound, a1.ID, a2.ID)
	}
	if closedFound != 50 {
		t.Errorf("expected 50 closed-grace entities in snapshot, got %d", closedFound)
	}
}

// TestListFactoryEntities_ClosedGraceLimitCapsBurst checks the upper
// bound on the closed-side fan-in: even a pathological mass-close
// (more closures in the grace window than FactoryClosedGraceLimit)
// can't make the snapshot grow without bound.
func TestListFactoryEntities_ClosedGraceLimitCapsBurst(t *testing.T) {
	database := newTestDB(t)

	closedAt := time.Now().Add(-5 * time.Second)
	burst := FactoryClosedGraceLimit + 20
	for i := 0; i < burst; i++ {
		e := makeEntity(t, database, i)
		closeEntityAt(t, database, e.ID, closedAt)
	}

	rows, err := ListFactoryEntities(database, 100)
	if err != nil {
		t.Fatalf("ListFactoryEntities: %v", err)
	}
	if len(rows) != FactoryClosedGraceLimit {
		t.Errorf("len(rows) = %d, want %d (closed-grace cap binding)",
			len(rows), FactoryClosedGraceLimit)
	}
}

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

	// "bug" label added first, "help wanted" added second. The
	// type-only helper would return the "help wanted" event; the
	// dedup-aware helper must return the older "bug" event when
	// asked for it specifically.
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

// TestListActiveTaskRefsForEntities_FiltersTerminalStatuses pins the
// snapshot's pending_tasks contract: only non-terminal tasks ride
// (otherwise the drawer would offer to delegate already-resolved
// tasks). Active = NOT IN ('done', 'dismissed').
func TestListActiveTaskRefsForEntities_FiltersTerminalStatuses(t *testing.T) {
	database := newTestDB(t)
	a := makeEntity(t, database, 1)
	b := makeEntity(t, database, 2)
	evtA := recordEvent(t, database, a.ID, domain.EventGitHubPRCICheckPassed)
	evtB := recordEvent(t, database, b.ID, domain.EventGitHubPRCICheckPassed)

	// Active task on A — should be returned.
	activeTask, _, err := FindOrCreateTask(database, a.ID, domain.EventGitHubPRCICheckPassed, "", evtA, 0.5)
	if err != nil {
		t.Fatalf("create active task: %v", err)
	}
	// Done task on B — must be filtered out.
	doneTask, _, err := FindOrCreateTask(database, b.ID, domain.EventGitHubPRCICheckPassed, "", evtB, 0.5)
	if err != nil {
		t.Fatalf("create done task: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE tasks SET status = 'done', closed_at = ?, close_reason = 'manual' WHERE id = ?`,
		time.Now(), doneTask.ID,
	); err != nil {
		t.Fatalf("close task: %v", err)
	}

	tasks, err := ListActiveTaskRefsForEntities(database, []string{a.ID, b.ID})
	if err != nil {
		t.Fatalf("ListActiveTaskRefsForEntities: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1 (only active)", len(tasks))
	}
	if tasks[0].ID != activeTask.ID {
		t.Errorf("returned task ID = %s, want %s (active)", tasks[0].ID, activeTask.ID)
	}
}

// TestListActiveTaskRefsForEntities_EmptyInput — defensive: empty slice
// returns no rows without hitting the DB. The factory snapshot's
// entity list can legitimately be empty (fresh install, no
// integrations configured), and a "WHERE id IN ()" query is invalid
// in SQLite.
func TestListActiveTaskRefsForEntities_EmptyInput(t *testing.T) {
	database := newTestDB(t)
	tasks, err := ListActiveTaskRefsForEntities(database, nil)
	if err != nil {
		t.Fatalf("nil entityIDs: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("got %d tasks, want 0", len(tasks))
	}
}
