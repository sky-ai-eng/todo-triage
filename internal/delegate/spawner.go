package delegate

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// shortRunID truncates a run UUID to 8 chars for toast messages — full UUIDs
// are noisy in a notification. Kept consistent so users can cross-reference
// the runs page listing.
func shortRunID(runID string) string {
	if len(runID) < 8 {
		return runID
	}
	return runID[:8]
}

// QueueDrainer is the interface the spawner uses to notify the per-entity
// firing queue that an auto run has reached a terminal state and the
// entity may be ready to drain its next pending firing. Implemented by
// the routing.Router. Manual runs do not call this — manual is fully
// decoupled from the queue per the SKY-189 design.
type QueueDrainer interface {
	DrainEntity(entityID string)
}

// Spawner manages delegated agent runs.
type Spawner struct {
	database *sql.DB
	wsHub    *websocket.Hub

	mu        sync.Mutex
	ghClient  *ghclient.Client
	model     string
	cancels   map[string]context.CancelFunc // runID → cancel the entire run
	drainer   QueueDrainer                  // nil-safe; set post-construction via SetQueueDrainer
	takenOver map[string]bool               // runIDs claimed by Takeover. Sticky-on for the rest of the goroutine's lifetime even after rollback — clearing the entry would let late-firing goroutine gates race the takeover/abort lifecycle. Suppresses every cleanup path in runAgent so Takeover/abortTakeover own the row's terminal state.
}

func NewSpawner(database *sql.DB, ghClient *ghclient.Client, wsHub *websocket.Hub, model string) *Spawner {
	return &Spawner{
		database:  database,
		ghClient:  ghClient,
		wsHub:     wsHub,
		model:     model,
		cancels:   make(map[string]context.CancelFunc),
		takenOver: make(map[string]bool),
	}
}

// wasTakenOver reports whether Takeover() has claimed this run. The
// flag is set the moment Takeover validates state, BEFORE the worktree
// hand-over and the DB mark — that's intentional: every cleanup path
// in runAgent (worktree.Remove defers, RemoveClaudeProjectDir defer,
// the natural-completion block, failRun, handleCancelled) checks this
// and short-circuits, which is what keeps the source worktree on disk
// while the hand-over runs and prevents a concurrent natural completion
// from overwriting the taken_over status.
//
// The flag is sticky-on once set: neither successful takeovers nor
// failed takeovers (rolled back via abortTakeover) ever clear the
// entry. Clearing would let any late-firing gate in the runAgent
// goroutine re-read wasTakenOver and proceed with normal cleanup,
// racing whatever Takeover/abortTakeover is doing — the goroutine's
// unconditional db.CompleteAgentRun would overwrite our terminal
// stop_reason, and its RemoveClaudeProjectDir would run alongside
// ours. Leaving the flag set keeps every gate closed and Takeover
// /abortTakeover the sole writer of the row's terminal state.
func (s *Spawner) wasTakenOver(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.takenOver[runID]
}

// SetQueueDrainer wires the firing-queue drainer into the spawner. Done
// post-construction because the router (which implements QueueDrainer)
// holds a reference to the spawner, so the spawner can't take it as a
// constructor arg without a circular dependency. Same wiring pattern as
// UpdateCredentials. Safe to call once at startup; nil drainer disables
// the drain hook (used in tests).
func (s *Spawner) SetQueueDrainer(d QueueDrainer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drainer = d
}

// notifyDrainer fires the QueueDrainer hook for an entity if a drainer is
// configured AND the run that just finished was an auto-fired one.
// Manual runs are fully decoupled from the queue per SKY-189 — they
// neither participate in the gate nor trigger drains. Runs in goroutine
// to keep run-teardown latency unaffected.
func (s *Spawner) notifyDrainer(triggerType, entityID string) {
	if triggerType == "manual" || entityID == "" {
		return
	}
	s.mu.Lock()
	d := s.drainer
	s.mu.Unlock()
	if d == nil {
		return
	}
	go d.DrainEntity(entityID)
}

// UpdateCredentials hot-swaps the GitHub client and model without
// disrupting in-flight runs.
func (s *Spawner) UpdateCredentials(ghClient *ghclient.Client, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ghClient = ghClient
	s.model = model
}

// Cancel aborts a run at any phase — clone, fetch, worktree setup, or agent execution.
// The goroutine handles cleanup (worktree removal, status update).
func (s *Spawner) Cancel(runID string) error {
	s.mu.Lock()
	cancel, ok := s.cancels[runID]
	s.mu.Unlock()

	if !ok {
		return fmt.Errorf("no active run %s", runID)
	}

	cancel()
	return nil
}

// TakeoverResult is what Takeover returns to the HTTP handler.
type TakeoverResult struct {
	TakeoverPath string `json:"takeover_path"`
	SessionID    string `json:"session_id"`
}

// Sentinel errors the takeover HTTP handler uses to pick an HTTP status
// class. Anything NOT matching one of these is treated as a server-
// side problem (filesystem, git subprocess, DB) and surfaced as 5xx.
var (
	// ErrTakeoverInvalidState — the run is not in a state that can be
	// taken over (no active goroutine, no session id yet, no worktree).
	// 400 Bad Request: the client asked for something that doesn't make
	// sense given the run's state, but nothing's broken.
	ErrTakeoverInvalidState = errors.New("takeover: run not in a takeoverable state")

	// ErrTakeoverInProgress — another takeover for the same run is
	// already running. 409 Conflict: a previous request owns the slot.
	ErrTakeoverInProgress = errors.New("takeover: already in progress")

	// ErrTakeoverRaceLost — the run reached a terminal state on its own
	// before our takeover could finalize. 409 Conflict: the resource
	// changed under us, the client should re-fetch.
	ErrTakeoverRaceLost = errors.New("takeover: run finished before takeover could finalize")

	// ErrPromptNotFound — Delegate's caller passed a prompt id that
	// doesn't resolve to any row. Race-correctable (the prompt was
	// deleted between snapshot fetch and drop, or the id was simply
	// wrong) — 400 Bad Request, not 5xx.
	ErrPromptNotFound = errors.New("delegate: prompt not found")

	// ErrPromptUnspecified — Delegate's caller passed an empty prompt
	// id. The picker should have prevented this; 400 Bad Request when
	// the contract is violated.
	ErrPromptUnspecified = errors.New("delegate: no prompt specified")
)

