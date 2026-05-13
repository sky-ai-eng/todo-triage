package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// AssertLocalSentinels confirms the synthetic LocalDefault* rows
// seeded by the v1.11.0 baseline migration are present at startup.
// Catches the case where the seed INSERT block is removed or its
// UUIDs are edited without updating the runmode constants — boot
// fails loudly rather than silently producing orphan rows.
//
// Scope limit: this is a seed-row-presence check only. It does NOT
// validate that the migration's NOT NULL DEFAULT '<uuid>' clauses
// on org_id/team_id/creator_user_id columns still match the runmode
// constants — those columns have no FK in SQLite, so DEFAULT drift
// is invisible at runtime. That class of drift is caught at CI
// time by TestMigrationDefaults_MatchRuntimeConstants in
// sentinels_test.go, which probes every defaulted column by INSERT.
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
