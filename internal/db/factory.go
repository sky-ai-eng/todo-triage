package db

import (
	"database/sql"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Factory-view queries. All scoped to "active" state (entities that haven't
// closed, runs that haven't terminated) and to 24h windows for throughput
// counts. Used by the /api/factory/snapshot handler to drive the 2.5D
// visualization at /factory.

// factoryActiveRunStatuses is the set of run.status values we treat as "in
// flight" for the factory view. Matches the X-button window in AgentCard —
// every state before a terminal transition (completed | failed | cancelled
// | task_unsolvable). pending_approval counts as active: the run is paused
// waiting for user input, not done.
var factoryActiveRunStatuses = []string{
	"initializing",
	"cloning",
	"fetching",
	"worktree_created",
	"agent_starting",
	"running",
	"pending_approval",
}

// EventCountsByTypeSince counts events per event_type that were emitted
// after `since`. Keys with zero counts are absent. Uses
// idx_events_type_created for the scan.
func EventCountsByTypeSince(database *sql.DB, since time.Time) (map[string]int, error) {
	rows, err := database.Query(`
		SELECT event_type, COUNT(*)
		FROM events
		WHERE created_at > ?
		GROUP BY event_type
	`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var eventType string
		var count int
		if err := rows.Scan(&eventType, &count); err != nil {
			return nil, err
		}
		out[eventType] = count
	}
	return out, rows.Err()
}

// DistinctEntityCountsByEventTypeLifetime counts the distinct entities
// that have ever produced an event of each event_type, from catalog
// start (no time cutoff). Production reads of this number go through
// LifetimeDistinctCounter, which folds events incrementally off the
// bus and avoids the per-request scan; this function survives as the
// canonical SQL aggregate behind that counter — pinned by tests, and
// available for one-shot reconciliation if cache drift is ever
// suspected.
//
// "Ready for Review · 47" reads as "47 distinct PRs have reached this
// station," not "47 events fired" — re-entries from the same entity
// (e.g. a flaky CI check failing twice on one PR) don't double-count.
// For terminal event types (PR merged/closed) the answer matches a
// plain COUNT(*) since each entity contributes exactly one terminal
// event.
//
// Covered by the partial index `idx_events_type_entity (event_type,
// entity_id) WHERE entity_id IS NOT NULL`: SQLite walks the index in
// (event_type, entity_id) order, so each event_type group's distinct
// entity_ids are contiguous and DISTINCT collapses to a per-group
// adjacency dedupe — no temp B-tree, no table touch. Without that
// index this query degrades to a full scan with a hash dedupe per
// group, which is why we don't run it on the hot path.
func DistinctEntityCountsByEventTypeLifetime(database *sql.DB) (map[string]int, error) {
	rows, err := database.Query(`
		SELECT event_type, COUNT(DISTINCT entity_id)
		FROM events
		WHERE entity_id IS NOT NULL
		GROUP BY event_type
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var eventType string
		var count int
		if err := rows.Scan(&eventType, &count); err != nil {
			return nil, err
		}
		out[eventType] = count
	}
	return out, rows.Err()
}

// TaskCountsByEventTypeSince counts tasks per event_type created after
// `since`. Used alongside EventCountsByTypeSince to compute the "triggered
// / seen" ratio displayed in the station overlay.
func TaskCountsByEventTypeSince(database *sql.DB, since time.Time) (map[string]int, error) {
	rows, err := database.Query(`
		SELECT event_type, COUNT(*)
		FROM tasks
		WHERE created_at > ?
		GROUP BY event_type
	`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var eventType string
		var count int
		if err := rows.Scan(&eventType, &count); err != nil {
			return nil, err
		}
		out[eventType] = count
	}
	return out, rows.Err()
}

// FactoryActiveRun is a run + its task + enough entity context to render an
// overlay row and decide ownership tint. Returned by ListFactoryActiveRuns
// so the handler doesn't have to do N+1 follow-up queries.
type FactoryActiveRun struct {
	Run            domain.AgentRun
	Task           domain.Task
	EntityAuthor   string // PR author login (github) or assignee (jira); "" if unknown
	EntityEventTyp string // task.event_type; pre-copied for keyed lookup
}

// ListFactoryActiveRuns returns every run currently in-flight (status in
// factoryActiveRunStatuses) joined with its task and entity. Ordered by
// started_at so the overlay can render most-recent-first without client-side
// sorting.
func ListFactoryActiveRuns(database *sql.DB) ([]FactoryActiveRun, error) {
	placeholders := "?"
	args := make([]any, 0, len(factoryActiveRunStatuses))
	args = append(args, factoryActiveRunStatuses[0])
	for i := 1; i < len(factoryActiveRunStatuses); i++ {
		placeholders += ", ?"
		args = append(args, factoryActiveRunStatuses[i])
	}

	query := `
		SELECT
			r.id, r.task_id, COALESCE(r.prompt_id, ''),
			r.status, COALESCE(r.model, ''), r.started_at, r.completed_at,
			r.total_cost_usd, r.duration_ms, r.num_turns,
			COALESCE(r.stop_reason, ''), COALESCE(r.worktree_path, ''),
			COALESCE(r.result_summary, ''), COALESCE(r.session_id, ''),
			r.memory_missing, r.trigger_type, COALESCE(r.trigger_id, ''),
			` + taskColumnsWithEntity + `
		FROM runs r
		JOIN tasks t ON r.task_id = t.id
		JOIN entities e ON t.entity_id = e.id
		WHERE r.status IN (` + placeholders + `)
		ORDER BY r.started_at DESC
	`

	rows, err := database.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FactoryActiveRun
	for rows.Next() {
		var r domain.AgentRun
		var t domain.Task
		var completedAt sql.NullTime
		var costUSD sql.NullFloat64
		var durationMs, numTurns sql.NullInt64

		// Run half: nullable numeric/time columns scanned via NullX,
		// nullable text columns COALESCEd in the SELECT above.
		runTargets := []any{
			&r.ID, &r.TaskID, &r.PromptID,
			&r.Status, &r.Model, &r.StartedAt, &completedAt,
			&costUSD, &durationMs, &numTurns,
			&r.StopReason, &r.WorktreePath,
			&r.ResultSummary, &r.SessionID,
			&r.MemoryMissing, &r.TriggerType, &r.TriggerID,
		}
		// Task half: taskScanState holds the NullX intermediates on the
		// caller's stack so NULL-able text columns (ai_summary, severity,
		// close_reason, ...) don't error on "converting NULL to string" for
		// unscored tasks, without a per-row closure/heap allocation.
		var ts taskScanState
		if err := rows.Scan(append(runTargets, ts.targets(&t)...)...); err != nil {
			return nil, err
		}
		ts.finalize(&t)
		if completedAt.Valid {
			r.CompletedAt = &completedAt.Time
		}
		if costUSD.Valid {
			r.TotalCostUSD = &costUSD.Float64
		}
		if durationMs.Valid {
			v := int(durationMs.Int64)
			r.DurationMs = &v
		}
		if numTurns.Valid {
			v := int(numTurns.Int64)
			r.NumTurns = &v
		}
		out = append(out, FactoryActiveRun{Run: r, Task: t, EntityEventTyp: t.EventType})
	}
	return out, rows.Err()
}

// FactoryEntityRow is an active entity plus the event_type of its most
// recent event. LatestEventType is "" if the entity has no recorded events
// (fresh discovery before any diff has fired). LatestEventAt is nil under
// the same condition.
type FactoryEntityRow struct {
	Entity          domain.Entity
	LatestEventType string
	LatestEventAt   *time.Time
}

// FactoryRecentEvent is a single entry in an entity's recent event history.
// Ordered chronologically ascending by caller (ListRecentEventsByEntity).
//
// Two timestamps because we need both for the factory animation:
//   - CreatedAt is the "event time" used for chain ordering: occurred_at
//     when the upstream system reported it, falling back to detection
//     time. So two events from one poll order by their upstream times,
//     not their insert order.
//   - DetectedAt is purely the row's insert time (events.created_at).
//     Used for clustering events into "this poll's burst" — events from
//     a single poll cycle insert within milliseconds, so a small gap
//     test on this field cleanly separates one poll's chain from another.
//     Independent of upstream timestamps, which can be arbitrarily lagged.
type FactoryRecentEvent struct {
	EventType  string
	CreatedAt  time.Time
	DetectedAt time.Time
}

// ListRecentEventsByEntity returns the last `perEntity` events per
// entity id, grouped in a map keyed by entity_id with each slice ordered
// chronologically ascending (oldest first). Used to drive the factory's
// chain animation — when two events fire for the same entity in a single
// poll cycle (new_commits → ci_passed), we want to see the item travel
// both stations rather than teleport to the second.
//
// Single query with a row-number window partition so we pull at most
// perEntity*len(ids) rows rather than scanning the whole events table.
// Chunks on len(ids) > SQLite's variable limit the same way the scorer's
// description loader does — factoryEntityLimit is 100 today so we never
// hit it, but the guard is cheap.
func ListRecentEventsByEntity(database *sql.DB, ids []string, perEntity int) (map[string][]FactoryRecentEvent, error) {
	out := map[string][]FactoryRecentEvent{}
	if len(ids) == 0 || perEntity <= 0 {
		return out, nil
	}
	// Chunk to respect SQLite's SQLITE_LIMIT_VARIABLE_NUMBER.
	const chunkSize = 500
	for start := 0; start < len(ids); start += chunkSize {
		end := start + chunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		args = append(args, perEntity)

		// Chain ordering is driven by source time (commit committed_at,
		// check-run completed_at, review submitted_at) when available —
		// detection time (created_at) as a fallback. rowid is a tie-breaker
		// for genuine collisions (two events sharing a source timestamp to
		// the second, or both lacking a source time and inserted in the
		// same poll cycle). Inner window partitions on COALESCE so the
		// "most recent" per entity is stable regardless of which column is
		// populated.
		query := `
			SELECT entity_id, event_type, event_at, detected_at
			FROM (
				SELECT entity_id, event_type,
					COALESCE(occurred_at, created_at) AS event_at,
					created_at AS detected_at,
					rowid AS row_id,
					ROW_NUMBER() OVER (
						PARTITION BY entity_id
						ORDER BY COALESCE(occurred_at, created_at) DESC, rowid DESC
					) AS rn
				FROM events
				WHERE entity_id IN (` + strings.Join(placeholders, ",") + `)
			)
			WHERE rn <= ?
			ORDER BY entity_id, event_at ASC, row_id ASC
		`
		rows, err := database.Query(query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var entityID, eventType, eventAtStr, detectedAtStr string
			// COALESCE over two DATETIME columns loses the column-type
			// metadata the SQLite driver needs to scan directly into
			// time.Time (the driver returns the value as a string in
			// that case). Scan as string and parse ourselves — cheap
			// and consistent across whichever source column contributed
			// the value. detected_at is plain `created_at` but we scan
			// it as a string too for symmetry with the COALESCEd column.
			if err := rows.Scan(&entityID, &eventType, &eventAtStr, &detectedAtStr); err != nil {
				rows.Close()
				return nil, err
			}
			eventAt, err := parseDBDatetime(eventAtStr)
			if err != nil {
				rows.Close()
				return nil, err
			}
			detectedAt, err := parseDBDatetime(detectedAtStr)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[entityID] = append(out[entityID], FactoryRecentEvent{
				EventType:  eventType,
				CreatedAt:  eventAt,
				DetectedAt: detectedAt,
			})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// parseDBDatetime handles the ISO-8601 and SQLite-default datetime
// formats we see in the events table. Direct time.Time scans work for
// plain DATETIME columns because the SQLite driver reads the column
// type and parses; once COALESCE enters the picture that type metadata
// is gone and we get back a raw string.
//
// Accepted shapes, in order:
//   - RFC3339 ("2006-01-02T15:04:05Z07:00")
//   - modernc with _time_format=sqlite ("2006-01-02 15:04:05.999999999-07:00")
//   - SQLite default ("2006-01-02 15:04:05" — CURRENT_TIMESTAMP, naive UTC)
//   - RFC3339 with fractional seconds and 'T' separator
//   - Legacy Go time.String() ("2006-01-02 15:04:05.999999999 -0700 MST"),
//     produced by older modernc-driver writes that bound time.Time as its
//     default String() form. The monotonic-clock suffix " m=+..." is
//     stripped before parsing because time.Parse can't consume it.
func parseDBDatetime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02 15:04:05.999999999-07:00", s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02T15:04:05.999999999Z07:00", s); err == nil {
		return t, nil
	}
	cleaned := s
	if i := strings.Index(cleaned, " m=+"); i >= 0 {
		cleaned = cleaned[:i]
	} else if i := strings.Index(cleaned, " m=-"); i >= 0 {
		cleaned = cleaned[:i]
	}
	return time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", cleaned)
}

// FactoryClosedGracePeriod is the window during which a freshly-closed
// entity remains in the factory snapshot so the chip can ride the final
// bridge into its terminal station before disposal. One full poll cycle
// (~30s baseline) plus headroom — generous enough to cover any chain
// animation duration without being load-bearing.
const FactoryClosedGracePeriod = 60 * time.Second

// FactoryClosedGraceLimit caps how many recently-closed entities ride
// alongside the active set in a snapshot. Bounded separately from
// `factoryEntityLimit` so a burst of closures can't crowd active
// entities out of the displayed set — the active limit keeps its
// original meaning regardless of close pressure. 64 is generous: even
// a worst-case mass-merge of half a sprint's PRs in one poll cycle
// fits within it without overflow.
const FactoryClosedGraceLimit = 64

const factoryEntitySelectColumns = `
	e.id, e.source, e.source_id, e.kind,
	COALESCE(e.title, ''), COALESCE(e.url, ''),
	COALESCE(e.snapshot_json, ''), COALESCE(e.description, ''),
	e.state, e.created_at, e.last_polled_at, e.closed_at,
	(SELECT event_type FROM events WHERE entity_id = e.id ORDER BY created_at DESC LIMIT 1),
	-- Direct column read (not COALESCE) so the SQLite driver keeps
	-- the DATETIME column-type hint and scans into sql.NullTime.
	-- The per-event source timestamps come via ListRecentEventsByEntity,
	-- which does the COALESCE + string-parse dance.
	(SELECT created_at FROM events WHERE entity_id = e.id ORDER BY created_at DESC LIMIT 1)
`

// ListFactoryEntities returns the active set of entities (up to `limit`)
// plus any entity closed within FactoryClosedGracePeriod (up to
// FactoryClosedGraceLimit) so the chip can finish animating to its
// terminal station before disappearing.
//
// Implemented as two separate queries instead of a single OR'd WHERE:
//
//   - The OR (`state='active' OR closed_at > ?`) spans two columns and
//     prevents SQLite from choosing a single-index plan, forcing a
//     filtered table scan on big entity tables.
//   - A combined LIMIT also lets a burst of closures crowd the active
//     set out of the snapshot, silently shrinking the meaning of
//     `limit` — the active half should always get its full budget.
//
// Active side uses idx_entities_state and falls back to a small in-
// memory sort by created_at; closed side uses idx_entities_closed_at
// (partial, defined in db.go) and is bounded by both the time window
// and the small grace limit, so the lookup is near-free even on
// large entity tables.
func ListFactoryEntities(database *sql.DB, limit int) ([]FactoryEntityRow, error) {
	active, err := queryFactoryEntities(database, `
		SELECT `+factoryEntitySelectColumns+`
		FROM entities e
		WHERE e.state = 'active'
		ORDER BY e.created_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}

	graceCutoff := time.Now().Add(-FactoryClosedGracePeriod)
	closed, err := queryFactoryEntities(database, `
		SELECT `+factoryEntitySelectColumns+`
		FROM entities e
		WHERE e.closed_at IS NOT NULL AND e.closed_at > ?
		ORDER BY e.closed_at DESC
		LIMIT ?
	`, graceCutoff, FactoryClosedGraceLimit)
	if err != nil {
		return nil, err
	}

	if len(closed) == 0 {
		return active, nil
	}
	return append(active, closed...), nil
}

func queryFactoryEntities(database *sql.DB, query string, args ...any) ([]FactoryEntityRow, error) {
	rows, err := database.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FactoryEntityRow
	for rows.Next() {
		var row FactoryEntityRow
		var latestType sql.NullString
		var latestAt sql.NullTime
		if err := rows.Scan(
			&row.Entity.ID, &row.Entity.Source, &row.Entity.SourceID, &row.Entity.Kind,
			&row.Entity.Title, &row.Entity.URL,
			&row.Entity.SnapshotJSON, &row.Entity.Description,
			&row.Entity.State, &row.Entity.CreatedAt, &row.Entity.LastPolledAt, &row.Entity.ClosedAt,
			&latestType, &latestAt,
		); err != nil {
			return nil, err
		}
		if latestType.Valid {
			row.LatestEventType = latestType.String
		}
		if latestAt.Valid {
			row.LatestEventAt = &latestAt.Time
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