// Takeover hands a running headless session over to the user for
// interactive resume.
//
// Order of operations is load-bearing:
//
//  1. Atomically validate the run + flip the takenOver flag while still
//     holding the spawner lock. Setting the flag BEFORE anything else
//     ensures every cleanup path in runAgent (worktree.Remove,
//     RemoveRunCwd, RemoveClaudeProjectDir, db.CompleteAgentRun, the
//     pending_approval flip, the toasts) checks wasTakenOver() and
//     short-circuits. Without that, the previous race-prone version
//     could see runAgent's defers nuke the worktree mid-copy.
//
//  2. Cancel the agent's context. The kill-watcher goroutine wakes
//     and SIGKILLs the headless process group within milliseconds, so
//     the agent stops mutating files. We DON'T wait for the runAgent
//     goroutine to finish — its defers are no-ops because of the flag,
//     and the worktree files are stable from the moment the SIGKILL
//     lands.
//
//  3. Hand over the worktree into the takeover destination via
//     worktree.CopyForTakeover (atomic `git worktree move` on the same
//     filesystem; add+overlay fallback otherwise).
//
//  4. Mark the run terminal as taken_over and broadcast the status so
//     the frontend re-renders the card.
//
// Preconditions: the run must be active (registered cancel func) and
// have produced a session_id. Without a session id `claude --resume`
// has nothing to attach to, so we refuse early.
//
// Takeover does NOT take a request context. Once we set the flag and
// SIGKILL the agent (step 2), the operation MUST complete cleanly —
// either succeed or run abortTakeover to roll back filesystem and DB
// state. Tying the work to an HTTP request's context would let a
// client disconnect (closed tab, network blip, browser navigate)
// trigger the rollback mid-takeover, irreversibly destroying the
// agent, the worktree, and the session JSONL — for an action the user
// didn't ask to abort. We use context.Background() for the commit-
// phase git operations so the work is insulated from the request
// lifecycle. Pre-commit DB lookups are fast and don't need
// cancellation either.
//
// The session JSONL under ~/.claude/projects survives by virtue of:
// (a) the gated RemoveClaudeProjectDir defer in runAgent, and
// (b) startup Cleanup honoring ListTakenOverRunIDs.
func (s *Spawner) Takeover(runID, baseDir string) (*TakeoverResult, error) {
	if baseDir == "" {
		return nil, fmt.Errorf("takeover: empty destination base dir")
	}

	// Background ctx so client disconnect during the commit can't abort
	// us mid-rename. See the function-level comment.
	ctx := context.Background()

	run, err := db.GetAgentRun(s.database, runID)
	if err != nil {
		return nil, fmt.Errorf("load run: %w", err)
	}
	if run == nil {
		return nil, fmt.Errorf("%w: run %s not found", ErrTakeoverInvalidState, runID)
	}
	if run.SessionID == "" {
		return nil, fmt.Errorf("%w: run %s has no session id yet — wait until the agent has started", ErrTakeoverInvalidState, runID)
	}
	if run.WorktreePath == "" {
		return nil, fmt.Errorf("%w: run %s has no worktree to take over (no-repo Jira run)", ErrTakeoverInvalidState, runID)
	}

	// Atomically: confirm the run is still active, confirm we haven't
	// already taken it over, flip the flag, and grab the cancel func.
	// Doing all four under the lock prevents a concurrent natural
	// completion from draining cancels[runID] between our check and
	// our set.
	s.mu.Lock()
	cancel, active := s.cancels[runID]
	already := s.takenOver[runID]
	if !active {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: run %s is no longer active — it may have just finished", ErrTakeoverInvalidState, runID)
	}
	if already {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: run %s", ErrTakeoverInProgress, runID)
	}
	s.takenOver[runID] = true
	s.mu.Unlock()

	// Capture the source cwd's symlink-resolved path BEFORE
	// CopyForTakeover removes/renames it. We need this later to find
	// the agent's session JSONL under ~/.claude/projects/<encoded-cwd>;
	// Claude Code keys session storage by cwd-encoding, so without
	// MaterializeSessionForTakeover the user's `claude --resume <id>`
	// from the takeover dir fails with "No conversation found." The
	// resolution has to happen here (path still exists) rather than
	// after the move (path is gone).
	resolvedOldCwd := worktree.ResolveClaudeProjectCwd(run.WorktreePath)

	// Stop the agent. Fire-and-forget — the kill-watcher SIGKILLs the
	// process group, the goroutine's gated defers skip cleanup, the
	// worktree stays on disk for our copy.
	if cancel != nil {
		cancel()
	}

	destPath, err := worktree.CopyForTakeover(ctx, runID, run.WorktreePath, baseDir)
	if err != nil {
		// Copy failed — the agent has been cancelled but the gated
		// defers in runAgent suppressed all the normal cleanup. Roll
		// back: clear the flag, do the cleanup ourselves, and mark the
		// row terminal so it doesn't sit in 'running' forever and so a
		// retry isn't permanently blocked by a stale takenOver entry.
		s.abortTakeover(runID, run.WorktreePath, "")
		return nil, fmt.Errorf("copy worktree: %w", err)
	}

	// CopyForTakeover removes the source: the move path renames it out
	// of /tmp atomically, the overlay fallback explicitly Removes after
	// the copy. Either way no further cleanup of the original is needed.

	// Materialize the session JSONL under the takeover dir's project
	// entry so `claude --resume` finds it. Done before the DB mark so
	// that a copy failure rolls the whole takeover back rather than
	// leaving the row in 'taken_over' state pointing at a destination
	// the user can't actually resume from.
	if err := worktree.MaterializeSessionForTakeover(resolvedOldCwd, destPath, run.SessionID); err != nil {
		s.abortTakeover(runID, run.WorktreePath, destPath)
		return nil, fmt.Errorf("copy session jsonl: %w", err)
	}

	ok, err := db.MarkAgentRunTakenOver(s.database, runID, destPath)
	if err != nil {
		// Same rollback as the copy-failure path. destPath needs
		// removing too since the row never got marked.
		s.abortTakeover(runID, run.WorktreePath, destPath)
		return nil, fmt.Errorf("mark taken_over: %w", err)
	}
	if !ok {
		// Race-loss: the goroutine wrote a real terminal status before
		// our flag was set. Its natural defers ran (the gates evaluated
		// false at that point), but ours skipped because the flag was
		// set by the time they fired — abortTakeover catches them up.
		// abortTakeover's guarded UPDATE will no-op since the row is
		// already terminal, so the agent's actual outcome is preserved.
		s.abortTakeover(runID, run.WorktreePath, destPath)
		return nil, fmt.Errorf("%w: run %s", ErrTakeoverRaceLost, runID)
	}

	s.broadcastRunUpdate(runID, "taken_over")
	toast.Info(s.wsHub, fmt.Sprintf("Taken over: run %s — resume in your terminal", shortRunID(runID)))

	return &TakeoverResult{TakeoverPath: destPath, SessionID: run.SessionID}, nil
}

