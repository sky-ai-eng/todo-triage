package domain

import "time"

// Agent is the workload-identity domain type — the org's bot. One row
// per org in multi mode; one row in the synthetic single-org world in
// local mode. Distinct from AgentRun (which is the execution domain in
// the same package): an Agent is "who acts," an AgentRun is "what they
// did in one delegation." SKY-242 / SKY-260 introduces this domain to
// give the bot first-class identity separate from human users.
//
// Credentials: at most one of GitHubAppInstallationID / GitHubPATUserID
// is populated in v1. Multi mode prefers App install; local + B2C trials
// fall back to PAT-borrow. Local mode leaves both empty because the
// PAT lives in the OS keychain and is looked up at run dispatch, not
// referenced by FK into a users table that doesn't exist locally.
type Agent struct {
	ID                         string
	DisplayName                string
	DefaultModel               string   // "" = no default; consumer falls through to global default
	DefaultAutonomySuitability *float64 // nil = no default; consumer uses the trigger-level threshold instead
	GitHubAppInstallationID    string   // "" if no App installed
	GitHubPATUserID            string   // "" if not borrowing a PAT (always "" in local mode)
	JiraServiceAccountID       string   // "" if no Jira service account (v2 surface)
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
}

// TeamAgent is the team-level membership row for an Agent. Default-
// enabled at team creation; team members can toggle Enabled per team
// and override the agent-level model / autonomy settings without
// touching the org-wide row.
//
// PerTeamModel / PerTeamAutonomySuitability override the corresponding
// fields on Agent when populated. Empty / nil falls back to the agent's
// defaults — consumers read with a small helper, not by string-compare.
type TeamAgent struct {
	TeamID                     string
	AgentID                    string
	Enabled                    bool
	PerTeamModel               string   // "" → use Agent.DefaultModel
	PerTeamAutonomySuitability *float64 // nil → use Agent.DefaultAutonomySuitability
	AddedAt                    time.Time
}
