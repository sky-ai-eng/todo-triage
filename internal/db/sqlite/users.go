package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

type usersStore struct{ q queryer }

func newUsersStore(q queryer) db.UsersStore { return &usersStore{q: q} }

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