// abortTakeover unwinds the in-progress takeover state when CopyForTakeover
// or the final DB mark fails. Two things have to happen: the runAgent
// goroutine's gated cleanup (worktree.Remove, RemoveClaudeProjectDir,
// the natural-completion DB write) was suppressed because the
// takenOver flag is set, so we have to do it ourselves; and the run
// row needs to reach a terminal state so the UI and the active-run
// gate don't see a phantom running run forever.
//
// Notably we DO NOT delete s.takenOver[runID]. The flag must stay
// sticky-on for the rest of the goroutine's lifetime — its gates
// (handleCancelled, failRun, the deferred RemoveClaudeProjectDir, the
// natural-completion block) re-read wasTakenOver at unpredictable
// times relative to abortTakeover. Clearing the flag would let any
// late-firing gate proceed with normal cleanup, which races our own
// rollback: the goroutine's unconditional db.CompleteAgentRun would
// overwrite our 'takeover_failed' stop_reason with 'cancelled', and
// its RemoveClaudeProjectDir would run alongside ours. Leaving the
// flag on keeps every gate closed and abortTakeover the sole writer.
//
// The retry-unblocking concern that motivated the original delete is
// moot anyway: once we've SIGKILLed the agent and reached this code
// path, there's no live run left to take over. A user retry creates
// a new delegated run, not a new takeover of the same run.
//
// destPath may be empty (copy never produced one) or may point at a
// partial directory; either way we RemoveAll it.
func (s *Spawner) abortTakeover(runID, claudeCwd, destPath string) {
	if destPath != "" {
		// Use RemoveAt rather than os.RemoveAll so the bare's
		// worktree registration is pruned. By the time we reach
		// abortTakeover after a successful CopyForTakeover, the
		// bare has a worktrees/<runID>/ entry whose gitdir points at
		// destPath; just removing the directory leaves that entry
		// dangling and breaks the next `git worktree add` or `move`
		// against the same runID.
		if err := worktree.RemoveAt(destPath, runID); err != nil {
			log.Printf("[delegate] warning: abort takeover for %s: remove dest %s: %v", runID, destPath, err)
		}
	}
	if claudeCwd != "" {
		worktree.RemoveClaudeProjectDir(claudeCwd)
		// Remove the actual worktree path (which equals claudeCwd for
		// runs with a worktree, the only kind takeover supports). Using
		// claudeCwd rather than runID-derived runDir() guards against
		// the source worktree ever living outside /tmp/triagefactory-
		// runs/<runID> — Remove(runID) would silently target the
		// canonical path and miss the actual one.
		if err := worktree.RemoveAt(claudeCwd, runID); err != nil {
			log.Printf("[delegate] warning: abort takeover for %s: remove worktree: %v", runID, err)
		}
	}

	// If the row is still non-terminal (copy/DB-error path: the
	// goroutine's gated handleCancelled didn't write anything), mark it
	// cancelled so the UI and the active-run gate don't see a phantom
	// running run forever. If the row is already terminal (race-loss
	// path), this no-ops and we leave the agent's real outcome alone.
	ok, err := db.MarkAgentRunCancelledIfActive(s.database, runID, "takeover_failed", "Takeover failed; run was cancelled")
	if err != nil {
		log.Printf("[delegate] warning: abort takeover for %s: mark cancelled: %v", runID, err)
		return
	}
	if ok {
		s.broadcastRunUpdate(runID, "cancelled")
	}
}

// runConfig holds everything the generic agent runner needs.
type runConfig struct {
	scope    string // what the agent is scoped to (repo, PR, issue)
	toolsRef string // tool documentation to inject
	wtPath   string // worktree path (empty = no working directory)
	hasWT    bool   // whether a worktree was created (controls cleanup)
	owner    string // resolved GitHub owner (empty for no-repo Jira runs)
	repo     string // resolved GitHub repo (empty for no-repo Jira runs)
}

