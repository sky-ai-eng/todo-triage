// Top-level entry points for kicking off a delegated agent run, plus
// the source-specific worktree setup (GitHub PR vs Jira lazy) the
// generic runAgent loop consumes via runConfig.

package delegate

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// runConfig holds everything the generic agent runner needs.
//
// Two delegation shapes share this struct:
//
//   - GitHub PR (eager): hasWT=true, wtPath is the worktree, runRoot=wtPath
//     (the worktree IS the run-root), owner/repo populated from the PR.
//     Cleanup uses RemoveAt(wtPath) + CleanupPRConfig.
//
//   - Jira (lazy): hasWT=false, wtPath=runRoot is the throwaway run-root
//     (initial cwd; holds _scratch/entity-memory/ but no codebase), owner/repo empty.
//     Per-repo worktrees materialize as subdirs under runRoot via the
//     `triagefactory exec workspace add` CLI; the run_worktrees DB table
//     is the source of truth for cleanup, which iterates the table at
//     runAgent terminal.
type runConfig struct {
	scope     string  // what the agent is scoped to (repo, PR, issue)
	toolsRef  string  // tool documentation to inject
	wtPath    string  // initial cwd: GitHub PR worktree, or Jira run-root
	hasWT     bool    // GitHub PR has a real worktree to clean up via RemoveAt; Jira's worktrees are tracked in run_worktrees and cleaned by iterating that table
	runRoot   string  // run-root path: GitHub PR runs == wtPath; Jira lazy runs == the throwaway parent of materialized worktrees. Always set so $TRIAGE_FACTORY_RUN_ROOT resolves uniformly for the memory-gate retry.
	owner     string  // resolved GitHub owner (empty for Jira lazy runs)
	repo      string  // resolved GitHub repo (empty for Jira lazy runs)
	prNumber  int     // PR number (0 for non-PR runs); set so the runAgent defer can call worktree.CleanupPRConfig and reclaim the per-PR remote + branch tracking config the bare repo would otherwise accumulate
	headRef   string  // PR head ref (empty for non-PR runs); passed to CleanupPRConfig so own-repo branch tracking (branch.<headRef>.*) gets reclaimed alongside fork-only artifacts
	projectID *string // entity's project assignment (nil for un-assigned); SKY-219 uses this to copy the project's knowledge-base into ./_scratch/project-knowledge/

	extraAllowedTools string // comma-separated extra tools from prompt.AllowedTools + agent scans; merged into --allowedTools at spawn time
}

// Delegate kicks off an async agent run for any task type.
// Routes to the appropriate worktree setup based on task source.
func (s *Spawner) Delegate(task domain.Task, explicitPromptID string, triggerType string, triggerID string) (string, error) {
	s.mu.Lock()
	ghClient := s.ghClient
	model := s.model
	s.mu.Unlock()

	// Resolve prompt
	resolved, err := s.resolvePrompt(task, explicitPromptID)
	if err != nil {
		return "", err
	}
	promptID := resolved.ID
	mission := resolved.Body

	extraTools := s.collectExtraTools(resolved.AllowedTools)

	if err := s.prompts.IncrementUsage(context.Background(), runmode.LocalDefaultOrg, promptID); err != nil {
		log.Printf("[delegate] warning: failed to increment usage for prompt %s: %v", promptID, err)
	}

	if triggerType == "" {
		triggerType = "manual"
	}
	runID := uuid.New().String()
	if err := db.CreateAgentRun(s.database, domain.AgentRun{
		ID:          runID,
		TaskID:      task.ID,
		PromptID:    promptID,
		Status:      "initializing",
		Model:       model,
		TriggerType: triggerType,
		TriggerID:   triggerID,
	}); err != nil {
		return "", fmt.Errorf("create agent run: %w", err)
	}
	s.broadcastRunUpdate(runID, "initializing")

	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancels[runID] = cancel
	s.mu.Unlock()

	go func() {
		startTime := time.Now()
		defer func() {
			s.mu.Lock()
			delete(s.cancels, runID)
			// takenOver is intentionally NOT deleted here — Takeover
			// is still running its copy/finalize work in another
			// goroutine and reads wasTakenOver() up to the very last
			// step. Leaving the entry set forever costs a few bytes
			// per takeover and avoids subtle TOCTOU races.
			s.mu.Unlock()
			cancel()
			// Drain the per-entity firing queue. Fires after every
			// terminal status (completed, failed, cancelled,
			// task_unsolvable, pending_approval, taken_over) since
			// this defer runs unconditionally on goroutine exit.
			// notifyDrainer itself filters out manual runs — manual
			// is decoupled from the queue per SKY-189.
			s.notifyDrainer(triggerType, task.EntityID)
		}()

		// Phase 1: set up worktree + build config based on task source
		var cfg runConfig
		var setupErr error

		switch task.EntitySource {
		case "github":
			cfg, setupErr = s.setupGitHub(ctx, runID, task, ghClient)
		case "jira":
			cfg, setupErr = s.setupJira(ctx, runID, task, ghClient)
		default:
			setupErr = fmt.Errorf("unsupported task source: %s", task.EntitySource)
		}

		if setupErr != nil {
			if ctx.Err() != nil {
				s.handleCancelled(runID, startTime, cfg.wtPath)
				return
			}
			s.failRun(runID, task.ID, triggerType, setupErr.Error())
			return
		}

		cfg.extraAllowedTools = extraTools

		// Phase 2: run the agent
		// Announce the run start once setup is done — the agent is about to
		// actually execute work. Distinguish auto-fired (event trigger) from
		// user-initiated (manual) so the user can tell at a glance whether
		// they kicked this off or automation did.
		verb := "Run started"
		if triggerType == "event" {
			verb = "Auto-fired"
		}
		toast.Info(s.wsHub, fmt.Sprintf("%s: %s (%s)", verb, truncateToastMsg(task.Title, 80), shortRunID(runID)))
		s.runAgent(ctx, runID, task, mission, cfg, startTime, model, triggerType)
	}()

	return runID, nil
}

