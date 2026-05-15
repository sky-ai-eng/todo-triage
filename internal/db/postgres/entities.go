package postgres

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// entityStore is the Postgres impl of db.EntityStore. Wired against
// the app pool in postgres.New: every consumer is request-equivalent
// (server panels, classifier, delegate context loaders) or runs in a
// server-side goroutine that already operates within the org's
// identity scope (the tracker, started at server boot). RLS policy
// entities_all gates every read/write on
// (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id));
// org_id is also included in every WHERE clause as defense in depth.
//
// SQL is written fresh against D3's schema: $N placeholders, JSONB
// cast on snapshot_json reads/writes, explicit timestamptz binds so
// poll cycles share a time source with the SQLite path rather than
// drifting onto Postgres's now().
type entityStore struct{ q queryer }

func newEntityStore(q queryer) db.EntityStore { return &entityStore{q: q} }

var _ db.EntityStore = (*entityStore)(nil)

// pgEntitySelectCols is the column list shared by every entity read.
// snapshot_json is cast to text so the Go side gets the same string
// shape SQLite returns; the caller pipes that through json.Unmarshal
// when it needs structured data.
const pgEntitySelectCols = `id, source, source_id, kind, COALESCE(title, ''), COALESCE(url, ''),
       COALESCE(snapshot_json::text, ''), COALESCE(description, ''), state, project_id,
       COALESCE(classification_rationale, ''), created_at, last_polled_at, closed_at`

// --- Lookup ---

func (s *entityStore) Get(ctx context.Context, orgID, id string) (*domain.Entity, error) {
	row := s.q.QueryRowContext(ctx, `SELECT `+pgEntitySelectCols+` FROM entities WHERE org_id = $1 AND id = $2`, orgID, id)
	return scanEntityRow(row)
}

func (s *entityStore) GetBySource(ctx context.Context, orgID, source, sourceID string) (*domain.Entity, error) {
	row := s.q.QueryRowContext(ctx, `SELECT `+pgEntitySelectCols+` FROM entities WHERE org_id = $1 AND source = $2 AND source_id = $3`, orgID, source, sourceID)
	return scanEntityRow(row)
}

func (s *entityStore) Descriptions(ctx context.Context, orgID string, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return out, nil
	}

	// Postgres can take an array directly via = ANY($2); no manual
	// chunking needed (the parameter count cap that drives SQLite's
	// chunked path doesn't apply when the list is a single array
	// bind).
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, COALESCE(description, '')
		FROM entities
		WHERE org_id = $1 AND id = ANY($2)
	`, orgID, pgUUIDArray(unique))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, desc string
		if err := rows.Scan(&id, &desc); err != nil {
			return nil, err
		}
		if desc != "" {
			out[id] = desc
		}
	}
	return out, rows.Err()
}

func (s *entityStore) ListUnclassified(ctx context.Context, orgID string) ([]domain.Entity, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgEntitySelectCols+`
		FROM entities
		WHERE org_id = $1 AND project_id IS NULL AND classified_at IS NULL AND state = 'active'
		ORDER BY created_at ASC
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntityList(rows)
}

func (s *entityStore) ListActive(ctx context.Context, orgID, source string) ([]domain.Entity, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgEntitySelectCols+`
		FROM entities
		WHERE org_id = $1 AND source = $2 AND state = 'active'
		ORDER BY last_polled_at ASC
	`, orgID, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntityList(rows)
}

func (s *entityStore) ListProjectPanel(ctx context.Context, orgID, projectID string) ([]domain.ProjectPanelEntity, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, source, source_id, kind, COALESCE(title, ''), COALESCE(url, ''),
		       state, COALESCE(classification_rationale, ''), created_at, last_polled_at
		FROM entities
		WHERE org_id = $1 AND project_id = $2 AND state = 'active'
		ORDER BY last_polled_at DESC NULLS LAST
	`, orgID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.ProjectPanelEntity{}
	for rows.Next() {
		var e domain.ProjectPanelEntity
		if err := rows.Scan(&e.ID, &e.Source, &e.SourceID, &e.Kind, &e.Title, &e.URL,
			&e.State, &e.ClassificationRationale, &e.CreatedAt, &e.LastPolledAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- Mutation ---

func (s *entityStore) FindOrCreate(ctx context.Context, orgID, source, sourceID, kind, title, url string) (*domain.Entity, bool, error) {
	existing, err := s.GetBySource(ctx, orgID, source, sourceID)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, false, nil
	}

	id := uuid.New().String()
	now := time.Now()
	_, err = s.q.ExecContext(ctx, `
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, state, created_at, last_polled_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'active', $8, $9)
	`, id, orgID, source, sourceID, kind, title, url, now, now)
	if err != nil {
		// Concurrent first-discovery race: the unique key
		// (org_id, source, source_id) just fired. Re-read so both
		// callers see a populated entity. If the re-read also
		// fails, surface the original error.
		existing, err2 := s.GetBySource(ctx, orgID, source, sourceID)
		if err2 == nil && existing != nil {
			return existing, false, nil
		}
		return nil, false, err
	}

	return &domain.Entity{
		ID:           id,
		Source:       source,
		SourceID:     sourceID,
		Kind:         kind,
		Title:        title,
		URL:          url,
		State:        "active",
		CreatedAt:    now,
		LastPolledAt: &now,
	}, true, nil
}

func (s *entityStore) UpdateSnapshot(ctx context.Context, orgID, id, snapshotJSON string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE entities
		SET snapshot_json = $1::jsonb, last_polled_at = $2
		WHERE org_id = $3 AND id = $4
	`, snapshotJSON, time.Now(), orgID, id)
	return err
}

