package db

import "context"

// UsersStore owns the users table — identity facts that aren't secrets
// (display_name, github_username) live on the row. The keychain holds
// only actual credentials (PATs in local mode); usernames and display
// names live in the DB so local mode and multi mode share storage.
//
// Local mode iterates a single synthetic LocalDefaultUserID row;
// multi mode (post-SKY-251) has one row per authenticated user.
//
// # Pool split (Postgres)
//
// All methods run on the app pool. There's no admin-pool routing
// because user-row creation is an auth-flow concern (SKY-251) and
// this store only mutates existing rows.
type UsersStore interface {
	// GetGitHubUsername returns users.github_username for a user row,
	// or "" if the column is NULL or the row does not exist. Used by
	// the SKY-264 predicate matcher (author_in / reviewer_in /
	// commenter_in allowlists), the poller startup, and several
	// display surfaces.
	GetGitHubUsername(ctx context.Context, userID string) (string, error)

	// SetGitHubUsername writes users.github_username for an existing
	// user row. Passing "" clears the column (NULL). Returns an error
	// when the target row does not exist — bootstrap paths own row
	// creation; this store only mutates existing rows. Idempotent on
	// identical input.
	SetGitHubUsername(ctx context.Context, userID, login string) error

	// GetDisplayName returns users.display_name, or "" if NULL or the
	// row is missing. The team-members endpoint surfaces this in
	// Variant B's roster dropdown.
	GetDisplayName(ctx context.Context, userID string) (string, error)
}
