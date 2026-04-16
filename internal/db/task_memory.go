package db

import (
	"database/sql"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SaveRunMemory inserts a run_memory row. The entity_id is denormalized from
// the run→task→entity chain for fast entity-scoped queries.
func SaveRunMemory(database *sql.DB, m domain.TaskMemory) error {
	_, err := database.Exec(`
		INSERT INTO run_memory (id, run_id, entity_id, content, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, m.ID, m.RunID, m.EntityID, m.Content, m.CreatedAt)
	return err
}

// GetMemoriesForEntity returns all memories across all runs on this entity
// (and linked entities via entity_links), oldest first. Used by the spawner
// to materialize prior context into a fresh worktree.
func GetMemoriesForEntity(database *sql.DB, entityID string) ([]domain.TaskMemory, error) {
	rows, err := database.Query(`
		WITH related AS (
			SELECT id FROM entities WHERE id = ?
			UNION
			SELECT to_entity_id FROM entity_links WHERE from_entity_id = ?
			UNION
			SELECT from_entity_id FROM entity_links WHERE to_entity_id = ?
		)
		SELECT rm.id, rm.run_id, rm.entity_id, rm.content, rm.created_at
		FROM run_memory rm
		WHERE rm.entity_id IN (SELECT id FROM related)
		ORDER BY rm.created_at ASC
	`, entityID, entityID, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.TaskMemory
	for rows.Next() {
		var m domain.TaskMemory
		var createdAt time.Time
		if err := rows.Scan(&m.ID, &m.RunID, &m.EntityID, &m.Content, &createdAt); err != nil {
			return nil, err
		}
		m.CreatedAt = createdAt
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetRunMemory returns the single memory row for a run, or nil.
func GetRunMemory(database *sql.DB, runID string) (*domain.TaskMemory, error) {
	row := database.QueryRow(`
		SELECT id, run_id, entity_id, content, created_at
		FROM run_memory WHERE run_id = ?
	`, runID)

	var m domain.TaskMemory
	var createdAt time.Time
	err := row.Scan(&m.ID, &m.RunID, &m.EntityID, &m.Content, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.CreatedAt = createdAt
	return &m, nil
}