// Delegate kicks off an async agent run for any task type.
// Routes to the appropriate worktree setup based on task source.
func (s *Spawner) Delegate(task domain.Task, explicitPromptID string, triggerType string, triggerID string) (string, error) {
	s.mu.Lock()
	ghClient := s.ghClient
	model := s.model
	s.mu.Unlock()

	// Resolve prompt
	promptID, mission, err := s.resolvePrompt(task, explicitPromptID)
	if err != nil {
		return "", err
	}
	if err := db.IncrementPromptUsage(s.database, promptID); err != nil {
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

	s.updateStatus(runID, "cloning")
	wtPath, err := worktree.CreateForPR(ctx, owner, repo, pr.CloneURL, pr.HeadRef, prNumber, runID)
	if err != nil {
		return runConfig{}, fmt.Errorf("failed to create worktree: %w", err)
	}

	if _, err := s.database.Exec(`UPDATE runs SET worktree_path = ? WHERE id = ?`, wtPath, runID); err != nil {
		log.Printf("[delegate] warning: failed to update worktree path for run %s: %v", runID, err)
	}

	return runConfig{
		scope:    fmt.Sprintf("Repository: %s/%s\nPR: #%d\nBranch: %s", owner, repo, prNumber, pr.HeadRef),
		toolsRef: ai.GHToolsTemplate,
		wtPath:   wtPath,
		hasWT:    true,
		owner:    owner,
		repo:     repo,
	}, nil
}

// setupJira prepares a worktree (if applicable) for a Jira task.
func (s *Spawner) setupJira(ctx context.Context, runID string, task domain.Task, ghClient *ghclient.Client) (runConfig, error) {
	// Look up matched repos from the task's scoring results
	matchedRepos, err := db.GetTaskMatchedRepos(s.database, task.ID)
	if err != nil {
		return runConfig{}, fmt.Errorf("failed to look up matched repos: %w", err)
	}

	switch len(matchedRepos) {
	case 0:
		// No repo match — pure Jira task, no worktree
		log.Printf("[delegate] Jira task %s: no matched repo, running without worktree", task.EntitySourceID)
		return runConfig{
			scope:    fmt.Sprintf("Jira issue: %s", task.EntitySourceID),
			toolsRef: ai.JiraToolsTemplate,
		}, nil

	case 1:
		// Single repo match — clone and create feature branch
		repoID := matchedRepos[0]
		profile, err := db.GetRepoProfile(s.database, repoID)
		if err != nil || profile == nil {
			return runConfig{}, fmt.Errorf("failed to load repo profile for %s: %v", repoID, err)
		}
		if profile.CloneURL == "" {
			return runConfig{}, fmt.Errorf("repo %s has no clone URL — try re-profiling", repoID)
		}

		s.updateStatus(runID, "cloning")
		baseBranch := profile.BaseBranch
		if baseBranch == "" {
			baseBranch = profile.DefaultBranch
		}
		featureBranch := "feature/" + task.EntitySourceID

		wtPath, err := worktree.CreateForBranch(ctx, profile.Owner, profile.Repo, profile.CloneURL, baseBranch, featureBranch, runID)
		if err != nil {
			return runConfig{}, fmt.Errorf("failed to create worktree: %w", err)
		}

		if _, err := s.database.Exec(`UPDATE runs SET worktree_path = ? WHERE id = ?`, wtPath, runID); err != nil {
			log.Printf("[delegate] warning: failed to update worktree path for run %s: %v", runID, err)
		}

		// Agent gets both GH and Jira tools when it has a repo (may need to create PRs)
		return runConfig{
			scope:    fmt.Sprintf("Repository: %s\nJira issue: %s\nBranch: %s", repoID, task.EntitySourceID, featureBranch),
			toolsRef: ai.GHToolsTemplate + "\n\n" + ai.JiraToolsTemplate,
			wtPath:   wtPath,
			hasWT:    true,
			owner:    profile.Owner,
			repo:     profile.Repo,
		}, nil

	default:
		// Multiple matches — ambiguous, block for now
		return runConfig{}, fmt.Errorf("jira task %s matched %d repos (%s) — cannot determine which to clone",
			task.EntitySourceID, len(matchedRepos), strings.Join(matchedRepos, ", "))
	}
}

// runAgent is the generic agent execution loop. Works for any task type.
func (s *Spawner) runAgent(ctx context.Context, runID string, task domain.Task, mission string, cfg runConfig, startTime time.Time, model string, triggerType string) {
	if cfg.hasWT {
		// Best-effort cleanup on return; the worktree ID is unique per run
		// so a failed remove just leaves a dangling directory under _worktrees.
		// Skipped when the run was taken over — Takeover() needs the worktree
		// to still exist for its copy and explicitly cleans up afterward.
		defer func() {
			if s.wasTakenOver(runID) {
				return
			}
			_ = worktree.RemoveAt(cfg.wtPath, runID)
		}()
	}

	// Determine the cwd for the child claude. For tasks without a repo (Jira no-match)
	// we spin up a throwaway dir so the child's session history lands in a predictable
	// disposable ~/.claude/projects entry instead of mixing into the parent binary's
	// own project dir.
	claudeCwd := cfg.wtPath
	if claudeCwd == "" {
		var err error
		claudeCwd, err = worktree.MakeRunCwd(runID)
		if err != nil {
			s.failRun(runID, task.ID, triggerType, "failed to create run cwd: "+err.Error())
			return
		}
		// Same takeover gate as the worktree.Remove defer above — Takeover
		// owns destruction of the no-cwd dir until after its copy runs.
		defer func() {
			if s.wasTakenOver(runID) {
				return
			}
			worktree.RemoveRunCwd(runID)
		}()
	}
	// Nuke the ghost ~/.claude/projects/<encoded-cwd> that claude auto-creates
	// for this cwd. Safety-railed to only touch entries under $TMPDIR.
	// Skipped when the run was taken over by the user — the JSONL inside
	// is the conversation state the resumed `claude --resume` reads.
	defer func() {
		if s.wasTakenOver(runID) {
			return
		}
		worktree.RemoveClaudeProjectDir(claudeCwd)
	}()

	// Materialize any prior task memories into ./task_memory/ so the agent
	// sees what previous iterations on this task have already tried. The
	// directory is git-excluded by writeLocalExcludes (managedExcludePatterns
	// in internal/worktree/worktree.go) so nothing leaks into the PR.
	materializePriorMemories(s.database, claudeCwd, task.EntityID)

	selfBin, err := os.Executable()
	if err != nil {
		s.failRun(runID, task.ID, triggerType, "failed to resolve own binary path: "+err.Error())
		return
	}

	// Load the primary event's metadata so buildPrompt can flatten its
	// fields into named placeholders (WORKFLOW_RUN_ID, HEAD_SHA, etc.) —
	// see placeholders.go. A DB failure here is non-fatal: the replacer
	// just leaves event-derived placeholders empty. FKs guarantee the
	// event exists, so a real miss would be a DB-level problem we want
	// to log and continue through rather than aborting the run.
	metadataJSON, err := db.GetEventMetadata(s.database, task.PrimaryEventID)
	if err != nil {
		log.Printf("[delegate] warning: failed to load event metadata for task %s (event %s): %v — event placeholders will render empty", task.ID, task.PrimaryEventID, err)
		metadataJSON = ""
	}

	prompt := buildPrompt(task, metadataJSON, mission, cfg.scope, cfg.toolsRef, selfBin, runID)

	s.updateStatus(runID, "agent_starting")
	if ctx.Err() != nil {
		s.handleCancelled(runID, startTime, cfg.wtPath)
		return
	}

	args := []string{
		"-p", prompt,
		"--model", model,
		"--output-format", "stream-json",
		"--verbose",
		"--allowedTools", BuildAllowedTools(selfBin),
		"--max-turns", "100",
	}

	cmd := exec.Command("claude", args...)
	cmd.Dir = claudeCwd
	cmd.Env = append(os.Environ(), "TRIAGE_FACTORY_RUN_ID="+runID, "TRIAGE_FACTORY_REVIEW_PREVIEW=1")
	// Set TRIAGE_FACTORY_REPO when the run has a resolved GitHub repo context
	// so gh subcommands can default to the right target without the agent
	// needing to pass --repo on every invocation. Left unset for Jira runs
	// with no matched repo; those commands either fall back to .git/config
	// (unlikely — no worktree) or hard-error, which is correct since they
	// shouldn't be touching GitHub.
	if cfg.owner != "" && cfg.repo != "" {
		cmd.Env = append(cmd.Env, "TRIAGE_FACTORY_REPO="+cfg.owner+"/"+cfg.repo)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.failRun(runID, task.ID, triggerType, "failed to create stdout pipe: "+err.Error())
		return
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		s.failRun(runID, task.ID, triggerType, "failed to start claude: "+err.Error())
		return
	}

	pgid := cmd.Process.Pid
	log.Printf("[delegate] claude started (pid: %d, pgid: %d, cwd: %s)", cmd.Process.Pid, pgid, claudeCwd)

	go func() {
		<-ctx.Done()
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
			log.Printf("[delegate] warning: failed to kill process group %d: %v", pgid, err)
		}
	}()

	s.updateStatus(runID, "running")

	stream := newStreamState()
	completion, streamErr := s.consumeClaudeStream(stdout, runID, stream)
	if streamErr != nil {
		log.Printf("[delegate] scanner error for run %s: %v", runID, streamErr)
	}

	// If Takeover() flipped the takenOver flag while we were streaming,
	// every code path below — completion ingestion, status updates, fail
	// paths, toasts — would step on the takeover lifecycle. Bail out
	// silently: Takeover owns the DB row and the worktree from here on.
	// We still drain cmd.Wait so the subprocess is reaped.
	if s.wasTakenOver(runID) {
		_ = cmd.Wait()
		return
	}

	if completion != nil {
		// Enforce the pre-complete task_memory write gate. If the agent
		// returned a completion JSON without writing ./task_memory/<runID>.md,
		// resume the session with a correction message (up to 2 retries).
		// Retries that produce new completions are merged into the totals
		// so cost/duration accounting reflects the full invocation, not
		// just the initial call.
		//
		// Pass model + repoEnv explicitly rather than letting the gate
		// read live spawner state, so a concurrent UpdateCredentials
		// can't silently switch models or drop repo context mid-run.
		repoEnv := ""
		if cfg.owner != "" && cfg.repo != "" {
			repoEnv = cfg.owner + "/" + cfg.repo
		}
		completion = s.runMemoryGate(ctx, runID, task.ID, claudeCwd, completion, stream.SessionID(), model, repoEnv)

		// Unconditional upsert of the run_memory row at termination
		// (SKY-204): row presence === "termination passed through the
		// memory gate", agent_content NULL/empty === "agent didn't
		// comply with the gate after retries." Replaces the previous
		// branching write + denormalized memory_missing flag, both of
		// which could drift from ground truth (a memory row written
		// outside the gate path would leave the flag stale). The new
		// shape also gives SKY-205's human-feedback writers an
		// always-present row to UPDATE, so they don't need
		// INSERT-or-UPDATE branching.
		agentContent := readAgentMemoryFileOrEmpty(claudeCwd, runID)
		if err := db.UpsertAgentMemory(s.database, runID, task.EntityID, agentContent); err != nil {
			log.Printf("[delegate] warning: failed to upsert memory for run %s: %v", runID, err)
		}
		if agentContent == "" {
			log.Printf("[delegate] run %s: memory file missing after gate retries (agent_content NULL)", runID)
		}

		resultSummary := ""
		status := "completed"
		if completion.IsError {
			status = "failed"
		}
		if parsed := parseAgentResult(completion.Result); parsed != nil {
			resultSummary = parsed.Summary
			switch parsed.Status {
			case "failed":
				status = "failed"
			case "task_unsolvable":
				status = "task_unsolvable"
			}
		}
		if err := db.CompleteAgentRun(s.database, runID, status, completion.CostUSD, completion.DurationMs, completion.NumTurns, completion.StopReason, resultSummary); err != nil {
			log.Printf("[delegate] warning: failed to record completion for run %s: %v", runID, err)
		}

		s.updateBreakerCounter(task.ID, triggerType, status)

		if status == "completed" {
			if pendingReview, _ := db.PendingReviewByRunID(s.database, runID); pendingReview != nil {
				status = "pending_approval"
				if _, err := s.database.Exec(`UPDATE runs SET status = ? WHERE id = ?`, status, runID); err != nil {
					log.Printf("[delegate] warning: failed to set pending_approval for run %s: %v", runID, err)
				}
			}
		}

		if status == "completed" {
			if _, err := s.database.Exec(`UPDATE tasks SET status = 'done' WHERE id = ?`, task.ID); err != nil {
				log.Printf("[delegate] warning: failed to update task %s to done: %v", task.ID, err)
			}
		}
		s.broadcastRunUpdate(runID, status)

		// Toast the terminal state. Success cases auto-hide; failed/unsolvable
		// show as an error toast so the user notices even if they've clicked
		// away from the runs page.
		switch status {
		case "completed", "pending_approval":
			toast.Success(s.wsHub, fmt.Sprintf("Run %s completed", shortRunID(runID)))
		case "failed":
			toast.Error(s.wsHub, fmt.Sprintf("Run %s failed: %s", shortRunID(runID), truncateToastMsg(resultSummary, 160)))
		case "task_unsolvable":
			toast.Warning(s.wsHub, fmt.Sprintf("Run %s — task unsolvable: %s", shortRunID(runID), truncateToastMsg(resultSummary, 140)))
		}

		// We've already captured the result from stdout; just drain any
		// remaining subprocess state. Exit code is not load-bearing here.
		_ = cmd.Wait()
		return
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			s.handleCancelled(runID, startTime, cfg.wtPath)
			return
		}
		stderr := stderrBuf.String()
		s.failRun(runID, task.ID, triggerType, fmt.Sprintf("claude exited with error: %v\nstderr: %s", err, stderr))
		return
	}

	s.failRun(runID, task.ID, triggerType, "claude exited cleanly without producing a result event")
}