// DelegatePR is a convenience wrapper that calls Delegate for backward compatibility.
func (s *Spawner) DelegatePR(task domain.Task, explicitPromptID string) (string, error) {
	return s.Delegate(task, explicitPromptID, "manual", "")
}

// setupGitHub prepares a worktree for a GitHub PR task.
func (s *Spawner) setupGitHub(ctx context.Context, runID string, task domain.Task, ghClient *ghclient.Client) (runConfig, error) {
	if ghClient == nil {
		return runConfig{}, fmt.Errorf("GitHub credentials not configured")
	}

	// EntitySourceID for GitHub PRs is "owner/repo#42" — extract the repo part.
	repoStr := task.EntitySourceID
	if idx := strings.LastIndex(repoStr, "#"); idx >= 0 {
		repoStr = repoStr[:idx]
	}
	owner, repo := parseOwnerRepo(repoStr)
	if owner == "" || repo == "" {
		return runConfig{}, fmt.Errorf("cannot parse owner/repo from entity source ID: %q", task.EntitySourceID)
	}

	prNumber := 0
	if idx := strings.LastIndex(task.EntitySourceID, "#"); idx >= 0 {
		fmt.Sscanf(task.EntitySourceID[idx+1:], "%d", &prNumber)
	}
	if prNumber == 0 {
		return runConfig{}, fmt.Errorf("invalid PR number from task.EntitySourceID: %q", task.EntitySourceID)
	}

	s.updateStatus(runID, "fetching")
	pr, err := ghClient.GetPR(owner, repo, prNumber, false)
	if err != nil {
		return runConfig{}, fmt.Errorf("failed to fetch PR: %w", err)
	}

	// pr.BaseCloneURL / pr.BaseSSHURL: the upstream (the repo where
	// /pulls/<n> lives, which is always the canonical repo by
	// construction), populated from base.repo.{clone_url,ssh_url}.
	// pr.CloneURL / pr.SSHURL: the head's URL — the fork's URL when
	// the PR is from a fork, equal to the base URL for own-repo PRs.
	// CreateForPR uses the upstream URL to fetch refs/pull/<n>/head
	// and (if they differ) the head URL to configure push tracking so
	// commits land in the fork's branch instead of creating a stray
	// branch on upstream.
	//
	// Pick HTTPS or SSH form based on the user's config. We can't
	// just always pass HTTPS — repairOriginURL inside CreateForPR
	// rewrites the bare's origin to whatever URL we pass, so passing
	// HTTPS would clobber the SSH origin that bootstrap put there.
	//
	// Failure modes when SSH is selected:
	//   - pr.BaseSSHURL empty: the API didn't return base.repo.ssh_url
	//     (theoretically possible on weird GHE configs). Fail loudly
	//     rather than fall back to HTTPS — falling back would silently
	//     repoint the bare to HTTPS via repairOriginURL.
	//   - pr.SSHURL empty while pr.CloneURL non-empty: same condition
	//     for the head repo (the fork). Fork tracking would silently
	//     mix origins (SSH bare, HTTPS push remote) and break `git push`
	//     for SSH-only users. Same fail-loud treatment.
	// pr.CloneURL == "" (head.repo == null) is the deleted-fork case;
	// pr.SSHURL is also empty there, and we leave headCloneURL = ""
	// so CreateForPR's hasHeadRepo=false branch fires correctly.
	upstreamCloneURL, headCloneURL := pr.BaseCloneURL, pr.CloneURL
	if cfg, cErr := config.Load(); cErr == nil && cfg.GitHub.CloneProtocol == "ssh" {
		if pr.BaseSSHURL == "" {
			return runConfig{}, fmt.Errorf("PR #%d on %s/%s: SSH clone protocol selected but GitHub did not return base.repo.ssh_url; switch to HTTPS in Settings or check your GHE config", prNumber, owner, repo)
		}
		upstreamCloneURL = pr.BaseSSHURL
		if pr.CloneURL != "" {
			if pr.SSHURL == "" {
				return runConfig{}, fmt.Errorf("PR #%d on %s/%s: SSH clone protocol selected but GitHub did not return head.repo.ssh_url for the head fork; switch to HTTPS in Settings or check your GHE config", prNumber, owner, repo)
			}
			headCloneURL = pr.SSHURL
		}
	} else if cErr != nil {
		log.Printf("[delegate] load config to pick clone protocol for run %s: %v (defaulting to HTTPS)", runID, cErr)
	}
	if upstreamCloneURL == "" {
		return runConfig{}, fmt.Errorf("PR #%d on %s/%s: GitHub did not return a usable upstream URL; cannot create worktree", prNumber, owner, repo)
	}

	s.updateStatus(runID, "cloning")
	wtPath, err := worktree.CreateForPR(ctx, owner, repo, upstreamCloneURL, headCloneURL, pr.HeadRef, prNumber, runID)
	if err != nil {
		return runConfig{}, fmt.Errorf("failed to create worktree: %w", err)
	}

	if _, err := s.database.Exec(`UPDATE runs SET worktree_path = ? WHERE id = ?`, wtPath, runID); err != nil {
		log.Printf("[delegate] warning: failed to update worktree path for run %s: %v", runID, err)
	}

	// SKY-220: block briefly so the project classifier (post-poll
	// runner) can decide this entity before we read project_id for KB
	// injection. Nil-safe — tests and pre-classifier configurations
	// skip the wait.
	s.awaitClassification(ctx, task.EntityID)

	return runConfig{
		scope:     fmt.Sprintf("Repository: %s/%s\nPR: #%d\nBranch: %s", owner, repo, prNumber, pr.HeadRef),
		toolsRef:  ai.GHToolsTemplate,
		wtPath:    wtPath,
		hasWT:     true,
		runRoot:   wtPath, // GitHub PR runs: worktree IS the run-root, so $TRIAGE_FACTORY_RUN_ROOT resolves to the worktree
		owner:     owner,
		repo:      repo,
		prNumber:  prNumber,
		headRef:   pr.HeadRef,
		projectID: lookupEntityProjectID(s.database, task.EntityID),
	}, nil
}

