package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// agentStore is the SQLite impl of db.AgentStore.
//
//   - assertLocalOrg at every method entry (one synthetic org locally),
//   - ctx propagation on every Exec/Query,
//   - UTC timestamps everywhere,
//   - INSERT OR IGNORE for Create with a deterministic id from
//     db.BootstrapAgentID(orgID) — local mode has no UNIQUE(org_id)
//     constraint (no org_id column), so idempotency is provided by the
//     stable id alone.
//
// Credential columns (github_app_installation_id, github_pat_user_id)
// stay NULL in local mode by design: the keychain is the source of
// truth for the user's PAT, and the agents row is metadata. The
// SetGitHubAppInstallation / SetGitHubPATUser methods still work — a
// caller can populate them — but the default install path leaves both
// empty.
type agentStore struct{ q queryer }

func newAgentStore(q queryer) db.AgentStore { return &agentStore{q: q} }

var _ db.AgentStore = (*agentStore)(nil)

const sqliteAgentColumns = `id, display_name, default_model, default_autonomy_suitability,
       github_app_installation_id, github_pat_user_id, jira_service_account_id,
       created_at, updated_at`

func (s *agentStore) GetForOrg(ctx context.Context, orgID string) (*domain.Agent, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// Local mode has at most one agents row; the deterministic id lets
	// us look it up directly without a (nonexistent) org_id column.
	row := s.q.QueryRowContext(ctx, `SELECT `+sqliteAgentColumns+` FROM agents WHERE id = ?`,
		db.BootstrapAgentID(orgID))
	a, err := scanAgentRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *agentStore) Create(ctx context.Context, orgID string, a domain.Agent) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	now := time.Now().UTC()
	// Caller may leave a.ID empty; the deterministic per-org id keeps
	// re-runs idempotent without relying on a UNIQUE constraint that
	// the SQLite shape doesn't have.
	id := a.ID
	if id == "" {
		id = db.BootstrapAgentID(orgID)
	}
	displayName := a.DisplayName
	if displayName == "" {
		displayName = "Triage Factory Bot"
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT OR IGNORE INTO agents
			(id, display_name, default_model, default_autonomy_suitability,
			 github_app_installation_id, github_pat_user_id, jira_service_account_id,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, displayName, nullString(a.DefaultModel), a.DefaultAutonomySuitability,
		nullString(a.GitHubAppInstallationID), nullString(a.GitHubPATUserID), nullString(a.JiraServiceAccountID),
		now, now)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *agentStore) Update(ctx context.Context, orgID string, a domain.Agent) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE agents
		SET display_name = ?,
		    default_model = ?,
		    default_autonomy_suitability = ?,
		    jira_service_account_id = ?,
		    updated_at = ?
		WHERE id = ?
	`, a.DisplayName, nullString(a.DefaultModel), a.DefaultAutonomySuitability,
		nullString(a.JiraServiceAccountID), time.Now().UTC(), a.ID)
	return err
}

func (s *agentStore) SetGitHubAppInstallation(ctx context.Context, orgID, agentID, installationID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	// Clear the PAT-borrow FK in the same statement so the "at most
	// one credential source" invariant holds. Empty installationID is
	// allowed (caller wiring an org out of App-mode back to PAT-borrow
	// will use SetGitHubPATUser instead, but this stays defensive).
	_, err := s.q.ExecContext(ctx, `
		UPDATE agents
		SET github_app_installation_id = ?,
		    github_pat_user_id = NULL,
		    updated_at = ?
		WHERE id = ?
	`, nullString(installationID), time.Now().UTC(), agentID)
	return err
}

func (s *agentStore) SetGitHubPATUser(ctx context.Context, orgID, agentID, userID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE agents
		SET github_pat_user_id = ?,
		    github_app_installation_id = NULL,
		    updated_at = ?
		WHERE id = ?
	`, nullString(userID), time.Now().UTC(), agentID)
	return err
}

// nullString returns NULL when s is empty so the column scans back as
// NULL on the next Get rather than as an empty TEXT.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func scanAgentRow(row *sql.Row) (domain.Agent, error) {
	var a domain.Agent
	var defaultModel, ghApp, ghPATUser, jiraSvc sql.NullString
	var defAutonomy sql.NullFloat64
	if err := row.Scan(&a.ID, &a.DisplayName, &defaultModel, &defAutonomy,
		&ghApp, &ghPATUser, &jiraSvc, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return a, err
	}
	a.DefaultModel = defaultModel.String
	if defAutonomy.Valid {
		v := defAutonomy.Float64
		a.DefaultAutonomySuitability = &v
	}
	a.GitHubAppInstallationID = ghApp.String
	a.GitHubPATUserID = ghPATUser.String
	a.JiraServiceAccountID = jiraSvc.String
	return a, nil
}
