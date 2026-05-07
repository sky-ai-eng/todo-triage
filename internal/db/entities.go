package db

import (
	"database/sql"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// FindOrCreateEntity returns the entity for (source, source_id), creating it
// if it doesn't exist. Returns (entity, created, error).
func FindOrCreateEntity(db *sql.DB, source, sourceID, kind, title, url string) (*domain.Entity, bool, error) {
	// Try to find existing first (common path on subsequent polls).
	existing, err := GetEntityBySource(db, source, sourceID)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, false, nil
	}

	// Create new entity.
	id := uuid.New().String()
	now := time.Now()
	_, err = db.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, state, created_at, last_polled_at)
		VALUES (?, ?, ?, ?, ?, ?, 'active', ?, ?)
	`, id, source, sourceID, kind, title, url, now, now)
	if err != nil {
		// Race condition: another goroutine may have created it between our
		// SELECT and INSERT. Re-read.
		existing, err2 := GetEntityBySource(db, source, sourceID)
		if err2 == nil && existing != nil {
			return existing, false, nil
		}
		return nil, false, err
	}

	entity := &domain.Entity{
		ID:           id,
		Source:       source,
		SourceID:     sourceID,
		Kind:         kind,
		Title:        title,
		URL:          url,
		State:        "active",
		CreatedAt:    now,
		LastPolledAt: &now,
	}
	return entity, true, nil
}

// MarkEntityClosed sets state='closed' and closed_at=now without the task
// cascade. Used at discovery time when the initial snapshot is already terminal
// (merged/closed PR, completed Jira issue) — the entity was never active, so
// there are no tasks to cascade-close.
func MarkEntityClosed(db *sql.DB, entityID string) error {
	_, err := db.Exec(`
		UPDATE entities SET state = 'closed', closed_at = ? WHERE id = ?
	`, time.Now(), entityID)
	return err
}

// ReactivateEntity transitions a closed entity back to active. Used when a
// previously-terminal entity reappears as open (e.g., reopened PR, reopened
// Jira issue). Returns true if the entity was actually reactivated.
func ReactivateEntity(db *sql.DB, entityID string) (bool, error) {
	result, err := db.Exec(`
		UPDATE entities SET state = 'active', closed_at = NULL WHERE id = ? AND state = 'closed'
	`, entityID)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// UpdateEntitySnapshot updates the snapshot_json and last_polled_at for an entity.
func UpdateEntitySnapshot(db *sql.DB, entityID, snapshotJSON string) error {
	_, err := db.Exec(`
		UPDATE entities SET snapshot_json = ?, last_polled_at = ? WHERE id = ?
	`, snapshotJSON, time.Now(), entityID)
	return err
}

// UpdateEntityTitle updates the title for an entity (e.g., PR title changed).
func UpdateEntityTitle(db *sql.DB, entityID, title string) error {
	_, err := db.Exec(`UPDATE entities SET title = ? WHERE id = ?`, title, entityID)
	return err
}

// UpdateEntityDescription updates the flattened description body for an entity.
// Description lives outside snapshot_json because it's large and not part of
// the diff scope — keeping it off the snapshot means diff reads stay small
// even on tickets with multi-KB bodies.
func UpdateEntityDescription(db *sql.DB, entityID, description string) error {
	_, err := db.Exec(`UPDATE entities SET description = ? WHERE id = ?`, description, entityID)
	return err
}

// AssignEntityProject sets entities.project_id (NULL if projectID is nil
// or "") and stamps classified_at = CURRENT_TIMESTAMP so the project
// classifier (SKY-220) won't re-fire on this entity. Both the auto
// classifier and the project-creation backfill popup write through this
// helper — a popup-driven assignment is also a "final answer" from the
// classifier's perspective.
//
// rationale is the highest-scoring project's one-sentence explanation
// (winner OR runner-up), preserved on the row so the UI can surface
// "why this match" or "closest match was X at score N." Empty string
// is acceptable for the popup path, where the human is the rationale.
//
// Returns sql.ErrNoRows when the UPDATE matches no row — i.e. the
// entity id doesn't exist. Callers that ingest user input (e.g. the
// backfill HTTP handler) need this signal to report per-row failures
// rather than silently counting bogus ids as applied.
func AssignEntityProject(database *sql.DB, entityID string, projectID *string, rationale string) error {
	var arg any
	if projectID != nil && *projectID != "" {
		arg = *projectID
	} else {
		arg = nil
	}
	var rationaleArg any
	if rationale != "" {
		rationaleArg = rationale
	} else {
		rationaleArg = nil
	}
	res, err := database.Exec(`
		UPDATE entities
		SET project_id = ?,
		    classification_rationale = ?,
		    classified_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, arg, rationaleArg, entityID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var exists int
		err := database.QueryRow(`SELECT 1 FROM entities WHERE id = ?`, entityID).Scan(&exists)
		if err == sql.ErrNoRows {
			return sql.ErrNoRows
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ListUnclassifiedEntities returns active entities that haven't been
// classified yet — i.e. project_id IS NULL AND classified_at IS NULL.
// Once an entity has been processed by the classifier (with any outcome,
// including below-threshold) classified_at is set and the entity won't
// resurface here. Re-assignment via the backfill popup also sets
// classified_at, so a popup-assigned entity stays out too.
func ListUnclassifiedEntities(database *sql.DB) ([]domain.Entity, error) {
	rows, err := database.Query(`
		SELECT id, source, source_id, kind, COALESCE(title, ''), COALESCE(url, ''),
		       COALESCE(snapshot_json, ''), COALESCE(description, ''), state, project_id, COALESCE(classification_rationale, ''), created_at, last_polled_at, closed_at
		FROM entities
		WHERE project_id IS NULL AND classified_at IS NULL AND state = 'active'
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Entity
	for rows.Next() {
		var e domain.Entity
		var projectID sql.NullString
		if err := rows.Scan(&e.ID, &e.Source, &e.SourceID, &e.Kind, &e.Title, &e.URL,
			&e.SnapshotJSON, &e.Description, &e.State, &projectID, &e.ClassificationRationale, &e.CreatedAt, &e.LastPolledAt, &e.ClosedAt); err != nil {
			return nil, err
		}
		if projectID.Valid {
			e.ProjectID = &projectID.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CloseEntity sets state='closed' and closed_at=now. Called by the entity
// lifecycle handler when an entity-terminating event fires.
func CloseEntity(db *sql.DB, entityID string) error {
	_, err := db.Exec(`
		UPDATE entities SET state = 'closed', closed_at = ? WHERE id = ? AND state = 'active'
	`, time.Now(), entityID)
	return err
}

// GetEntity returns an entity by ID, or nil if not found.
func GetEntity(db *sql.DB, id string) (*domain.Entity, error) {
	row := db.QueryRow(`
		SELECT id, source, source_id, kind, COALESCE(title, ''), COALESCE(url, ''),
		       COALESCE(snapshot_json, ''), COALESCE(description, ''), state, project_id, COALESCE(classification_rationale, ''), created_at, last_polled_at, closed_at
		FROM entities WHERE id = ?
	`, id)
	return scanEntity(row)
}

// GetEntityBySource returns an entity by (source, source_id), or nil if not found.
func GetEntityBySource(db *sql.DB, source, sourceID string) (*domain.Entity, error) {
	row := db.QueryRow(`
		SELECT id, source, source_id, kind, COALESCE(title, ''), COALESCE(url, ''),
		       COALESCE(snapshot_json, ''), COALESCE(description, ''), state, project_id, COALESCE(classification_rationale, ''), created_at, last_polled_at, closed_at
		FROM entities WHERE source = ? AND source_id = ?
	`, source, sourceID)
	return scanEntity(row)
}

// entityIDInChunkSize caps the number of `?` placeholders per batched
// WHERE id IN (...) query so we stay well under SQLite's default
// SQLITE_LIMIT_VARIABLE_NUMBER (999). Chunking runs multiple round-trips
// but keeps the query schema compatible with the default build — the
// scorer's entity set can easily exceed 1k tasks on large repos.
const entityIDInChunkSize = 500

// GetEntityDescriptions returns the flattened description body for each of
// the given entity IDs as a map keyed by entity ID. Empty descriptions are
// omitted. Used by the scorer to enrich TaskInput without dragging the
// full snapshot_json through the call. Dedupes IDs (multiple tasks can
// share an entity — e.g. ci_failed + new_commits on the same PR) and
// chunks the IN clause to respect SQLite's variable limit.
func GetEntityDescriptions(database *sql.DB, ids []string) (map[string]string, error) {
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

	for start := 0; start < len(unique); start += entityIDInChunkSize {
		end := start + entityIDInChunkSize
		if end > len(unique) {
			end = len(unique)
		}
		chunk := unique[start:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args[i] = id
		}
		query := `SELECT id, COALESCE(description, '') FROM entities WHERE id IN (` +
			strings.Join(placeholders, ",") + `)`
		rows, err := database.Query(query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, desc string
			if err := rows.Scan(&id, &desc); err != nil {
				rows.Close()
				return nil, err
			}
			if desc != "" {
				out[id] = desc
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	return out, nil
}

// ProjectPanelEntity is the trimmed entity shape the SKY-238 entities
// panel needs — no snapshot_json, no description, no project_id (the
// caller already knows which project this is for). Kept narrow so a
// project with many entities (or any with multi-KB snapshots) doesn't
// pull blobs the panel never renders.
type ProjectPanelEntity struct {
	ID                      string
	Source                  string
	SourceID                string
	Kind                    string
	Title                   string
	URL                     string
	State                   string
	ClassificationRationale string
	CreatedAt               time.Time
	LastPolledAt            *time.Time
}

// ListProjectPanelEntities returns active entities assigned to the
// given project, ordered by last_polled_at DESC so the most recently
// updated entity bubbles to the top. NULL last_polled_at sorts last —
// fresh-discovered entities haven't been polled yet but they're rare
// and the ordering is best-effort.
//
// Trimmed-column scan — see ProjectPanelEntity. The general
// scanEntity / ListActiveEntities path pulls snapshot_json +
// description, which is wasteful for the list-view payload.
func ListProjectPanelEntities(db *sql.DB, projectID string) ([]ProjectPanelEntity, error) {
	rows, err := db.Query(`
		SELECT id, source, source_id, kind, COALESCE(title, ''), COALESCE(url, ''),
		       state, COALESCE(classification_rationale, ''), created_at, last_polled_at
		FROM entities
		WHERE project_id = ? AND state = 'active'
		ORDER BY last_polled_at DESC NULLS LAST
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProjectPanelEntity
	for rows.Next() {
		var e ProjectPanelEntity
		if err := rows.Scan(&e.ID, &e.Source, &e.SourceID, &e.Kind, &e.Title, &e.URL,
			&e.State, &e.ClassificationRationale, &e.CreatedAt, &e.LastPolledAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListActiveEntities returns all entities with state='active' for a given source.
func ListActiveEntities(db *sql.DB, source string) ([]domain.Entity, error) {
	rows, err := db.Query(`
		SELECT id, source, source_id, kind, COALESCE(title, ''), COALESCE(url, ''),
		       COALESCE(snapshot_json, ''), COALESCE(description, ''), state, project_id, COALESCE(classification_rationale, ''), created_at, last_polled_at, closed_at
		FROM entities WHERE source = ? AND state = 'active'
		ORDER BY last_polled_at ASC
	`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entities []domain.Entity
	for rows.Next() {
		var e domain.Entity
		var projectID sql.NullString
		if err := rows.Scan(&e.ID, &e.Source, &e.SourceID, &e.Kind, &e.Title, &e.URL,
			&e.SnapshotJSON, &e.Description, &e.State, &projectID, &e.ClassificationRationale, &e.CreatedAt, &e.LastPolledAt, &e.ClosedAt); err != nil {
			return nil, err
		}
		if projectID.Valid {
			e.ProjectID = &projectID.String
		}
		entities = append(entities, e)
	}
	return entities, rows.Err()
}

func scanEntity(row *sql.Row) (*domain.Entity, error) {
	var e domain.Entity
	var projectID sql.NullString
	err := row.Scan(&e.ID, &e.Source, &e.SourceID, &e.Kind, &e.Title, &e.URL,
		&e.SnapshotJSON, &e.Description, &e.State, &projectID, &e.ClassificationRationale, &e.CreatedAt, &e.LastPolledAt, &e.ClosedAt)
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
