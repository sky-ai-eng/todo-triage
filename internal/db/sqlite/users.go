package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// usersStore — SQLite impl. The constructor accepts two queryers for
// signature parity with the Postgres impl (SKY-296); SQLite has one
// connection so both collapse to the same queryer. The
// `...System` variants delegate to their non-System counterparts.
type usersStore struct{ q queryer }

func newUsersStore(q, _ queryer) db.UsersStore { return &usersStore{q: q} }

var _ db.UsersStore = (*usersStore)(nil)

func (s *usersStore) GetGitHubUsername(ctx context.Context, userID string) (string, error) {
	var login sql.NullString
	err := s.q.QueryRowContext(ctx,
		`SELECT github_username FROM users WHERE id = ?`,
		userID,
	).Scan(&login)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read users.github_username: %w", err)
	}
	return login.String, nil
}

func (s *usersStore) SetGitHubUsername(ctx context.Context, userID, login string) error {
	var val any
	if login != "" {
		val = login
	} // else val stays nil → NULL
	result, err := s.q.ExecContext(ctx,
		`UPDATE users SET github_username = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		val, userID,
	)
	if err != nil {
		return fmt.Errorf("update users.github_username: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read users.github_username update result: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("update users.github_username: user %q not found", userID)
	}
	return nil
}

func (s *usersStore) GetDisplayName(ctx context.Context, userID string) (string, error) {
	var name sql.NullString
	err := s.q.QueryRowContext(ctx,
		`SELECT display_name FROM users WHERE id = ?`,
		userID,
	).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read users.display_name: %w", err)
	}
	return name.String, nil
}

func (s *usersStore) GetJiraIdentity(ctx context.Context, userID string) (string, string, error) {
	var accountID, displayName sql.NullString
	err := s.q.QueryRowContext(ctx,
		`SELECT jira_account_id, jira_display_name FROM users WHERE id = ?`,
		userID,
	).Scan(&accountID, &displayName)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read users.jira_account_id/jira_display_name: %w", err)
	}
	return accountID.String, displayName.String, nil
}

func (s *usersStore) GetJiraIdentitySystem(ctx context.Context, userID string) (string, string, error) {
	return s.GetJiraIdentity(ctx, userID)
}

func (s *usersStore) GetGitHubUsernameSystem(ctx context.Context, userID string) (string, error) {
	return s.GetGitHubUsername(ctx, userID)
}

func (s *usersStore) SetJiraIdentity(ctx context.Context, userID, accountID, displayName string) error {
	var accVal, nameVal any
	if accountID != "" {
		accVal = accountID
	}
	if displayName != "" {
		nameVal = displayName
	}
	result, err := s.q.ExecContext(ctx,
		`UPDATE users SET jira_account_id = ?, jira_display_name = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		accVal, nameVal, userID,
	)
	if err != nil {
		return fmt.Errorf("update users.jira_account_id/jira_display_name: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read users.jira_identity update result: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("update users.jira_identity: user %q not found", userID)
	}
	return nil
}