// consumeClaudeStream scans NDJSON output from claude -p, persists each
// accumulated message via InsertAgentMessage, broadcasts them to UI
// subscribers, and returns the first `result` event seen as a
// *runCompletion. Shared between the initial agent invocation and the
// ResumeWithMessage helper so stream handling stays consistent across
// both entry points.
//
// Session id is persisted on runs as soon as the `system/init`
// event surfaces it, not at stream close. Inline persistence means any
// mid-run consumer (a future concurrent gate, or a panic handler
// recovering from a crash) can read it from the database without
// waiting for the stream to complete. On resume the same stream still
// carries a fresh init event with the same session id, so writing it
// again is idempotent.
//
// Returns nil *runCompletion if the stream ended without a result event
// — the caller treats that as an involuntary failure and decides via
// cmd.Wait() whether to attribute the failure to cancellation or a
// real crash.
func (s *Spawner) consumeClaudeStream(stdout io.Reader, runID string, stream *streamState) (*runCompletion, error) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	sessionPersisted := false

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		messages, completion := stream.parseLine(line, runID)

		// Persist session id the first time it appears. Done inline so
		// mid-run consumers can read it from runs without needing
		// the stream to have closed first.
		if !sessionPersisted {
			if sid := stream.SessionID(); sid != "" {
				if err := db.SetAgentRunSession(s.database, runID, sid); err != nil {
					log.Printf("[delegate] warning: failed to persist session_id for run %s: %v", runID, err)
				}
				sessionPersisted = true
				// Re-broadcast the running status so the frontend
				// re-fetches the run row and picks up SessionID. The
				// "Take over" button is gated on session id presence;
				// without this nudge it stays hidden until the next
				// status flip (often "running" → terminal), which is
				// too late to be useful.
				s.broadcastRunUpdate(runID, "running")
			}
		}

		for _, msg := range messages {
			id, err := db.InsertAgentMessage(s.database, *msg)
			if err != nil {
				log.Printf("[delegate] error storing message: %v", err)
				continue
			}
			msg.ID = int(id)
			s.broadcastMessage(runID, msg)
		}

		if completion != nil {
			return completion, nil
		}
	}
	return nil, scanner.Err()
}

