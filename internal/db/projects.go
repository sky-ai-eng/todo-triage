package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// CreateProject inserts a new project and returns its id. PinnedRepos
// is JSON-marshalled into the pinned_repos column.
//
// ID handling: if p.ID is non-empty it's used as-is; otherwise a uuid
// is generated. The HTTP create handler always passes an empty ID, so
// the API surface is server-generation-only. Internal callers (tests,
// seed scripts) can pre-set an ID when deterministic IDs are useful;
// no security concern because there is no client path that supplies
// an arbitrary ID. The returned id is the one actually persisted.
//
// Empty PinnedRepos serializes as `[]` (not null) — matches the DB
// default and keeps the JSON round-trip predictable.
//
// Local-mode only: team_id is pinned to LocalDefaultTeamID. SKY-253
// D9 (org-scoping pass) will replace this raw-SQL function with a
// ProjectStore.Create(ctx, orgID, teamID, ...) that derives team_id
// from the request-scoped session context, with SQLite + Postgres
// impls. Until then, calling this in multi mode would silently attach
// the row to the wrong team — guarded only by main.go's
// log.Fatalf on TF_MODE=multi at startup.
func CreateProject(database *sql.DB, p domain.Project) (string, error) {
	id := p.ID
	if id == "" {
		id = uuid.New().String()
	}
	pinned := p.PinnedRepos
	if pinned == nil {
		pinned = []string{}
	}
	pinnedJSON, err := json.Marshal(pinned)
	if err != nil {
		return "", fmt.Errorf("marshal pinned_repos: %w", err)
	}
	now := time.Now().UTC()
	// team_id pinned to LocalDefaultTeamID so the
	// projects_team_visibility_requires_team CHECK passes for the
	// default visibility='team'.
	_, err = database.Exec(`
		INSERT INTO projects (id, name, description, curator_session_id, pinned_repos, jira_project_key, linear_project_key, spec_authorship_prompt_id, team_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		id, p.Name, p.Description,
		nullIfEmpty(p.CuratorSessionID), string(pinnedJSON),
		nullIfEmpty(p.JiraProjectKey), nullIfEmpty(p.LinearProjectKey),
		nullIfEmpty(p.SpecAuthorshipPromptID),
		runmode.LocalDefaultTeamID,
		now, now,
	)
	if err != nil {
		return "", err
	}
	return id, nil
}

// GetProject returns a project by id, or (nil, nil) if not found.
func GetProject(database *sql.DB, id string) (*domain.Project, error) {
	row := database.QueryRow(`
		SELECT id, name, description, curator_session_id, pinned_repos, jira_project_key, linear_project_key, spec_authorship_prompt_id, created_at, updated_at
		FROM projects WHERE id = ?
	`, id)
	return scanProject(row)
}

// ListProjects returns all projects ordered by name (case-insensitive).
// No pagination — projects are user-curated and counts stay small (≤100
// in any plausible install).
func ListProjects(database *sql.DB) ([]domain.Project, error) {
	rows, err := database.Query(`
		SELECT id, name, description, curator_session_id, pinned_repos, jira_project_key, linear_project_key, spec_authorship_prompt_id, created_at, updated_at
		FROM projects ORDER BY LOWER(name) ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		if p != nil {
			out = append(out, *p)
		}
	}
	return out, rows.Err()
}

// UpdateProject writes the full row from p (caller is responsible
// for merging partial PATCH input over an existing row first).
// updated_at is stamped server-side. created_at is preserved.
func UpdateProject(database *sql.DB, p domain.Project) error {
	pinned := p.PinnedRepos
	if pinned == nil {
		pinned = []string{}
	}
	pinnedJSON, err := json.Marshal(pinned)
	if err != nil {
		return fmt.Errorf("marshal pinned_repos: %w", err)
	}
	now := time.Now().UTC()
	res, err := database.Exec(`
		UPDATE projects
		SET name = ?, description = ?,
		    curator_session_id = ?, pinned_repos = ?,
		    jira_project_key = ?, linear_project_key = ?,
		    spec_authorship_prompt_id = ?,
		    updated_at = ?
		WHERE id = ?
	`,
		p.Name, p.Description,
		nullIfEmpty(p.CuratorSessionID), string(pinnedJSON),
		nullIfEmpty(p.JiraProjectKey), nullIfEmpty(p.LinearProjectKey),
		nullIfEmpty(p.SpecAuthorshipPromptID),
		now, p.ID,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteProject removes the row. The entities.project_id FK is
// declared ON DELETE SET NULL, so any tagged entities become
// untagged automatically — caller does not need to clear them
// first. Returns sql.ErrNoRows if the project doesn't exist so
// the handler can map that to 404.
//
// On-disk knowledge artifacts (`~/.triagefactory/projects/<id>/`)
// are NOT removed here — the handler owns that to keep this layer
// pure DB. Same split as the rest of the codebase (e.g. takeover
// directory cleanup lives in spawner, not in db).
func DeleteProject(database *sql.DB, id string) error {
	res, err := database.Exec(`DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// scanProject is the shared row reader for QueryRow / Query rows.
// Returns (nil, nil) on sql.ErrNoRows so callers can distinguish
// "missing" from "error" without a sentinel comparison.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanProject(row rowScanner) (*domain.Project, error) {
	var (
		p            domain.Project
		sessionID    sql.NullString
		jiraKey      sql.NullString
		linearKey    sql.NullString
		specPromptID sql.NullString
		pinnedJSON   string
		createdAt    time.Time
		updatedAt    time.Time
	)
	err := row.Scan(&p.ID, &p.Name, &p.Description, &sessionID, &pinnedJSON, &jiraKey, &linearKey, &specPromptID, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.CuratorSessionID = sessionID.String
	p.JiraProjectKey = jiraKey.String
	p.LinearProjectKey = linearKey.String
	p.SpecAuthorshipPromptID = specPromptID.String
	p.CreatedAt = createdAt
	p.UpdatedAt = updatedAt
	if pinnedJSON == "" {
		p.PinnedRepos = []string{}
	} else if err := json.Unmarshal([]byte(pinnedJSON), &p.PinnedRepos); err != nil {
		return nil, fmt.Errorf("unmarshal pinned_repos: %w", err)
	}
	if p.PinnedRepos == nil {
		p.PinnedRepos = []string{}
	}
	return &p, nil
}
