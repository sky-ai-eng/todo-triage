package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// eventStore is the Postgres impl of db.EventStore (SKY-305). SQL is
// written fresh against D3's schema: $N placeholders, JSONB cast on
// metadata_json, explicit org_id bind (the column is NOT NULL with
// no default), and org_id in every WHERE clause as defense in depth
// alongside RLS policy events_all.
//
// Holds two write paths (app + admin) plus an optional
// PendingEventHooks per path. When the path's pending pointer is
// non-nil, the SetOnEventRecorded hook is deferred to commit time
// (the TxRunner allocates the buffer and calls Fire() after the
// outer tx commits, dropping it on rollback). When nil, the hook
// fires immediately on a successful INSERT.
//
// In production WithTx: q is the *sql.Tx → defers; admin is
// s.admin → fires immediately (autonomous pool). In NewForTx (test
// door) both halves collapse to the same tx and both defer to the
// same buffer. Non-tx wiring (Store.New) passes nil for both pendings
// so the hook fires eagerly as before.
type eventStore struct {
	app   writePath
	admin writePath
}

// writePath is a queryer paired with the deferral buffer that
// governs hook firing for its writes. nil pending = "the write is
// durable when ExecContext returns nil" (autonomous pool or
// committed tx).
type writePath struct {
	q       queryer
	pending *db.PendingEventHooks
}

// newEventStore constructs the eager-hook variant used by Store.New
// for non-tx wiring. Both halves fire SetOnEventRecorded the
// moment the INSERT returns nil.
func newEventStore(q, admin queryer) db.EventStore {
	return &eventStore{
		app:   writePath{q: q},
		admin: writePath{q: admin},
	}
}

// newTxEventStore is the tx-bound variant. appPending controls
// hook deferral for Record (q-side); adminPending controls deferral
// for RecordSystem (admin-side). Pass nil for adminPending in
// production WithTx — the admin pool commits autonomously, so its
// hook fires immediately. For NewForTx (test door) pass the same
// buffer for both so the test's manual commit/rollback governs all
// hook firing uniformly.
func newTxEventStore(q, admin queryer, appPending, adminPending *db.PendingEventHooks) db.EventStore {
	return &eventStore{
		app:   writePath{q: q, pending: appPending},
		admin: writePath{q: admin, pending: adminPending},
	}
}

var _ db.EventStore = (*eventStore)(nil)

func (s *eventStore) Record(ctx context.Context, orgID string, evt domain.Event) (string, error) {
	return s.app.record(ctx, orgID, evt)
}

func (s *eventStore) RecordSystem(ctx context.Context, orgID string, evt domain.Event) (string, error) {
	return s.admin.record(ctx, orgID, evt)
}

// record runs the INSERT on the path's queryer and either fires
// the hook immediately or enqueues it on the pending buffer.
//
// metadata_json casts to jsonb at the placeholder so the caller
// hands a marshalled string — same pattern ProjectStore uses for
// pinned_repos. Empty MetadataJSON binds as NULL since jsonb rejects
// the empty string as invalid JSON; downstream reads COALESCE the
// column back to "" so the domain-level contract (empty = no
// metadata) stays intact.
//
// created_at is bound from time.Now() rather than relying on the
// DEFAULT now() column expression. Postgres' now() is the transaction
// start timestamp — multiple INSERTs in one tx all tie, and the
// LIMIT-1 ordering in LatestForEntityTypeAndDedupKey then falls
// through to the UUID tiebreaker which is random. Binding from Go
// gives nanosecond resolution that's monotonic within a process for
// sequential calls, so same-tx records sort by insertion order.
//
// occurred_at follows the nullable contract: zero time → NULL column.
// Chronological queries fall back to created_at via COALESCE.
func (wp *writePath) record(ctx context.Context, orgID string, evt domain.Event) (string, error) {
	id := evt.ID
	if id == "" {
		id = uuid.New().String()
	}
	var entityID any
	if evt.EntityID != nil && *evt.EntityID != "" {
		entityID = *evt.EntityID
	}
	var metadata any
	if evt.MetadataJSON != "" {
		metadata = evt.MetadataJSON
	}
	var occurredAt any
	if !evt.OccurredAt.IsZero() {
		occurredAt = evt.OccurredAt
	}
	createdAt := time.Now()
	if _, err := wp.q.ExecContext(ctx, `
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, occurred_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8)
	`, id, orgID, entityID, evt.EventType, evt.DedupKey, metadata, occurredAt, createdAt); err != nil {
		return "", err
	}
	evt.ID = id
	evt.CreatedAt = createdAt
	if wp.pending != nil {
		wp.pending.Add(evt)
	} else {
		db.NotifyEventRecorded(evt)
	}
	return id, nil
}

func (s *eventStore) LatestForEntityTypeAndDedupKey(ctx context.Context, orgID, entityID, eventType, dedupKey string) (*domain.Event, error) {
	// Primary sort on COALESCE(occurred_at, created_at) so the row
	// picked reflects underlying event order when the source clock
	// is known (check completion time, review submission time).
	// Fall back to created_at for events that don't carry a source
	// timestamp (system events, derived events). The id DESC at the
	// end is a deterministic tiebreaker for the otherwise-impossible
	// case where two writes share both timestamps to the nanosecond.
	row := s.app.q.QueryRowContext(ctx, `
		SELECT id, entity_id, event_type, dedup_key,
		       COALESCE(metadata_json::text, ''), occurred_at, created_at
		FROM events
		WHERE org_id = $1 AND entity_id = $2 AND event_type = $3 AND dedup_key = $4
		ORDER BY COALESCE(occurred_at, created_at) DESC, created_at DESC, id DESC
		LIMIT 1
	`, orgID, entityID, eventType, dedupKey)
	var evt domain.Event
	var entID sql.NullString
	var occurredAt sql.NullTime
	if err := row.Scan(&evt.ID, &entID, &evt.EventType, &evt.DedupKey,
		&evt.MetadataJSON, &occurredAt, &evt.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if entID.Valid {
		s := entID.String
		evt.EntityID = &s
	}
	if occurredAt.Valid {
		evt.OccurredAt = occurredAt.Time
	}
	return &evt, nil
}

func (s *eventStore) GetMetadataSystem(ctx context.Context, orgID, eventID string) (string, error) {
	var metadata sql.NullString
	err := s.admin.q.QueryRowContext(ctx, `
		SELECT metadata_json::text FROM events WHERE org_id = $1 AND id = $2
	`, orgID, eventID).Scan(&metadata)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return metadata.String, nil
}