// ResumeOptions configures a ResumeWithMessage invocation. Callers that
// care about consistency with an earlier invocation should populate these
// explicitly — the fallbacks read live Spawner state and will race with
// UpdateCredentials if the user rotates auth mid-run.
type ResumeOptions struct {
	// Model overrides the live spawner model. **Always pass this** when
	// resuming within a single logical run (e.g. the memory-gate retry
	// loop) — read from the value you captured at run start, not from
	// s.model at resume time. If UpdateCredentials runs between the
	// initial invocation and a resume, the live spawner model may point
	// at a different model than the initial invocation ran under, which
	// would silently switch models mid-run.
	//
	// Empty falls back to the live spawner model, which is only the
	// right choice for callers that genuinely want "current spawner
	// state" (none exist today, but the door's open).
	Model string

	// RepoEnv, if non-empty, is passed to the resumed subprocess as
	// TRIAGE_FACTORY_REPO=<value>. Preserves the GitHub repo context that
	// the initial runAgent invocation set up for gh subcommands so
	// resumes don't lose the implicit --repo default. Format is
	// "owner/name" — composed by the caller from cfg.owner and cfg.repo.
	//
	// Left empty for Jira-no-match runs that never had repo context in
	// the first place.
	RepoEnv string
}

// ResumeOutcome bundles what ResumeWithMessage returns: the raw
// completion event from the resumed stream (nil if none was observed),
// the parsed agent result JSON (nil if the completion text didn't
// contain a parseable envelope), and captured stderr for diagnostics.
//
// Callers decide how to interpret a nil Completion — the memory-gate
// retry loop treats it as "retry again if attempts remain, else flag
// memory_missing," while a yield-resume flow might treat it as a
// session-level failure and surface an error.
type ResumeOutcome struct {
	Completion *runCompletion
	Result     *agentResult
	StderrText string
}

// ResumeWithMessage resumes a prior headless claude session with a new
// user message and streams the result through the same message-
// persistence path as the initial invocation. Used by the SKY-141
// task-memory write-gate retry loop, and designed to be reusable by
// SKY-139's yield-to-user flow once that ticket lands.
//
// Callers pass the sessionID captured during the initial run (read
// from runs.session_id, populated by consumeClaudeStream), the
// cwd the original run used so the resumed subprocess sees the same
// worktree, and the user message to append to the conversation. The
// runID is reused so resumed messages append to the existing
// run_messages stream — the UI sees one coherent conversation.
//
// This helper does NOT update runs status. The caller manages
// lifecycle: the memory-gate retry loop keeps the run in its current
// state during retries and only finalizes once the gate passes or
// gives up. Mirroring the initial invocation's status updates here
// would produce double CompleteAgentRun writes with stale
// cost/duration fields overwriting the real totals.
func (s *Spawner) ResumeWithMessage(ctx context.Context, runID, sessionID, cwd, message string, opts ResumeOptions) (*ResumeOutcome, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("resume: missing session id")
	}
	if cwd == "" {
		return nil, fmt.Errorf("resume: missing cwd")
	}

	s.mu.Lock()
	model := s.model
	s.mu.Unlock()
	if opts.Model != "" {
		model = opts.Model
	}

	selfBin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve own binary path: %w", err)
	}

	args := []string{
		"-p", message,
		"--resume", sessionID,
		"--model", model,
		"--output-format", "stream-json",
		"--verbose",
		"--allowedTools", BuildAllowedTools(selfBin),
		"--max-turns", "100",
	}

	cmd := exec.Command("claude", args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "TRIAGE_FACTORY_RUN_ID="+runID, "TRIAGE_FACTORY_REVIEW_PREVIEW=1")
	// Preserve the initial run's GitHub repo context so gh subcommands
	// in the resumed session keep their implicit --repo default. Without
	// this, a resumed run on a GitHub task could suddenly fail any gh
	// invocation that relied on the env var set in runAgent.
	if opts.RepoEnv != "" {
		cmd.Env = append(cmd.Env, "TRIAGE_FACTORY_REPO="+opts.RepoEnv)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude resume: %w", err)
	}

	pgid := cmd.Process.Pid
	go func() {
		<-ctx.Done()
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
			// Best-effort; subprocess may have already exited
			_ = err
		}
	}()

	stream := newStreamState()
	completion, streamErr := s.consumeClaudeStream(stdout, runID, stream)

	waitErr := cmd.Wait()

	outcome := &ResumeOutcome{
		Completion: completion,
		StderrText: stderrBuf.String(),
	}
	if completion != nil {
		outcome.Result = parseAgentResult(completion.Result)
	}

	// A stream error with no completion means the subprocess produced
	// malformed output or died mid-stream. Surface it to the caller so
	// the gate can decide whether to retry or give up.
	if streamErr != nil && completion == nil {
		return outcome, fmt.Errorf("resume stream: %w", streamErr)
	}

	// A wait error without a captured completion is an involuntary
	// failure — the subprocess exited without sending a result event.
	// Either cancellation (via ctx) or a genuine crash.
	if waitErr != nil && completion == nil {
		if ctx.Err() != nil {
			return outcome, ctx.Err()
		}
		return outcome, fmt.Errorf("claude resume failed: %w (stderr: %s)", waitErr, stderrBuf.String())
	}

	return outcome, nil
}

// maxMemoryRetries is the hard cap on how many times the write-gate
// will resume a run to ask the agent to write its memory file. Chosen
// in the SKY-141 design: 0 retries is too strict (one missed write
// shouldn't discard work), 3+ is overkill (if the agent ignored the
// first correction, a third attempt is almost never the one that
// works). Not a config knob because no one needs to tune it per-run.
const maxMemoryRetries = 2

// memoryFileExists returns true iff the agent wrote ./task_memory/<runID>.md
// during the run. Used by the write-gate both before retrying (is another
// attempt needed?) and after (did the retry succeed?).
func memoryFileExists(cwd, runID string) bool {
	_, err := os.Stat(filepath.Join(cwd, "task_memory", runID+".md"))
	return err == nil
}

// readAgentMemoryFileOrEmpty returns the contents of the agent-written
// ./task_memory/<runID>.md, or "" when the file is absent (or the read
// fails for any reason — bad permissions, race with cleanup, etc.).
// Empty string is the documented signal for "agent didn't comply with
// the memory gate" and is what the caller passes to UpsertAgentMemory
// to record the gap in run_memory. Read errors that aren't a missing
// file get logged so they're not silent.
func readAgentMemoryFileOrEmpty(cwd, runID string) string {
	path := filepath.Join(cwd, "task_memory", runID+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[delegate] warning: failed to read memory file %s: %v", path, err)
		}
		return ""
	}
	return string(data)
}

