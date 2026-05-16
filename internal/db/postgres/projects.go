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

func (s *projectStore) Create(ctx context.Context, orgID, userID, teamID string, p domain.Project) (string, error) {
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

	// Sentinel-leak handling: the handler/router still passes
	// runmode.LocalDefault{User,Team}ID until D9 retrofits handler-
	// level claims. Those sentinels have no FK target in a multi-mode
	// users/teams table, so binding them directly would trip
	// creator_user_id_fkey / team_id_fkey on every Create. Normalize
	// to empty before binding so NULLIF collapses to NULL and the
	// COALESCE walks to a real fallback. Same shape as the SKY-289
	// pending_firings filter.
	creatorBind := userID
	if creatorBind == runmode.LocalDefaultUserID {
		creatorBind = ""
	}
	teamBind := teamID
	if teamBind == runmode.LocalDefaultTeamID {
		teamBind = ""
	}

	// creator_user_id resolution: caller-supplied → tf.current_user_id()
	// → org owner. team_id resolution: caller-supplied → caller's first
	// team in the org (deterministic by created_at) — the CHECK refuses
	// NULL when visibility='team', and the schema default for visibility
	// is 'team', so a fallback that lands NULL would fail. The two-arg
	// COALESCE is enough today; D9 will replace these COALESCE chains
	// with bare $N once handler-level claims pin the values.
	_, err = s.q.ExecContext(ctx, `
		INSERT INTO projects
		  (id, org_id, creator_user_id, team_id, name, description,
		   curator_session_id, pinned_repos,
		   jira_project_key, linear_project_key, spec_authorship_prompt_id)
		VALUES
		  ($1, $2,
		   COALESCE(NULLIF($3, '')::uuid, tf.current_user_id(),
		            (SELECT owner_user_id FROM orgs WHERE id = $2)),
		   COALESCE(NULLIF($4, '')::uuid,
		            (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1)),
		   $5, $6, NULLIF($7, ''), $8::jsonb,
		   NULLIF($9, ''), NULLIF($10, ''), NULLIF($11, ''))
	`,
		id, orgID, creatorBind, teamBind,
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
