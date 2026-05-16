package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// eventStore is the SQLite impl of db.EventStore (SKY-305). SQL
// bodies are moved verbatim from the pre-store internal/db/events.go;
// behavioral changes:
//
//   - assertLocalOrg(orgID) at every entry point.
//   - The `...System` methods route through the same underlying
//     connection as Record (SQLite has one) but each path gets its
//     own deferral slot so the Postgres-side asymmetry — Record
//     defers, RecordSystem fires immediately in production WithTx —
//     can be mirrored or collapsed per call site.
//   - created_at is bound from time.Now() rather than relying on
//     CURRENT_TIMESTAMP (second resolution). Same-tx writes now
//     order by insertion time at nanosecond resolution.
//
// In SQLite-WithTx every write goes through the single tx connection,
// so both halves defer to the same buffer; on rollback the
// LifetimeDistinctCounter never observes the events.
type eventStore struct {
	app   writePath
	admin writePath
}

type writePath struct {
	q       queryer
	pending *db.PendingEventHooks
}

func newEventStore(q, admin queryer) db.EventStore {
	return &eventStore{
		app:   writePath{q: q},
		admin: writePath{q: admin},
	}
}

// newTxEventStore is the tx-bound variant for runTx. Both halves
// collapse to the same tx in SQLite, and so does the deferral
// buffer — pass the same *PendingEventHooks for appPending and
// adminPending. The tx wrapper drains it post-commit.
func newTxEventStore(q, admin queryer, appPending, adminPending *db.PendingEventHooks) db.EventStore {
	return &eventStore{
		app:   writePath{q: q, pending: appPending},
		admin: writePath{q: admin, pending: adminPending},
	}
}

var _ db.EventStore = (*eventStore)(nil)

func (s *eventStore) Record(ctx context.Context, orgID string, evt domain.Event) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	return s.app.record(ctx, evt)
}

func (s *eventStore) RecordSystem(ctx context.Context, orgID string, evt domain.Event) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	return s.admin.record(ctx, evt)
}

func (wp *writePath) record(ctx context.Context, evt domain.Event) (string, error) {
	id := evt.ID
	if id == "" {
		id = uuid.New().String()
	}
	// occurred_at is nullable — leave it unset when the caller
	// doesn't have a source timestamp. Factory and other
	// chronological queries COALESCE(occurred_at, created_at).
	var occurredAt any
	if !evt.OccurredAt.IsZero() {
		occurredAt = evt.OccurredAt
	}
	var entityID any
	if evt.EntityID != nil && *evt.EntityID != "" {
		entityID = *evt.EntityID
	}
	// Bind created_at from Go rather than CURRENT_TIMESTAMP so the
	// LIMIT-1 ordering in LatestForEntityTypeAndDedupKey gets
	// nanosecond resolution across same-tx writes. The schema's
	// DEFAULT CURRENT_TIMESTAMP is second-resolution and would tie
	// for any burst-rate inserts (poller diff fanout, carry-over
	// loops), falling through to a rowid-DESC tiebreak that doesn't
	// always agree with insertion order under WAL.
	createdAt := time.Now()
	if _, err := wp.q.ExecContext(ctx, `
		INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, occurred_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, id, entityID, evt.EventType, evt.DedupKey, evt.MetadataJSON, occurredAt, createdAt); err != nil {
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
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// Same ordering rule as the Postgres impl — primary sort on
	// COALESCE(occurred_at, created_at) so the audit row picked
	// reflects underlying event time when known, falling back to
	// insertion order otherwise. rowid DESC is the stable
	// final-tiebreaker since the TEXT PK doesn't sort meaningfully.
	row := s.app.q.QueryRowContext(ctx, `
		SELECT id, entity_id, event_type, dedup_key,
		       COALESCE(metadata_json, ''), occurred_at, created_at
		FROM events
		WHERE entity_id = ? AND event_type = ? AND dedup_key = ?
		ORDER BY COALESCE(occurred_at, created_at) DESC, created_at DESC, rowid DESC
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
	err := s.admin.q.QueryRowContext(ctx, `SELECT metadata_json FROM events WHERE id = ?`, eventID).Scan(&metadata)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return metadata.String, nil
}