// runMemoryGate enforces the pre-complete task_memory file requirement.
//
// If the agent wrote ./task_memory/<runID>.md during its initial
// invocation, returns the original completion unchanged. Otherwise
// resumes the session (up to maxMemoryRetries times) with a correction
// message and re-checks after each attempt. Completions from resumed
// sessions are merged into the returned completion so cost/duration/
// num_turns accounting reflects the full span of the run.
//
// The gate does not touch runs status — that remains the caller's
// responsibility. Side effects: (a) spawns resume subprocesses via
// ResumeWithMessage, whose messages land in run_messages via
// consumeClaudeStream's persistence, (b) logs progress for operator
// diagnosis.
//
// Model and repoEnv are passed in rather than read from live spawner
// state so the gate's retries use the same model and repo context as
// the initial invocation. If we read s.model at resume time, a
// concurrent UpdateCredentials could silently switch models mid-run.
//
// If no session id is available (shouldn't happen in practice because
// consumeClaudeStream persists the init event, but defensive), the gate
// logs and returns without retrying. The caller will see a missing
// memory file and flag memory_missing.
func (s *Spawner) runMemoryGate(
	ctx context.Context,
	runID, taskID, cwd string,
	initial *runCompletion,
	sessionID, model, repoEnv string,
) *runCompletion {
	if memoryFileExists(cwd, runID) {
		return initial
	}

	if sessionID == "" {
		log.Printf("[delegate] run %s: memory file missing and no session id available — cannot gate-retry", runID)
		return initial
	}

	resumeOpts := ResumeOptions{Model: model, RepoEnv: repoEnv}

	current := initial
	for attempt := 1; attempt <= maxMemoryRetries; attempt++ {
		log.Printf("[delegate] run %s: memory file missing after attempt %d, resuming", runID, attempt-1)
		msg := fmt.Sprintf(
			"You returned a completion JSON but did not write your memory file to ./task_memory/%s.md. "+
				"Write it now — one paragraph of what you did, one of why, one of what to try next "+
				"if this recurs — then return your completion JSON again.",
			runID,
		)
		outcome, err := s.ResumeWithMessage(ctx, runID, sessionID, cwd, msg, resumeOpts)
		if err != nil {
			log.Printf("[delegate] run %s: resume attempt %d failed: %v", runID, attempt, err)
			// Give up on further retries — the caller will mark
			// memory_missing. Don't wipe out the initial completion's
			// accounting just because the retry subprocess crashed.
			return current
		}
		if outcome.Completion != nil {
			current = mergeCompletion(current, outcome.Completion)
		}
		if memoryFileExists(cwd, runID) {
			return current
		}
	}

	return current
}

// mergeCompletion combines an initial completion event with one from a
// resumed session so final accounting reflects total cost, duration, and
// turn count across all invocations. The result text and stop_reason
// come from the resume (that's what the caller wants to report as the
// final outcome), but cost and turns are summed.
//
// If either the resume's Result or StopReason is empty, the base's
// values are preserved — partial resume outcomes shouldn't blank
// fields that were already populated.
func mergeCompletion(base, resume *runCompletion) *runCompletion {
	merged := *base
	merged.CostUSD += resume.CostUSD
	merged.DurationMs += resume.DurationMs
	merged.NumTurns += resume.NumTurns
	if resume.IsError {
		merged.IsError = true
	}
	if resume.Result != "" {
		merged.Result = resume.Result
	}
	if resume.StopReason != "" {
		merged.StopReason = resume.StopReason
	}
	return &merged
}

// materializePriorMemories writes any existing task_memory rows for the
// task into <cwd>/task_memory/<prior_run_id>.md as individual markdown
// files, so a fresh agent invocation sees what previous iterations on
// the same task have already tried. The agent is taught to read this
// directory by the envelope.
//
// Pattern: DB is the source of truth, we materialize into the worktree
// at startup, and ingest back on completion. The worktree is destroyed
// after every run, so these files never outlive their run on disk —
// only the DB rows do.
//
// Degrades gracefully: database errors, mkdir failures, or per-file
// write failures are logged but do not fail the run. An agent running
// without materialized priors is still useful, just without the
// cross-run memory benefit. This "advisory" posture only holds for
// the read side — the write-before-finish gate is enforced separately
// for NEW memories produced during the run.
func materializePriorMemories(database *sql.DB, cwd, entityID string) {
	memories, err := db.GetMemoriesForEntity(database, entityID)
	if err != nil {
		log.Printf("[delegate] warning: failed to load prior memories for entity %s: %v", entityID, err)
		return
	}
	if len(memories) == 0 {
		return
	}

	memDir := filepath.Join(cwd, "task_memory")
	if err := os.MkdirAll(memDir, 0755); err != nil {
		log.Printf("[delegate] warning: failed to create task_memory dir at %s: %v", memDir, err)
		return
	}

	written := 0
	for _, m := range memories {
		filename := filepath.Join(memDir, m.RunID+".md")
		if err := os.WriteFile(filename, []byte(m.Content), 0644); err != nil {
			log.Printf("[delegate] warning: failed to materialize task memory %s: %v", filename, err)
			continue
		}
		written++
	}
	if written > 0 {
		log.Printf("[delegate] materialized %d prior memories for entity %s", written, entityID)
	}
}

// resolvePrompt finds the mission text for a task from an explicit prompt ID.
// Manual delegation always requires the caller to pick a prompt; auto-delegation
// supplies the prompt_id from the trigger row.
func (s *Spawner) resolvePrompt(task domain.Task, explicitPromptID string) (string, string, error) {
	if explicitPromptID == "" {
		return "", "", fmt.Errorf("%w — select one from the prompt picker", ErrPromptUnspecified)
	}

	p, err := db.GetPrompt(s.database, explicitPromptID)
	if err != nil {
		return "", "", fmt.Errorf("failed to load prompt %s: %w", explicitPromptID, err)
	}
	if p == nil {
		return "", "", fmt.Errorf("%w: %s", ErrPromptNotFound, explicitPromptID)
	}
	return p.ID, p.Body, nil
}

