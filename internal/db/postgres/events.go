package postgres

import (
	"context"
	"database/sql"

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
// Holds two pools:
//
//   - q: app pool (tf_app, RLS-active). Request-handler-equivalent
//     consumers (stock carry-over, factory drag-to-delegate) route
//     here. RLS policy events_all gates the statement; the caller
//     must be inside WithTx so request.jwt.claims is set.
//
//   - admin: admin pool (supabase_admin, BYPASSRLS). Background-
//     goroutine consumers without a JWT-claims context (router
//     subscriber + re-derive, delegate post-run metadata read)
//     route here via the `...System` methods. org_id stays bound in
//     the INSERT/SELECT as defense in depth.
type eventStore struct {
	q     queryer
	admin queryer
}

func newEventStore(q, admin queryer) db.EventStore {
	return &eventStore{q: q, admin: admin}
}

var _ db.EventStore = (*eventStore)(nil)

func (s *eventStore) Record(ctx context.Context, orgID string, evt domain.Event) (string, error) {
	return recordEvent(ctx, s.q, orgID, evt)
}

func (s *eventStore) RecordSystem(ctx context.Context, orgID string, evt domain.Event) (string, error) {
	return recordEvent(ctx, s.admin, orgID, evt)
}

// recordEvent is the shared INSERT body. metadata_json casts to jsonb
// at the placeholder so the caller hands a marshalled string — same
// pattern ProjectStore uses for pinned_repos. Empty MetadataJSON
// binds as NULL since jsonb rejects the empty string as invalid JSON;
// downstream
// reads COALESCE the column back to "" so the domain-level contract
// (empty = no metadata) stays intact.
//
// occurred_at follows the same nullable contract as the SQLite impl:
// zero time → NULL column → chronological queries fall back to
// created_at via COALESCE.
func recordEvent(ctx context.Context, q queryer, orgID string, evt domain.Event) (string, error) {
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
	if _, err := q.ExecContext(ctx, `
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)
	`, id, orgID, entityID, evt.EventType, evt.DedupKey, metadata, occurredAt); err != nil {
		return "", err
	}
	// Fire the package-level hook so LifetimeDistinctCounter (and any
	// future subscriber registered via db.SetOnEventRecorded) stays in
	// sync regardless of which pool the write went through. evt.ID
	// must reflect the persisted identity, so stamp it before fan-out.
	evt.ID = id
	db.NotifyEventRecorded(evt)
	return id, nil
}

func (s *eventStore) LatestForEntityTypeAndDedupKey(ctx context.Context, orgID, entityID, eventType, dedupKey string) (*domain.Event, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT id, entity_id, event_type, dedup_key,
		       COALESCE(metadata_json::text, ''), occurred_at, created_at
		FROM events
		WHERE org_id = $1 AND entity_id = $2 AND event_type = $3 AND dedup_key = $4
		ORDER BY created_at DESC, id DESC
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
	err := s.admin.QueryRowContext(ctx, `
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
