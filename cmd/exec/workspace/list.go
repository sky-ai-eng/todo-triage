package workspace

import (
	"encoding/json"
	"fmt"
	"os"

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
}

type listMaterialized struct {
	Repo   string `json:"repo"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
}

// runList prints the JSON inventory of repos the agent can or has added
// for the current run. Diagnostic + discovery surface — the spawner's
// cleanup is the source of truth for actual on-disk state, but the
// agent uses this to decide which repo(s) to materialize when the
// ticket text alone isn't conclusive.
func runList(database *db.DB, args []string) {
	runID := os.Getenv("TRIAGE_FACTORY_RUN_ID")
	if runID == "" {
		exitErr("workspace list: TRIAGE_FACTORY_RUN_ID not set; this command must be invoked by the delegated agent")
	}

	configured, err := db.GetConfiguredRepoNames(database.Conn)
	if err != nil {
		exitErr("workspace list: load configured repos: " + err.Error())
	}
	rows, err := db.GetRunWorktrees(database.Conn, runID)
	if err != nil {
		exitErr("workspace list: load materialized worktrees: " + err.Error())
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

	// Available = configured minus already-materialized. Filtering keeps
	// the agent from re-adding a repo it already has (which would just
	// be a no-op via the idempotency check, but having `available`
	// reflect "what's still unmaterialized" is the more useful framing).
	available := make([]listAvailable, 0, len(configured))
	for _, name := range configured {
		if _, alreadyAdded := materializedSet[name]; alreadyAdded {
			continue
		}
		available = append(available, listAvailable{Repo: name})
	}

	out := listOutput{Available: available, Materialized: materialized}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, "workspace list: encode: "+err.Error())
		os.Exit(1)
	}
}
