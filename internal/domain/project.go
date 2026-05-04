package domain

import "time"

// Project is the top-level concept that segments work items by *concept*
// rather than by repo. SKY-211 / SKY-215. The Curator is the per-project
// long-lived Claude Code session that owns project context — its session
// id lives on this row. The knowledge base lives on disk at
// `~/.triagefactory/projects/<id>/knowledge-base/*.md`; SummaryMD is the
// distilled version that gets injected into delegated agents' worktrees.
//
// CuratorSessionID was originally named DesignerSessionID; SKY-216
// renamed it to match the runtime that actually populates it. The
// rename happened via the 20260503_001_curator.sql migration on
// existing installs, with the new name carried through Go code in
// the same release.
type Project struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	SummaryMD        string   `json:"summary_md,omitempty"`
	SummaryStale     bool     `json:"summary_stale"`
	CuratorSessionID string   `json:"curator_session_id,omitempty"`
	PinnedRepos      []string `json:"pinned_repos"`
	// JiraProjectKey is the Jira project key (e.g. "SKY") this
	// project is linked to, or empty if not linked. Validation at the
	// API layer requires a non-empty value to be present in
	// config.Jira.Projects. SKY-217.
	JiraProjectKey string `json:"jira_project_key"`
	// LinearProjectKey is the Linear project key/identifier this
	// project is linked to, or empty if not linked. Independent of
	// JiraProjectKey — both can be set on the same project. Linear
	// integration is future work; until it ships, validation rejects
	// any non-empty value at the API layer.
	LinearProjectKey string `json:"linear_project_key"`
	// SpecAuthorshipPromptID points at the prompt the Curator
	// materializes as a Claude Code skill (`.claude/skills/ticket-spec/`)
	// when authoring tickets for this project. Empty = use the seeded
	// system default ("system-ticket-spec"). Per-project rather than
	// global so a user with mixed teams can give each its own
	// editorial standard. SKY-221.
	SpecAuthorshipPromptID string    `json:"spec_authorship_prompt_id"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

// SystemTicketSpecPromptID is the deterministic ID of the seeded
// default spec-authorship prompt. Curator dispatch falls back to
// this ID whenever a project's SpecAuthorshipPromptID is empty.
// Kept as a const so the seed site, the DB defaulting on insert,
// and the curator runtime all reference one source of truth.
const SystemTicketSpecPromptID = "system-ticket-spec"
