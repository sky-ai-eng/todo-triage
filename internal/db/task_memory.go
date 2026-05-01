package db

import (
	"database/sql"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// humanFeedbackHeader marks the start of the human verdict in
// materialized memory. Stable so the next agent's prompt context can
// parse the boundary regardless of which run wrote which half.
// Future writers (SKY-205 / SKY-206) populate human_content; today
// every row has it NULL and this header never appears.
const humanFeedbackHeader = "## Human feedback (post-run)\n\n"

// humanFeedbackSeparator is the divider rendered when both halves of
// a memory row are populated — leading newlines and HR push the
// header onto its own visual block after the agent's text. The
// agent-empty + human-set case uses humanFeedbackHeader alone (no
// stray HR ahead of the only block of content).
const humanFeedbackSeparator = "\n\n---\n" + humanFeedbackHeader

// UpsertAgentMemory writes the agent-side memory row for a run. Called
// unconditionally from the spawner's memory-gate teardown — content
// that's empty or whitespace-only is the canonical "agent didn't
// comply with the gate" signal and is stored as SQL NULL. That gives
// downstream consumers (factory's memory_missing derivation) a single
// truth condition (`agent_content IS NULL`) for the noncompliance
// case while still preserving any meaningful surrounding whitespace
// the agent wrote when there's actual content (the original string is
// stored as-is, no trim).
//
// Idempotent on (run_id) via ON CONFLICT — re-running the gate after
// a retry overwrites agent_content but preserves the row's id,
// created_at, and (importantly for SKY-205) any human_content the
// user has already attached.
func UpsertAgentMemory(database *sql.DB, runID, entityID, content string) error {
	var agentContent any
	if strings.TrimSpace(content) != "" {
		agentContent = content
	}
	_, err := database.Exec(`
		INSERT INTO run_memory (id, run_id, entity_id, agent_content, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET agent_content = excluded.agent_content
	`, uuid.New().String(), runID, entityID, agentContent, time.Now().UTC())
	return err
}

// GetMemoriesForEntity returns all memories across all runs on this entity
// (and linked entities via entity_links), oldest first. Used by the spawner
// to materialize prior context into a fresh worktree.
//
// TaskMemory.Content is materialized: agent_content alone when no human
// feedback has been recorded (the common case in this PR), or the
// agent's text + a stable separator + the human's verdict when both
// are present. The next agent reads the separator as a parseable
// boundary; format is fixed for this purpose, so don't change it
// without also updating any briefing docs that teach agents how to
// read prior memory.
func GetMemoriesForEntity(database *sql.DB, entityID string) ([]domain.TaskMemory, error) {
	rows, err := database.Query(`
		WITH related AS (
			SELECT id FROM entities WHERE id = ?
			UNION
			SELECT to_entity_id FROM entity_links WHERE from_entity_id = ?
			UNION
			SELECT from_entity_id FROM entity_links WHERE to_entity_id = ?
		)
		SELECT rm.id, rm.run_id, rm.entity_id, rm.agent_content, rm.human_content, rm.created_at
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
		var agentContent, humanContent sql.NullString
		var createdAt time.Time
		if err := rows.Scan(&m.ID, &m.RunID, &m.EntityID, &agentContent, &humanContent, &createdAt); err != nil {
			return nil, err
		}
		m.Content = materializeMemory(agentContent.String, humanContent.String)
		m.CreatedAt = createdAt
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetRunMemory returns the single memory row for a run, or nil. Same
// materialization contract as GetMemoriesForEntity.
func GetRunMemory(database *sql.DB, runID string) (*domain.TaskMemory, error) {
	row := database.QueryRow(`
		SELECT id, run_id, entity_id, agent_content, human_content, created_at
		FROM run_memory WHERE run_id = ?
	`, runID)

	var m domain.TaskMemory
	var agentContent, humanContent sql.NullString
	var createdAt time.Time
	err := row.Scan(&m.ID, &m.RunID, &m.EntityID, &agentContent, &humanContent, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.Content = materializeMemory(agentContent.String, humanContent.String)
	m.CreatedAt = createdAt
	return &m, nil
}

func materializeMemory(agentContent, humanContent string) string {
	hasAgent := strings.TrimSpace(agentContent) != ""
	hasHuman := strings.TrimSpace(humanContent) != ""
	switch {
	case hasAgent && hasHuman:
		return agentContent + humanFeedbackSeparator + humanContent
	case hasHuman:
		// Agent-empty + human-set: render just the header + body, no
		// leading HR. The HR only makes sense as a divider between
		// two blocks; without an agent block it would just be visual
		// noise that the next agent has to skip past.
		return humanFeedbackHeader + humanContent
	default:
		return agentContent
	}
}