// setupJira prepares the run-root for a Jira delegation. No repo is
// pre-cloned — the agent decides which repo(s) it needs after reading
// the ticket and materializes them via `triagefactory exec workspace
// add <owner/repo>`. Each materialization lands a worktree at
// {runRoot}/{owner}/{repo}/ and inserts a row into run_worktrees.
//
// The agent's initial cwd is the run-root: a throwaway dir holding
// only ./_scratch/entity-memory/ (populated by materializePriorMemories
// below). Both gh and jira tool surfaces are exposed since the agent
// will need both to implement and ship a PR.
//
// runs.worktree_path is set to the run-root. Yield/resume reads this
// field as the cwd to resume the session in (`claude --resume` keys
// session storage by cwd-encoded ~/.claude/projects/<encoded>, and we
// passed cwd=runRoot to the original agentproc.Run). Even though Jira
// runs don't have a single "the worktree" the way GitHub PR runs do,
// the run-root IS the agent's session cwd, which is the load-bearing
// invariant for resume. Takeover guards against Jira runs explicitly
// further down.
func (s *Spawner) setupJira(ctx context.Context, runID string, task domain.Task, ghClient *ghclient.Client) (runConfig, error) {
	runRoot, err := worktree.MakeRunRoot(runID)
	if err != nil {
		return runConfig{}, fmt.Errorf("create run root: %w", err)
	}
	if _, err := s.database.Exec(`UPDATE runs SET worktree_path = ? WHERE id = ?`, runRoot, runID); err != nil {
		log.Printf("[delegate] warning: failed to set worktree_path for Jira run %s: %v — yield/resume will reject this run", runID, err)
	}

	// SKY-220: block briefly so the project classifier can decide this
	// entity before we read project_id for KB injection. Nil-safe.
	s.awaitClassification(ctx, task.EntityID)

	return runConfig{
		scope:     fmt.Sprintf("Jira issue: %s", task.EntitySourceID),
		toolsRef:  ai.GHToolsTemplate + "\n\n" + ai.JiraToolsTemplate,
		wtPath:    runRoot,
		hasWT:     false,
		runRoot:   runRoot,
		projectID: lookupEntityProjectID(s.database, task.EntityID),
		// owner/repo intentionally empty: the agent picks per-ticket via `workspace add`
	}, nil
}
