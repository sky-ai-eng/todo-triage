package db

import (
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// MarkScoring sets scoring_status = 'in_progress' for the given task IDs.
// Runs in a transaction so either all tasks are marked or none — prevents
// partial updates that leave some tasks stuck in 'in_progress' on error.
func MarkScoring(database *sql.DB, taskIDs []string) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`UPDATE tasks SET scoring_status = 'in_progress' WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, id := range taskIDs {
		if _, err := stmt.Exec(id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ResetScoringToPending flips scoring_status back to 'pending' for the given
// task IDs. Used when a scoring batch failed so the tasks are retried on the
// next cycle — without this, MarkScoring would have left them stuck in
// 'in_progress' (UnscoredTasks only picks up 'pending') and they'd never
// be rescored. Runs in a transaction for atomicity.
func ResetScoringToPending(database *sql.DB, taskIDs []string) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`UPDATE tasks SET scoring_status = 'pending' WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, id := range taskIDs {
		if _, err := stmt.Exec(id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpdateTaskScores applies AI-generated scores and summaries to tasks,
// and sets scoring_status = 'scored'.
func UpdateTaskScores(database *sql.DB, updates []domain.TaskScoreUpdate) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		UPDATE tasks
		SET priority_score = ?, autonomy_suitability = ?, ai_summary = ?,
		    priority_reasoning = ?, scoring_status = 'scored'
		WHERE id = ?
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, u := range updates {
		_, err := stmt.Exec(u.PriorityScore, u.AutonomySuitability, u.Summary, u.PriorityReasoning, u.ID)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// UnscoredTasks returns queued tasks that haven't been scored yet.
func UnscoredTasks(database *sql.DB) ([]domain.Task, error) {
	return queryTasks(database, `
		SELECT `+taskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.status = 'queued' AND t.scoring_status = 'pending'
		ORDER BY t.created_at DESC
	`)
}
