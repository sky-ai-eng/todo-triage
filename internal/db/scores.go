package db

import (
	"database/sql"

	"github.com/sky-ai-eng/todo-tinder/internal/domain"
)

// UpdateTaskScores applies AI-generated scores and summaries to tasks.
func UpdateTaskScores(database *sql.DB, updates []domain.TaskScoreUpdate) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		UPDATE tasks
		SET priority_score = ?, agent_confidence = ?, ai_summary = ?, priority_reasoning = ?
		WHERE id = ?
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, u := range updates {
		_, err := stmt.Exec(u.PriorityScore, u.AgentConfidence, u.Summary, u.PriorityReasoning, u.ID)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// UnscoredTasks returns queued tasks that don't have an AI summary yet.
func UnscoredTasks(database *sql.DB) ([]domain.Task, error) {
	return queryTasks(database, `
		SELECT id, source, source_id, source_url, title, description, repo, author, labels, severity,
		       diff_additions, diff_deletions, files_changed, ci_status, relevance_reason, created_at, fetched_at, status, priority_score, ai_summary,
		       priority_reasoning, agent_confidence, snooze_until
		FROM tasks
		WHERE status = 'queued' AND ai_summary IS NULL
		ORDER BY created_at DESC
	`)
}