// handleCancelled finalizes a run that exited via context cancel. wtPath
// is the worktree directory the run was using (empty for no-cwd Jira
// runs); we clean it up explicitly here in addition to runAgent's
// deferred cleanup so the bare-repo registration is pruned even if the
// goroutine returns through one of the early paths that doesn't reach
// the defer (e.g., setupErr before the defer is installed).
func (s *Spawner) handleCancelled(runID string, startTime time.Time, wtPath string) {
	if s.wasTakenOver(runID) {
		// Takeover owns the DB row, the worktree, and the broadcast from
		// here on — it needs the temp worktree to stay on disk until its
		// copy completes, then will explicitly remove it. The cancel
		// that woke us up was just the mechanism for stopping the
		// headless process; everything else is Takeover's job.
		return
	}
	elapsed := int(time.Since(startTime).Milliseconds())
	if err := db.CompleteAgentRun(s.database, runID, "cancelled", 0, elapsed, 0, "cancelled", "Cancelled by user"); err != nil {
		log.Printf("[delegate] warning: failed to record cancellation for run %s: %v", runID, err)
	}
	s.broadcastRunUpdate(runID, "cancelled")
	if wtPath != "" {
		// Best-effort cleanup; same rationale as the defer in runAgent.
		_ = worktree.RemoveAt(wtPath, runID)
	}
}

func (s *Spawner) updateStatus(runID, status string) {
	if _, err := s.database.Exec(`UPDATE runs SET status = ? WHERE id = ?`, status, runID); err != nil {
		log.Printf("[delegate] warning: failed to update status for run %s: %v", runID, err)
	}
	s.broadcastRunUpdate(runID, status)
}

// updateBreakerCounter is a no-op stub. The breaker is now query-based
// (see routing.Router + db.CountConsecutiveFailedRuns). Kept as a call site
// placeholder until all callers are cleaned up.
func (s *Spawner) updateBreakerCounter(taskID, triggerType, status string) {
	// Breaker is query-based now — no per-task counter to update.
	// See internal/routing/router.go and internal/db/tasks.go.
}

func (s *Spawner) failRun(runID, taskID, triggerType, errMsg string) {
	if s.wasTakenOver(runID) {
		// Takeover finalized this run; whatever error the goroutine
		// observed is downstream of the SIGKILL we sent it. Don't
		// overwrite taken_over with failed.
		return
	}
	log.Printf("[delegate] run %s failed: %s", runID, errMsg)

	if _, err := s.database.Exec(`UPDATE runs SET status = 'failed' WHERE id = ?`, runID); err != nil {
		log.Printf("[delegate] warning: failed to mark run %s as failed: %v", runID, err)
	}

	if _, err := db.InsertAgentMessage(s.database, domain.AgentMessage{
		RunID:   runID,
		Role:    "assistant",
		Subtype: "text",
		Content: "Error: " + errMsg,
		IsError: true,
	}); err != nil {
		log.Printf("[delegate] warning: failed to record failure message for run %s: %v", runID, err)
	}

	s.updateBreakerCounter(taskID, triggerType, "failed")
	s.broadcastRunUpdate(runID, "failed")

	// Surface as a sticky error toast so the user sees the failure even when
	// they're not watching the runs page. Truncate the message — full stderr
	// dumps don't fit in a toast card.
	toast.Error(s.wsHub, fmt.Sprintf("Run %s failed: %s", shortRunID(runID), truncateToastMsg(errMsg, 160)))
}

// truncateToastMsg caps an error message at maxLen runes with an ellipsis.
// Toasts show a short body; full errors belong in the runs log.
func truncateToastMsg(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen-1]) + "…"
}

func (s *Spawner) broadcastRunUpdate(runID, status string) {
	if s.wsHub == nil {
		return
	}
	s.wsHub.Broadcast(websocket.Event{
		Type:  "agent_run_update",
		RunID: runID,
		Data:  map[string]string{"status": status},
	})
}

func (s *Spawner) broadcastMessage(runID string, msg *domain.AgentMessage) {
	if s.wsHub == nil {
		return
	}
	s.wsHub.Broadcast(websocket.Event{
		Type:  "agent_message",
		RunID: runID,
		Data:  msg,
	})
}

type agentResult struct {
	Status  string         `json:"status"`
	Link    string         `json:"link"` // legacy — single URL
	Summary string         `json:"summary"`
	Links   map[string]any `json:"links"` // new — keyed URLs (pr_review, pr, jira_issues)
}

// PrimaryLink returns the most relevant URL from the result.
func (r *agentResult) PrimaryLink() string {
	if r.Link != "" {
		return r.Link
	}
	for _, key := range []string{"pr_review", "pr"} {
		if v, ok := r.Links[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	if v, ok := r.Links["jira_issues"]; ok {
		if arr, ok := v.([]any); ok && len(arr) > 0 {
			if s, ok := arr[0].(string); ok {
				return s
			}
		}
	}
	return ""
}

// parseAgentResult extracts the structured {status, link, summary} JSON from
// the agent's final message. Handles markdown fences, leading/trailing text.
func parseAgentResult(text string) *agentResult {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	var result agentResult
	if json.Unmarshal([]byte(text), &result) == nil && result.Summary != "" {
		return &result
	}

	stripped := text
	if idx := strings.Index(stripped, "```"); idx >= 0 {
		stripped = stripped[idx+3:]
		if nl := strings.Index(stripped, "\n"); nl >= 0 {
			stripped = stripped[nl+1:]
		}
		if end := strings.LastIndex(stripped, "```"); end >= 0 {
			stripped = stripped[:end]
		}
		stripped = strings.TrimSpace(stripped)
		if json.Unmarshal([]byte(stripped), &result) == nil && result.Summary != "" {
			return &result
		}
	}

	if start := strings.Index(text, "{"); start >= 0 {
		if end := strings.LastIndex(text, "}"); end > start {
			candidate := text[start : end+1]
			if json.Unmarshal([]byte(candidate), &result) == nil && result.Summary != "" {
				return &result
			}
		}
	}

	return nil
}

// buildPrompt composes: mission + envelope (scope, tools, task memory, completion contract).
// buildPrompt composes mission + envelope and interpolates all placeholders
// in one pass. See placeholders.go for the full catalog — every {{X}} in
// the mission or envelope gets resolved here, with unknown names falling
// through as literal braces so they're obvious to prompt authors on first
// run. metadataJSON is the primary event's metadata blob ("" is fine —
// event-derived placeholders just render empty).
func buildPrompt(task domain.Task, metadataJSON, mission, scope, toolsRef, binaryPath, runID string) string {
	// Compatibility shim: some early prompts were written with the literal
	// "triagefactory exec" prefix on CLI invocations, assuming the binary
	// was on PATH. The binary lives at an absolute path in the worktree
	// session, so rewrite those before interpolation. New prompts should
	// use {{BINARY_PATH}} directly.
	body := strings.ReplaceAll(mission, "triagefactory exec", binaryPath+" exec")
	full := body + "\n\n" + ai.EnvelopeTemplate
	return BuildPromptReplacer(task, metadataJSON, runID, binaryPath, scope, toolsRef).Replace(full)
}

func parseOwnerRepo(s string) (string, string) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}
