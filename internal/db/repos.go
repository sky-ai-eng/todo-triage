package db

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/sky-ai-eng/todo-tinder/internal/domain"
)

// UpsertRepoProfile inserts or updates a repo profile.
// On conflict it updates all metadata fields while preserving the row identity.
func UpsertRepoProfile(database *sql.DB, p domain.RepoProfile) error {
	_, err := database.Exec(`
		INSERT INTO repo_profiles (id, owner, repo, description, has_readme, has_claude_md, has_agents_md, profile_text, profiled_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET
			description  = excluded.description,
			has_readme   = excluded.has_readme,
			has_claude_md = excluded.has_claude_md,
			has_agents_md = excluded.has_agents_md,
			profile_text = excluded.profile_text,
			profiled_at  = excluded.profiled_at,
			updated_at   = datetime('now')
	`,
		p.ID, p.Owner, p.Repo,
		nullIfEmpty(p.Description),
		p.HasReadme, p.HasClaudeMd, p.HasAgentsMd,
		nullIfEmpty(p.ProfileText),
		p.ProfiledAt,
	)
	return err
}

// GetRepoProfilesWithContent returns all repo profiles that have a non-null profile_text.
func GetRepoProfilesWithContent(database *sql.DB) ([]domain.RepoProfile, error) {
	rows, err := database.Query(`
		SELECT id, owner, repo, description, has_readme, has_claude_md, has_agents_md, profile_text
		FROM repo_profiles
		WHERE profile_text IS NOT NULL AND profile_text != ''
		ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var profiles []domain.RepoProfile
	for rows.Next() {
		var p domain.RepoProfile
		var description sql.NullString
		var profileText sql.NullString
		if err := rows.Scan(&p.ID, &p.Owner, &p.Repo, &description, &p.HasReadme, &p.HasClaudeMd, &p.HasAgentsMd, &profileText); err != nil {
			return nil, err
		}
		p.Description = description.String
		p.ProfileText = profileText.String
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}

// UpdateTaskRepoMatch stores the repo match results for a task.
func UpdateTaskRepoMatch(database *sql.DB, taskID string, repos []string, blockedReason string) error {
	reposJSON, err := json.Marshal(repos)
	if err != nil {
		return fmt.Errorf("marshal repos: %w", err)
	}
	_, err = database.Exec(`
		UPDATE tasks SET matched_repos = ?, blocked_reason = ? WHERE id = ?
	`, string(reposJSON), nullIfEmpty(blockedReason), taskID)
	return err
}
