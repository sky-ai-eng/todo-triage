package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// projectStore is the Postgres impl of db.ProjectStore. Wired against
// the app pool in postgres.New: every consumer is request-equivalent
// (projects handler, curator, backfill, project_entities) or runs in
// a startup goroutine that already operates within the org's identity
// scope. RLS policies projects_{select,insert,update,delete} gate
// every statement; org_id defense-in-depth fires alongside.
//
// pinned_repos is jsonb in Postgres vs text-JSON in SQLite. The
// jsonb cast happens at the placeholder level ($N::jsonb) — callers
// always pass a marshalled string, and the impl coerces.
type projectStore struct{ q queryer }

func newProjectStore(q queryer) db.ProjectStore { return &projectStore{q: q} }

var _ db.ProjectStore = (*projectStore)(nil)

func (s *projectStore) Create(ctx context.Context, orgID, teamID string, p domain.Project) (string, error) {
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

	// teamID: required, no synthesis. Projects are user-driven writes
	// and the human picks which team owns the project at the Create
	// UI (SKY-294). A "first of caller's teams" or "any team in org"
	// fallback would either silently attach to the wrong team or
	// collide with projects_insert RLS (tf.user_in_team(team_id)).
	// The handler ships runmode.LocalDefaultTeamID today; the
	// sentinel filter converts that to empty, which then trips the
	// explicit guard below. Post-D9, the handler threads a real team
	// from request context.
	teamBind := teamID
	if teamBind == runmode.LocalDefaultTeamID {
		teamBind = ""
	}
	if teamBind == "" {
		return "", fmt.Errorf("project store: team_id required for Postgres Create (handler must thread the user-selected team from request context; the SQLite-only LocalDefaultTeamID sentinel does not satisfy the projects_insert RLS policy)")
	}

	// creator_user_id: pulled from tf.current_user_id() set by WithTx
	// claims — same pattern every other app-pool store uses
	// (event_handlers, swipes, chains, tasks, prompts). Org-owner
	// fallback covers the pgtest admin-pool path (BYPASSRLS, no
	// claims set); in production multi-mode under tf_app, claims are
	// always set and the COALESCE stops at the first branch.
	_, err = s.q.ExecContext(ctx, `
		INSERT INTO projects
		  (id, org_id, creator_user_id, team_id, name, description,
		   curator_session_id, pinned_repos,
		   jira_project_key, linear_project_key, spec_authorship_prompt_id)
		VALUES
		  ($1, $2,
		   COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
		   $3::uuid,
		   $4, $5, NULLIF($6, ''), $7::jsonb,
		   NULLIF($8, ''), NULLIF($9, ''), NULLIF($10, ''))
	`,
		id, orgID, teamBind,
		p.Name, p.Description,
		p.CuratorSessionID, string(pinnedJSON),
		p.JiraProjectKey, p.LinearProjectKey, p.SpecAuthorshipPromptID,
	)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *projectStore) Get(ctx context.Context, orgID, id string) (*domain.Project, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT id, name, description, curator_session_id, pinned_repos,
		       jira_project_key, linear_project_key, spec_authorship_prompt_id,
		       created_at, updated_at
		FROM projects
		WHERE org_id = $1 AND id = $2
	`, orgID, id)
	return scanProjectRow(row)
}

func (s *projectStore) List(ctx context.Context, orgID string) ([]domain.Project, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, name, description, curator_session_id, pinned_repos,
		       jira_project_key, linear_project_key, spec_authorship_prompt_id,
		       created_at, updated_at
		FROM projects
		WHERE org_id = $1
		ORDER BY LOWER(name) ASC
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Project{}
	for rows.Next() {
		p, err := scanProjectRow(rows)
		if err != nil {
			return nil, err
		}
		if p != nil {
			out = append(out, *p)
		}
	}
	return out, rows.Err()
}

func (s *projectStore) Update(ctx context.Context, orgID string, p domain.Project) error {
	pinned := p.PinnedRepos
	if pinned == nil {
		pinned = []string{}
	}
	pinnedJSON, err := json.Marshal(pinned)
	if err != nil {
		return fmt.Errorf("marshal pinned_repos: %w", err)
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE projects
		SET name = $1, description = $2,
		    curator_session_id = NULLIF($3, ''),
		    pinned_repos = $4::jsonb,
		    jira_project_key = NULLIF($5, ''),
		    linear_project_key = NULLIF($6, ''),
		    spec_authorship_prompt_id = NULLIF($7, ''),
		    updated_at = now()
		WHERE org_id = $8 AND id = $9
	`,
		p.Name, p.Description,
		p.CuratorSessionID, string(pinnedJSON),
		p.JiraProjectKey, p.LinearProjectKey, p.SpecAuthorshipPromptID,
		orgID, p.ID,
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

func (s *projectStore) Delete(ctx context.Context, orgID, id string) error {
	res, err := s.q.ExecContext(ctx, `DELETE FROM projects WHERE org_id = $1 AND id = $2`, orgID, id)
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

// scanProjectRow reads a SELECT … FROM projects row into a
// *domain.Project. (nil, nil) on sql.ErrNoRows so callers can map
// missing rows to a 404 without a sentinel comparison.
func scanProjectRow(row interface {
	Scan(dest ...any) error
}) (*domain.Project, error) {
	var (
		p            domain.Project
		sessionID    sql.NullString
		jiraKey      sql.NullString
		linearKey    sql.NullString
		specPromptID sql.NullString
		pinnedJSON   []byte
	)
	err := row.Scan(
		&p.ID, &p.Name, &p.Description, &sessionID, &pinnedJSON,
		&jiraKey, &linearKey, &specPromptID,
		&p.CreatedAt, &p.UpdatedAt,
	)
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
	if len(pinnedJSON) == 0 {
		p.PinnedRepos = []string{}
	} else if err := json.Unmarshal(pinnedJSON, &p.PinnedRepos); err != nil {
		return nil, fmt.Errorf("unmarshal pinned_repos: %w", err)
	}
	if p.PinnedRepos == nil {
		p.PinnedRepos = []string{}
	}
	return &p, nil
}
