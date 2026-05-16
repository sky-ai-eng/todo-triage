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

	// GetJiraIdentity returns (jira_account_id, jira_display_name) for
	// a user row, both "" if the columns are NULL or the row does not
	// exist. Used by the SKY-270 predicate matcher (assignee_in /
	// reporter_in / commenter_in allowlists), the stock handler's
	// "is this assigned to me" check, and the optimistic post-claim
	// snapshot update.
	GetJiraIdentity(ctx context.Context, userID string) (accountID, displayName string, err error)

	// SetJiraIdentity writes both jira_account_id and jira_display_name
	// for an existing user row in a single UPDATE. Both come from one
	// auth.ValidateJira call (Atlassian's /myself endpoint returns
	// them together), so pairing them keeps the columns consistent.
	// Passing "" for either clears the column (NULL). Returns an error
	// when the target row does not exist — bootstrap paths own row
	// creation; this store only mutates existing rows. Idempotent on
	// identical input.
	SetJiraIdentity(ctx context.Context, userID, accountID, displayName string) error

	// --- Admin-pool variants (`...System`) ---
	//
	// GetGitHubUsernameSystem mirrors GetGitHubUsername but routes
	// through the admin pool in Postgres. The single consumer is the
	// poller bootstrap, which reads each repo owner's stored login at
	// server boot to seed the GitHub poller's identity allowlist —
	// there is no JWT-claims context at that point, and the read
	// spans every user the poller intends to act for. Behavior
	// matches GetGitHubUsername; SQLite collapses the two variants
	// to one connection.
	GetGitHubUsernameSystem(ctx context.Context, userID string) (string, error)
}