func (s *entityStore) PatchSnapshot(ctx context.Context, orgID, id, snapshotJSON string) error {
	_, err := s.q.ExecContext(ctx, `UPDATE entities SET snapshot_json = $1::jsonb WHERE org_id = $2 AND id = $3`, snapshotJSON, orgID, id)
	return err
}

func (s *entityStore) UpdateTitle(ctx context.Context, orgID, id, title string) error {
	_, err := s.q.ExecContext(ctx, `UPDATE entities SET title = $1 WHERE org_id = $2 AND id = $3`, title, orgID, id)
	return err
}

func (s *entityStore) UpdateDescription(ctx context.Context, orgID, id, description string) error {
	_, err := s.q.ExecContext(ctx, `UPDATE entities SET description = $1 WHERE org_id = $2 AND id = $3`, description, orgID, id)
	return err
}

func (s *entityStore) AssignProject(ctx context.Context, orgID, id string, projectID *string, rationale string) error {
	var projectArg any
	if projectID != nil && *projectID != "" {
		projectArg = *projectID
	}
	var rationaleArg any
	if rationale != "" {
		rationaleArg = rationale
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE entities
		SET project_id = $1,
		    classification_rationale = $2,
		    classified_at = now()
		WHERE org_id = $3 AND id = $4
	`, projectArg, rationaleArg, orgID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var exists int
		err := s.q.QueryRowContext(ctx, `SELECT 1 FROM entities WHERE org_id = $1 AND id = $2`, orgID, id).Scan(&exists)
		if err == sql.ErrNoRows {
			return sql.ErrNoRows
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *entityStore) MarkClosed(ctx context.Context, orgID, id string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE entities SET state = 'closed', closed_at = $1 WHERE org_id = $2 AND id = $3
	`, time.Now(), orgID, id)
	return err
}

func (s *entityStore) Close(ctx context.Context, orgID, id string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE entities SET state = 'closed', closed_at = $1 WHERE org_id = $2 AND id = $3 AND state = 'active'
	`, time.Now(), orgID, id)
	return err
}

func (s *entityStore) Reactivate(ctx context.Context, orgID, id string) (bool, error) {
	res, err := s.q.ExecContext(ctx, `
		UPDATE entities SET state = 'active', closed_at = NULL WHERE org_id = $1 AND id = $2 AND state = 'closed'
	`, orgID, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// pgUUIDArray formats a Go string slice as a Postgres uuid[] literal
// for binding through a single $N parameter. The pgx stdlib driver
// accepts the textual array form for typed-array columns. Quoting
// rules: ids are uuid-shaped (no commas, braces, or backslashes), so
// raw element values are safe to emit inside the {…} envelope without
// escaping.
func pgUUIDArray(ids []string) string {
	if len(ids) == 0 {
		return "{}"
	}
	return "{" + strings.Join(ids, ",") + "}"
}

func scanEntityRow(row *sql.Row) (*domain.Entity, error) {
	var e domain.Entity
	var projectID sql.NullString
	err := row.Scan(&e.ID, &e.Source, &e.SourceID, &e.Kind, &e.Title, &e.URL,
		&e.SnapshotJSON, &e.Description, &e.State, &projectID, &e.ClassificationRationale,
		&e.CreatedAt, &e.LastPolledAt, &e.ClosedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if projectID.Valid {
		e.ProjectID = &projectID.String
	}
	return &e, nil
}

func scanEntityList(rows *sql.Rows) ([]domain.Entity, error) {
	out := []domain.Entity{}
	for rows.Next() {
		var e domain.Entity
		var projectID sql.NullString
		if err := rows.Scan(&e.ID, &e.Source, &e.SourceID, &e.Kind, &e.Title, &e.URL,
			&e.SnapshotJSON, &e.Description, &e.State, &projectID, &e.ClassificationRationale,
			&e.CreatedAt, &e.LastPolledAt, &e.ClosedAt); err != nil {
			return nil, err
		}
		if projectID.Valid {
			e.ProjectID = &projectID.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
