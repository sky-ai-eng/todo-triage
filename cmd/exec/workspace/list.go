package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/runident"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// listOutput is the JSON shape printed by `workspace list`. Two sections:
//
//   - available: every repo configured in Triage Factory the agent COULD
//     `workspace add`. Sourced from the repo_profiles table. Empty list
//     means no repos are configured (delegated agents shouldn't ever see
//     this since the spawner gates on profile readiness, but the shape
//     stays consistent).
//   - materialized: repos the agent has already `workspace add`'d for
//     this run, with the absolute worktree path and the feature branch
//     `git worktree add` checked out.
//
// Two-section structure (rather than a flat list with a `materialized`
// boolean) keeps the field shape per entry uniform — available entries
// don't carry a path, materialized entries don't need a default branch
// — and makes the "what could I add vs. what have I added" split
// obvious to a reader skimming the JSON.
type listOutput struct {
	Available    []listAvailable    `json:"available"`
	Materialized []listMaterialized `json:"materialized"`
}

type listAvailable struct {
	Repo string `json:"repo"`
	// Description is the upstream-sourced one-liner from the repo's
	// profile (GitHub repo metadata, captured during profiling). Helps
	// the agent disambiguate between configured repos when the ticket
	// text doesn't make the target obvious. Empty for repos whose
	// profiling hasn't run yet (skeleton rows in repo_profiles).
	//
	// We deliberately omit profile_text — it's multi-KB of LLM-
	// generated prose, would burn meaningful context on every list
	// call, and can be stale (regenerated only on GitHub config
	// change). Description is the cheap, authoritative signal.
	Description string `json:"description,omitempty"`
}

type listMaterialized struct {
	Repo   string `json:"repo"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
}

// listWorkspaces is the orchestration body of `workspace list`,
// extracted from runList so it returns errors instead of os.Exit-ing.
// Mirrors the runAdd / materializeWorkspace split for testability.
//
// Jira-only, mirroring materializeWorkspace. GitHub PR runs have a
// single eagerly-materialized worktree and don't use the workspace
// surface at all; surfacing configured-repo discovery on those runs
// would advertise a path the agent can't take and contradict the docs
// in jira-tools.txt.
//
// Routing (SKY-302): every read goes through `...System` admin-pool
// methods. List is pure introspection of the run the agent already
// owns — wrapping in synthetic-claims just to satisfy RLS for a
// read-only inventory would be tx-machinery overhead. The
// ResolveRunIdentity probe still happens so an invalid
// TRIAGE_FACTORY_RUN_ID fails with a clear "spawner bug" error.
func listWorkspaces(stores db.Stores, runID string) (listOutput, error) {
	ctx := context.Background()
	ident, err := runident.ResolveRunIdentity(ctx, stores, runID)
	switch {
	case errors.Is(err, runident.ErrRunIdentityMissing):
		return listOutput{}, errMissingRunID
	case errors.Is(err, runident.ErrRunIdentityNotFound):
		return listOutput{}, fmt.Errorf("%w: %s", errRunNotFound, runID)
	case err != nil:
		return listOutput{}, fmt.Errorf("workspace list: %w", err)
	}

	// Re-read the run to get task_id. ResolveRunIdentity could
	// return it, but every other subcommand only needs the routing
	// triple — keeping it minimal beats threading a one-call-site
	// field through the helper.
	run, err := stores.AgentRuns.GetSystem(ctx, ident.OrgID, ident.RunID)
	if err != nil {
		return listOutput{}, fmt.Errorf("workspace list: load run: %w", err)
	}
	if run == nil {
		// Resolved a moment ago and the row vanished — surface as
		// the same not-found shape rather than a confusing "load
		// run" wrapped error.
		return listOutput{}, fmt.Errorf("%w: %s", errRunNotFound, runID)
	}
	task, err := stores.Tasks.GetSystem(ctx, ident.OrgID, run.TaskID)
	if err != nil {
		return listOutput{}, fmt.Errorf("workspace list: load task: %w", err)
	}
	if task == nil {
		return listOutput{}, fmt.Errorf("%w: %s", errTaskNotFound, run.TaskID)
	}
	if task.EntitySource != "jira" {
		return listOutput{}, fmt.Errorf("%w (run task source is %q)", errNotJiraRun, task.EntitySource)
	}

	// ListSystem (not GetConfiguredRepoNames) so we can surface
	// each repo's description in the JSON output. The description
	// is the agent's cheapest disambiguation signal when the ticket
	// text doesn't make the target repo obvious.
	configured, err := stores.Repos.ListSystem(ctx, ident.OrgID)
	if err != nil {
		return listOutput{}, fmt.Errorf("workspace list: load configured repos: %w", err)
	}
	rows, err := stores.RunWorktrees.ListSystem(ctx, ident.OrgID, ident.RunID)
	if err != nil {
		return listOutput{}, fmt.Errorf("workspace list: load materialized worktrees: %w", err)
	}

	materializedSet := make(map[string]struct{}, len(rows))
	materialized := make([]listMaterialized, 0, len(rows))
	for _, r := range rows {
		materializedSet[r.RepoID] = struct{}{}
		materialized = append(materialized, listMaterialized{
			Repo:   r.RepoID,
			Path:   r.Path,
			Branch: r.FeatureBranch,
		})
	}

	// Available = configured-and-profilable minus already-materialized.
	// Skeleton rows in repo_profiles (added to the configured list
	// but not yet profiled — clone_url is empty) are filtered out:
	// `workspace add` would reject them later with "no clone URL on
	// its profile" anyway, so surfacing them here as discoverable
	// options would just send the agent toward unusable choices.
	// Materialized filter keeps the list framed as "what's still
	// unmaterialized," matching the agent's mental model.
	available := make([]listAvailable, 0, len(configured))
	for _, p := range configured {
		if p.CloneURL == "" {
			continue
		}
		if _, alreadyAdded := materializedSet[p.ID]; alreadyAdded {
			continue
		}
		available = append(available, listAvailable{
			Repo:        p.ID,
			Description: p.Description,
		})
	}

	return listOutput{Available: available, Materialized: materialized}, nil
}

// runList is the CLI entrypoint: env → listWorkspaces → stdout/stderr.
func runList(stores db.Stores, args []string) {
	out, err := listWorkspaces(stores, os.Getenv(runident.RunIdentityEnvVar))
	if err != nil {
		exitErr(err.Error())
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, "workspace list: encode: "+err.Error())
		os.Exit(1)
	}
}
