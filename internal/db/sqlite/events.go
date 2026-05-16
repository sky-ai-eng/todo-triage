package sqlite

import (
	"context"
	"database/sql"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// eventStore is the SQLite impl of db.EventStore (SKY-305). SQL
// bodies are moved verbatim from the pre-store internal/db/events.go;
// the only behavioral changes are:
//
//   - assertLocalOrg(orgID) at every entry point — the SQLite events
//     row defaults org_id to LocalDefaultOrgID, and a non-default
//     value indicates a confused caller that thinks it's in multi
//     mode (would silently misbehave against the single-tenant
//     schema).
//   - The `...System` methods (RecordSystem, GetMetadataSystem)
//     collapse onto the non-System bodies. SQLite has one
//     connection, so the dual-pool routing is a Postgres-only
//     concern; the methods exist on the interface for parity with
//     the Postgres impl.
//
// The constructor takes two queryers for signature parity with the
// Postgres impl, but both arguments collapse onto the same queryer
// here.
type eventStore struct{ q queryer }

func newEventStore(q, _ queryer) db.EventStore { return &eventStore{q: q} }

var _ db.EventStore = (*eventStore)(nil)

func (s *eventStore) Record(ctx context.Context, orgID string, evt domain.Event) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	id := evt.ID
	if id == "" {
		id = uuid.New().String()
	}
	// occurred_at is nullable — leave it unset in the row when the caller
	// doesn't have a source timestamp. Factory and other chronological
	// queries COALESCE(occurred_at, created_at) and degrade gracefully.
	var occurredAt any
	if !evt.OccurredAt.IsZero() {
		occurredAt = evt.OccurredAt
	}
	var entityID any
	if evt.EntityID != nil && *evt.EntityID != "" {
		entityID = *evt.EntityID
	}
	if _, err := s.q.ExecContext(ctx, `
		INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, id, entityID, evt.EventType, evt.DedupKey, evt.MetadataJSON, occurredAt); err != nil {
		return "", err
	}
	// Fire the package-level hook so LifetimeDistinctCounter stays
	// aligned with the DB regardless of which code path wrote the row.
	// evt.ID must reflect the persisted identity before fan-out.
	evt.ID = id
	db.NotifyEventRecorded(evt)
	return id, nil
}

// RecordSystem mirrors Record. SQLite has one connection, so the
// admin/app pool distinction collapses onto the same body.
func (s *eventStore) RecordSystem(ctx context.Context, orgID string, evt domain.Event) (string, error) {
	return s.Record(ctx, orgID, evt)
}

func (s *eventStore) LatestForEntityTypeAndDedupKey(ctx context.Context, orgID, entityID, eventType, dedupKey string) (*domain.Event, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT id, entity_id, event_type, dedup_key,
		       COALESCE(metadata_json, ''), occurred_at, created_at
		FROM events
		WHERE entity_id = ? AND event_type = ? AND dedup_key = ?
		ORDER BY created_at DESC, rowid DESC
		LIMIT 1
	`, entityID, eventType, dedupKey)
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
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	var metadata sql.NullString
	err := s.q.QueryRowContext(ctx, `SELECT metadata_json FROM events WHERE id = ?`, eventID).Scan(&metadata)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return metadata.String, nil
}
