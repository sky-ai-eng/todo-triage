package db

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestMigrationDefaults_MatchRuntimeConstants catches drift between
// the v1.11.0 baseline's DEFAULT '00000000-...' literals on
// org_id / team_id / creator_user_id columns and the runmode
// constants those literals are meant to mirror.
//
// AssertLocalSentinels only verifies the seeded orgs/teams/users
// rows exist. It does NOT catch the case where a DEFAULT literal
// on (e.g.) tasks.team_id silently drifts to a different UUID
// while the seeded teams row stays at LocalDefaultTeamID — there's
// no FK on these columns in the SQLite baseline, so the drift
// produces orphan rows that no other check notices.
//
// Strategy: insert minimal probe rows that exercise every column's
// DEFAULT clause, then read back and assert the column landed on
// the expected sentinel. Each probe is rolled back in a transaction
// so the test leaves no state behind.
func TestMigrationDefaults_MatchRuntimeConstants(t *testing.T) {
	d := openMigrationsTestDB(t)
	if err := Migrate(d, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Each probe: (table, columns to set explicitly, defaulted column
	// to read back, expected sentinel). The "extra setup" closures
	// satisfy FK chains and required-not-default columns.
	cases := []struct {
		name           string
		setup          func(t *testing.T, tx *sql.Tx)
		probe          string
		probeArgs      []any
		readBackColumn string
		readBackQuery  string
		readBackArgs   []any
		expected       string
	}{
		{
			name: "prompts.org_id",
			probe: `INSERT INTO prompts (id, name, body, team_id, creator_user_id)
			        VALUES ('probe-prompt', 'p', 'b', ?, ?)`,
			probeArgs:      []any{runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID},
			readBackColumn: "org_id",
			readBackQuery:  `SELECT org_id FROM prompts WHERE id = 'probe-prompt'`,
			expected:       runmode.LocalDefaultOrgID,
		},
		{
			name: "tasks.org_id+team_id+creator_user_id",
			setup: func(t *testing.T, tx *sql.Tx) {
				t.Helper()
				if _, err := tx.Exec(`INSERT INTO entities (id, source, source_id, kind)
				                      VALUES ('probe-ent', 'github', '1', 'pr')`); err != nil {
					t.Fatalf("seed entity: %v", err)
				}
				if _, err := tx.Exec(`INSERT INTO events (id, entity_id, event_type)
				                      VALUES ('probe-evt', 'probe-ent', 'github:pr:opened')`); err != nil {
					t.Fatalf("seed event: %v", err)
				}
			},
			probe: `INSERT INTO tasks (id, entity_id, event_type, primary_event_id)
			        VALUES ('probe-task', 'probe-ent', 'github:pr:opened', 'probe-evt')`,
			readBackQuery: `SELECT org_id || '|' || team_id || '|' || creator_user_id
			                FROM tasks WHERE id = 'probe-task'`,
			readBackColumn: "org_id|team_id|creator_user_id",
			expected: runmode.LocalDefaultOrgID + "|" +
				runmode.LocalDefaultTeamID + "|" +
				runmode.LocalDefaultUserID,
		},
		{
			name: "runs.org_id+team_id",
			setup: func(t *testing.T, tx *sql.Tx) {
				t.Helper()
				if _, err := tx.Exec(`INSERT INTO entities (id, source, source_id, kind)
				                      VALUES ('probe-ent-r', 'github', '2', 'pr')`); err != nil {
					t.Fatalf("seed entity: %v", err)
				}
				if _, err := tx.Exec(`INSERT INTO events (id, entity_id, event_type)
				                      VALUES ('probe-evt-r', 'probe-ent-r', 'github:pr:opened')`); err != nil {
					t.Fatalf("seed event: %v", err)
				}
				if _, err := tx.Exec(`INSERT INTO tasks (id, entity_id, event_type, primary_event_id)
				                      VALUES ('probe-task-r', 'probe-ent-r', 'github:pr:opened', 'probe-evt-r')`); err != nil {
					t.Fatalf("seed task: %v", err)
				}
				if _, err := tx.Exec(`INSERT INTO prompts (id, name, body, team_id, creator_user_id)
				                      VALUES ('probe-prompt-r', 'p', 'b', ?, ?)`,
					runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID); err != nil {
					t.Fatalf("seed prompt: %v", err)
				}
			},
			probe: `INSERT INTO runs (id, task_id, prompt_id, trigger_type, creator_user_id)
			        VALUES ('probe-run', 'probe-task-r', 'probe-prompt-r', 'manual', ?)`,
			probeArgs:      []any{runmode.LocalDefaultUserID},
			readBackQuery:  `SELECT org_id || '|' || team_id FROM runs WHERE id = 'probe-run'`,
			readBackColumn: "org_id|team_id",
			expected:       runmode.LocalDefaultOrgID + "|" + runmode.LocalDefaultTeamID,
		},
		{
			name: "projects.org_id+creator_user_id",
			probe: `INSERT INTO projects (id, name, team_id)
			        VALUES ('probe-proj', 'p', ?)`,
			probeArgs:      []any{runmode.LocalDefaultTeamID},
			readBackQuery:  `SELECT org_id || '|' || creator_user_id FROM projects WHERE id = 'probe-proj'`,
			readBackColumn: "org_id|creator_user_id",
			expected:       runmode.LocalDefaultOrgID + "|" + runmode.LocalDefaultUserID,
		},
		{
			name: "event_handlers.org_id",
			probe: `INSERT INTO event_handlers
			        (id, kind, event_type, source, name, default_priority, sort_order, team_id, creator_user_id)
			        VALUES ('probe-eh', 'rule', 'github:pr:opened', 'user', 'n', 0.5, 0, ?, ?)`,
			probeArgs:      []any{runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID},
			readBackQuery:  `SELECT org_id FROM event_handlers WHERE id = 'probe-eh'`,
			readBackColumn: "org_id",
			expected:       runmode.LocalDefaultOrgID,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := d.Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback() }()

			if tc.setup != nil {
				tc.setup(t, tx)
			}
			if _, err := tx.Exec(tc.probe, tc.probeArgs...); err != nil {
				t.Fatalf("probe insert: %v", err)
			}
			var got string
			if err := tx.QueryRow(tc.readBackQuery, tc.readBackArgs...).Scan(&got); err != nil {
				t.Fatalf("read back %s: %v", tc.readBackColumn, err)
			}
			if got != tc.expected {
				t.Errorf("%s DEFAULT drift: got %q, want %q — "+
					"the migration's DEFAULT clause has diverged from "+
					"runmode.LocalDefault*ID. Reconcile both sides.",
					tc.readBackColumn, got, tc.expected)
			}
		})
	}
}
