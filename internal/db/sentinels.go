package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// AssertLocalSentinels confirms the synthetic LocalDefault* rows
// seeded by the v1.11.0 baseline migration are present and match the
// runmode constants. Catches drift between the migration's hardcoded
// UUID literals (in INSERT seed rows + NOT NULL DEFAULT clauses on
// org_id/team_id/creator_user_id columns) and the Go runmode constants
// — if either side moves without the other, this fails loudly at
// startup rather than silently producing rows that reference a
// non-existent team.
//
// Only meaningful in local mode (multi-mode boots through a different
// path that creates orgs/teams/users at sign-up). Returns nil
// immediately in multi mode.
func AssertLocalSentinels(database *sql.DB) error {
	if runmode.Current() != runmode.ModeLocal {
		return nil
	}
	checks := []struct {
		table string
		id    string
	}{
		{"orgs", runmode.LocalDefaultOrgID},
		{"teams", runmode.LocalDefaultTeamID},
		{"users", runmode.LocalDefaultUserID},
	}
	for _, c := range checks {
		var seen string
		err := database.QueryRow(
			fmt.Sprintf(`SELECT id FROM %s WHERE id = ?`, c.table),
			c.id,
		).Scan(&seen)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf(
				"local sentinel mismatch: %s row %q not found — "+
					"the migration's seeded UUID has drifted from the runmode constant. "+
					"Reconcile internal/db/migrations-sqlite/202605130001_baseline.sql "+
					"with internal/runmode/runmode.go",
				c.table, c.id,
			)
		}
		if err != nil {
			return fmt.Errorf("probe %s sentinel: %w", c.table, err)
		}
	}
	return nil
}
