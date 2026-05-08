package delegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
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

	mu                    sync.Mutex
	ghClient              *ghclient.Client
	model                 string
	cancels               map[string]context.CancelFunc              // runID → cancel the entire run
	drainer               QueueDrainer                               // nil-safe; set post-construction via SetQueueDrainer
	takenOver             map[string]bool                            // runIDs claimed by Takeover. Sticky-on for the rest of the goroutine's lifetime even after rollback — clearing the entry would let late-firing goroutine gates race the takeover/abort lifecycle. Suppresses every cleanup path in runAgent so Takeover/abortTakeover own the row's terminal state.
	waitForClassification func(ctx context.Context, entityID string) // SKY-220 hook: blocks until the project classifier has decided this entity, or a timeout/ctx-cancel elapses. Nil-safe (test setups skip it). Wired in main.go via SetWaitForClassification — keeps internal/delegate from importing internal/projectclassify.

	agentToolsOnce  sync.Once
	agentToolsCache string
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

// SetWaitForClassification wires the SKY-220 hook that blocks the
// spawner until the project classifier has decided the entity (or
// the timeout / ctx fires). main.go provides the implementation so
// this package doesn't import projectclassify. Nil-safe — tests and
// any configuration without a classifier skip the wait entirely.
func (s *Spawner) SetWaitForClassification(fn func(ctx context.Context, entityID string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waitForClassification = fn
}

// awaitClassification calls the wait hook if one is configured. ctx
// is forwarded so the spawner's run cancellation / shutdown path
// breaks out of the wait early instead of blocking the full
// classifier timeout.
func (s *Spawner) awaitClassification(ctx context.Context, entityID string) {
	s.mu.Lock()
	fn := s.waitForClassification
	s.mu.Unlock()
	if fn != nil {
		fn(ctx, entityID)
	}
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

	if ok {
		cancel()
		return nil
	}

	// No active goroutine — the run may be parked in awaiting_input
	// with no subprocess to kill (SKY-139). Mark it cancelled directly
	// via DB. MarkAgentRunCancelledIfActive's status-NOT-IN filter
	// handles every non-terminal state, so this is also a defensive
	// catch for any other "no goroutine but row not terminal"
	// edge case.
	//
	// We also have to drain the per-entity firing queue ourselves on
	// terminal exit. The active-goroutine cancel paths drain via
	// their goroutine defer (Delegate's defer / ResumeAfterYield's
	// defer); a Cancel() that hits this DB-only path has no defer to
	// piggy-back on, so an auto-fired run cancelled while parked in
	// awaiting_input would leave the entity's firing queue stuck
	// until some other run on that entity terminated. Look up
	// triggerType + entityID before the flip so a concurrent task
	// delete can't strand us; drain only on a successful flip so we
	// don't double-drain a row another path already terminated.
	//
	// Done as a direct query rather than via GetAgentRun + GetTask
	// because GetAgentRun's SELECT doesn't include trigger_type
	// (it's not part of the API/UI projection), and we need both
	// fields atomically for the manual-run filter to work.
	var triggerType, entityID string
	if err := s.database.QueryRow(`
		SELECT COALESCE(r.trigger_type, ''), COALESCE(t.entity_id, '')
		FROM runs r LEFT JOIN tasks t ON t.id = r.task_id
		WHERE r.id = ?
	`, runID).Scan(&triggerType, &entityID); err != nil {
		// Row missing or query error — let the flip below decide
		// whether to surface that as "no active run" or proceed.
		// Drain just won't fire if entityID stays empty.
		_ = err
	}

	flipped, err := db.MarkAgentRunCancelledIfActive(s.database, runID, "user_cancelled", "Run cancelled by user")
	if err != nil {
		return fmt.Errorf("mark cancelled: %w", err)
	}
	if !flipped {
		return fmt.Errorf("no active run %s", runID)
	}
	s.broadcastRunUpdate(runID, "cancelled")
	if entityID != "" {
		s.notifyDrainer(triggerType, entityID)
	}
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

	// ErrReleaseNothingHeld — Release was called against a run that
	// isn't a held takeover (wrong status, or already released so
	// worktree_path is empty). 409 Conflict from the HTTP handler.
	ErrReleaseNothingHeld = errors.New("release: run is not a held takeover")
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
		return nil, fmt.Errorf("%w: run %s has no worktree path; cannot take over", ErrTakeoverInvalidState, runID)
	}
	// Jira lazy runs DO populate worktree_path (with the run-root, so
	// yield/resume can reuse it as the resume cwd). Takeover for those
	// would require copying the full run-root tree including all
	// per-repo worktrees as subdirs, plus rewriting any absolute paths
	// the agent recorded in its session — out of scope for now. Reject
	// explicitly via a task-source check rather than papering over with
	// an empty-path heuristic.
	task, err := db.GetTask(s.database, run.TaskID)
	if err != nil {
		return nil, fmt.Errorf("load task for takeover gate: %w", err)
	}
	if task != nil && task.EntitySource == "jira" {
		return nil, fmt.Errorf("%w: run %s is a Jira lazy delegation; multi-worktree takeover is not yet supported (use the user-respond / yield-resume flow instead)", ErrTakeoverInvalidState, runID)
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

// Release tears down a held takeover: removes the takeover worktree dir,
// reclaims per-PR config in the bare repo, drops the takeover dir's
// ~/.claude/projects entry, and flips the run row from "held" to
// "released" (status stays 'taken_over' for audit; worktree_path is
// cleared so the resume picker, banner and startup-cleanup-preserve
// queries all drop the row).
//
// Caller signals: pre-release validation failures return ErrReleaseNothingHeld
// (HTTP 409). Filesystem failures during teardown are surfaced as 5xx;
// the row stays held so a retry can finish the job rather than ending
// up half-cleaned with the row marked released.
//
// Order matters:
//
//  1. Resolve config (takeover base) and validate the worktree_path
//     lives under it — defense-in-depth against a poisoned DB row
//     pointing at an arbitrary directory.
//
//  2. Snapshot identity that's only available while the worktree
//     exists: branch name (via WorktreeBranch) and the symlink-
//     resolved cwd (via EvalSymlinks, captured at step 1's safety
//     check). Both are gone after RemoveAt.
//
//  3. Remove the worktree dir + deregister from the bare. From this
//     moment forward, the next delegated run on the same PR can
//     fetch into the branch ref — the load-bearing step. If this
//     fails, return early and leave EVERYTHING ELSE intact (DB row
//     stays held, projects entry stays put) so a retry can resume
//     with the user's resume command still functional.
//
//  4. CleanupPRConfig — reclaim the bare's per-PR remote + branch
//     tracking. Best-effort; SweepStaleForkPRConfig is the next
//     bootstrap's backstop if this fails.
//
//  5. Drop the ~/.claude/projects entry. Done AFTER RemoveAt so a
//     RemoveAt failure doesn't leave the user with neither a
//     working takeover dir nor the resume JSONL needed to retry.
//     Uses the resolved-path variant (RemoveClaudeProjectDirForResolved)
//     since the cwd no longer exists on disk by this point —
//     EvalSymlinks-based variants would silently no-op.
//
//  6. Flip the DB row. Done LAST so a teardown failure leaves the row
//     held and the action retryable.
//
// We deliberately don't clear s.takenOver[runID] (sticky-by-design;
// see Takeover's comment) — a released run never spawns another
// goroutine, so the flag is harmless once set.
func (s *Spawner) Release(runID string) error {
	run, err := db.GetAgentRun(s.database, runID)
	if err != nil {
		return fmt.Errorf("load run: %w", err)
	}
	if run == nil {
		return fmt.Errorf("%w: run %s not found", ErrReleaseNothingHeld, runID)
	}
	if run.Status != "taken_over" || run.WorktreePath == "" {
		return fmt.Errorf("%w: run %s (status=%q, worktree_path=%q)", ErrReleaseNothingHeld, runID, run.Status, run.WorktreePath)
	}

	takeoverPath := run.WorktreePath

	// (1) Resolve the takeover base early. Two reasons:
	//
	//   - Path-safety check: a poisoned worktree_path (DB row tampered
	//     with, or a bug elsewhere wrote an arbitrary path into the
	//     column) would otherwise let RemoveAt nuke a directory we
	//     don't own. Refuse the release if takeoverPath isn't under
	//     the configured takeover base.
	//
	//   - Projects-dir cleanup needs the base for its own safety rail.
	//
	// If config can't be loaded (broken on-disk DB or filesystem), we
	// refuse rather than barrel ahead without the safety check —
	// handleAgentTakeover already requires this to work, so a release
	// against a takeover that successfully created should also have
	// access to the same config.
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config for release: %w", err)
	}
	takeoverBase, err := cfg.Server.ResolvedTakeoverDir()
	if err != nil {
		return fmt.Errorf("resolve takeover base: %w", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(takeoverPath)
	if err != nil {
		return fmt.Errorf("resolve takeover path %s: %w", takeoverPath, err)
	}
	resolvedBase, err := filepath.EvalSymlinks(takeoverBase)
	if err != nil {
		return fmt.Errorf("resolve takeover base %s: %w", takeoverBase, err)
	}
	rel, err := filepath.Rel(resolvedBase, resolvedPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("release: takeover path %s is not under takeover base %s; refusing teardown", resolvedPath, resolvedBase)
	}

	// (2) Capture branch name from the takeover dir. Best-effort: empty
	// branch is acceptable (CleanupPRConfig handles "" — it just skips
	// the own-repo branch.<headRef>.* and `branch -D headRef` blocks,
	// while still reclaiming the synthetic triagefactory/pr-<n> remote
	// + branch). Detached HEAD also returns "" via WorktreeBranch.
	headBranch, _ := worktree.WorktreeBranch(takeoverPath)

	// Look up task → entity to derive owner/repo/PR for CleanupPRConfig.
	task, err := db.GetTask(s.database, run.TaskID)
	if err != nil {
		return fmt.Errorf("load task for release: %w", err)
	}
	var owner, repo string
	var prNumber int
	if task != nil && task.EntitySource == "github" {
		repoStr := task.EntitySourceID
		if idx := strings.LastIndex(repoStr, "#"); idx >= 0 {
			repoStr = repoStr[:idx]
		}
		owner, repo = parseOwnerRepo(repoStr)
		if idx := strings.LastIndex(task.EntitySourceID, "#"); idx >= 0 {
			fmt.Sscanf(task.EntitySourceID[idx+1:], "%d", &prNumber)
		}
	}

	// (3) Remove the worktree dir + bare-side registration FIRST. If
	// this fails, return early with everything else intact: the
	// projects-dir entry (and the JSONL inside it) is still on disk,
	// so the user's `claude --resume` keeps working and they can
	// retry the release. If we'd cleaned the projects entry first
	// and then failed RemoveAt, we'd be in a half-released state
	// where the run is "held" for retry but the resume history is
	// already gone.
	if err := worktree.RemoveAt(takeoverPath, runID); err != nil {
		return fmt.Errorf("remove takeover worktree %s: %w", takeoverPath, err)
	}

	// (4) Per-PR config cleanup. Best-effort — failures here leak a
	// stale remote + branch into the bare's config, which the next
	// bootstrap's SweepStaleForkPRConfig reclaims. Don't block the
	// release on it.
	if owner != "" && repo != "" && prNumber > 0 {
		worktree.CleanupPRConfig(owner, repo, headBranch, prNumber)
	}

	// (5) Drop the ~/.claude/projects entry now that the worktree dir
	// is gone. Uses the resolved-path variant because the cwd no
	// longer exists on disk — RemoveClaudeProjectDir's internal
	// EvalSymlinks would fail and silently no-op. resolvedPath was
	// captured at step (1) before RemoveAt destroyed the path.
	worktree.RemoveClaudeProjectDirForResolved(resolvedPath)

	// (6) DB flip. The status='taken_over' AND worktree_path != ''
	// guard inside MarkAgentRunReleased makes a concurrent double-call
	// idempotent: the second one returns ok=false and we return a
	// 409-equivalent.
	ok, err := db.MarkAgentRunReleased(s.database, runID)
	if err != nil {
		return fmt.Errorf("mark released: %w", err)
	}
	if !ok {
		return fmt.Errorf("%w: run %s (DB row no longer matches release preconditions)", ErrReleaseNothingHeld, runID)
	}

	s.broadcastRunUpdate(runID, "taken_over")
	toast.Info(s.wsHub, fmt.Sprintf("Released takeover: run %s", shortRunID(runID)))

	return nil
}

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

// runAgent is the generic agent execution loop. Works for any task type.
func (s *Spawner) runAgent(ctx context.Context, runID string, task domain.Task, mission string, cfg runConfig, startTime time.Time, model string, triggerType string) {
	if cfg.hasWT {
		// GitHub PR cleanup. Best-effort cleanup on return; the worktree ID is unique per run
		// so a failed remove just leaves a dangling directory under _worktrees.
		// Skipped when the run was taken over — Takeover() needs the worktree
		// to still exist for its copy and explicitly cleans up afterward.
		defer func() {
			if s.wasTakenOver(runID) {
				// Taken-over runs leave their worktree in place for the
				// user's interactive session; don't touch the per-PR
				// config either, since the takeover dir still uses
				// head-<n> for push. SweepStaleForkPRConfig reclaims
				// that config on the next bootstrap once the takeover
				// dir is gone.
				return
			}
			// Capture the RemoveAt error rather than discarding it.
			// If the worktree dir failed to remove, the worktree is
			// still on disk and still attached to the bare's branch
			// tracking — stripping the per-PR config out from under a
			// surviving checkout would break its push/pull. Skip
			// cleanup in that case; the next bootstrap sweep will
			// reclaim the orphan once the worktree is gone.
			rmErr := worktree.RemoveAt(cfg.wtPath, runID)
			if rmErr != nil {
				log.Printf("[delegate] worktree remove failed for %s; skipping per-PR config cleanup: %v", runID, rmErr)
				return
			}
			// CleanupPRConfig uses a detached internal context so
			// cancellation of the agent's ctx (timeout, server
			// shutdown) doesn't short-circuit the cleanup.
			if cfg.prNumber > 0 && cfg.owner != "" && cfg.repo != "" {
				worktree.CleanupPRConfig(cfg.owner, cfg.repo, cfg.headRef, cfg.prNumber)
			}
		}()
	} else if cfg.runRoot != "" {
		// Jira lazy cleanup: the agent materialized zero or more worktrees
		// under cfg.runRoot via `workspace add`. Iterate run_worktrees,
		// nuke each, then remove the run-root parent. Same takeover gate
		// as the GitHub branch above — multi-worktree takeover isn't
		// implemented yet, but the Takeover() rejection on empty
		// runs.worktree_path catches Jira runs before they get here, so
		// the gate is defensive rather than load-bearing.
		defer func() {
			if s.wasTakenOver(runID) {
				return
			}
			rows, err := db.GetRunWorktrees(s.database, runID)
			if err != nil {
				log.Printf("[delegate] run %s: list run_worktrees for cleanup: %v", runID, err)
			} else {
				// Use a detached context so cleanup is not skipped if the
				// agent ctx has already been canceled.
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				for _, w := range rows {
					rmErr := worktree.RemoveAt(w.Path, runID)
					if rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
						log.Printf("[delegate] run %s: remove worktree %s: %v", runID, w.Path, rmErr)
						continue
					}
					if _, delErr := s.database.ExecContext(cleanupCtx, "DELETE FROM run_worktrees WHERE run_id = ? AND path = ?", runID, w.Path); delErr != nil {
						log.Printf("[delegate] run %s: delete run_worktrees row for %s: %v", runID, w.Path, delErr)
					}
				}
			}
			worktree.RemoveRunRoot(runID)
		}()
	}

	// Initial cwd for the child claude. Always the run-root: the worktree
	// itself for GitHub PR runs, or the throwaway parent for Jira lazy runs
	// (the agent cd's into a per-repo subdir after `workspace add`).
	claudeCwd := cfg.wtPath
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

	// Materialize any prior task memories into ./_scratch/entity-memory/
	// so the agent sees what previous iterations on this task have
	// already tried. The directory is git-excluded by writeLocalExcludes
	// (managedExcludePatterns in internal/worktree/worktree.go) so
	// nothing leaks into the PR.
	materializePriorMemories(s.database, claudeCwd, task.EntityID)

	// SKY-219: copy the entity's project knowledge-base into
	// ./_scratch/project-knowledge/ if the entity is assigned to a
	// project, so the agent has curated project context available
	// alongside prior memories.
	materializeProjectKnowledge(claudeCwd, cfg.projectID)

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

	extraEnv := []string{
		"TRIAGE_FACTORY_RUN_ID=" + runID,
		"TRIAGE_FACTORY_REVIEW_PREVIEW=1",
		"TRIAGE_FACTORY_RUN_ROOT=" + cfg.runRoot, // Set for both sources so the memory-gate retry message can reference an absolute _scratch/entity-memory path that resolves regardless of which worktree the agent has cd'd into.
	}
	// Set TRIAGE_FACTORY_REPO when the run has a resolved GitHub repo context
	// (GitHub PR runs only) so gh subcommands can default to the right target
	// without the agent needing to pass --repo. Jira lazy runs leave it unset:
	// after the agent cd's into a worktree materialized by `workspace add`,
	// cmd/exec/gh/repo.go:resolveRepo falls through to .git/config, which is
	// the correct per-repo answer.
	if cfg.owner != "" && cfg.repo != "" {
		extraEnv = append(extraEnv, "TRIAGE_FACTORY_REPO="+cfg.owner+"/"+cfg.repo)
	}

	s.updateStatus(runID, "running")

	log.Printf("[delegate] claude starting for run %s (cwd: %s)", runID, claudeCwd)
	outcome, err := agentproc.Run(ctx, agentproc.RunOptions{
		Cwd:          claudeCwd,
		Model:        model,
		Message:      prompt,
		AllowedTools: agentproc.BuildAllowedToolsWithExtras(selfBin, cfg.extraAllowedTools),
		MaxTurns:     100,
		ExtraEnv:     extraEnv,
		TraceID:      runID,
	}, newRunSink(s, runID))

	// If Takeover() flipped the takenOver flag while we were streaming,
	// every code path below — completion ingestion, status updates, fail
	// paths, toasts — would step on the takeover lifecycle. Bail out
	// silently: Takeover owns the DB row and the worktree from here on.
	if s.wasTakenOver(runID) {
		return
	}

	if outcome != nil && outcome.Result != nil {
		s.processCompletion(ctx, runID, task, outcome.Result, claudeCwd, outcome.SessionID, model, cfg.owner, cfg.repo, triggerType, cfg.extraAllowedTools)
		return
	}

	if err != nil {
		if ctx.Err() != nil {
			s.handleCancelled(runID, startTime, cfg.wtPath)
			return
		}
		stderr := ""
		if outcome != nil {
			stderr = outcome.Stderr
		}
		s.failRun(runID, task.ID, triggerType, fmt.Sprintf("%v\nstderr: %s", err, stderr))
		return
	}

	s.failRun(runID, task.ID, triggerType, "claude exited cleanly without producing a result event")
}

// processCompletion handles the post-stream branching for any Claude
// invocation (initial run or yield-resume): if the parsed envelope is
// a yield, park the run in awaiting_input; otherwise run the memory
// gate and finalize the run as terminal. Shared between runAgent and
// ResumeAfterYield so a yield-then-resume run lands in identical
// terminal state to a run that completed in one shot — same memory
// gate, same toast, same task-done bookkeeping. SKY-139.
//
// The caller is responsible for draining any subprocess state
// (the agentproc.Run path waits internally); this helper only
// operates on the parsed completion.
func (s *Spawner) processCompletion(
	ctx context.Context,
	runID string,
	task domain.Task,
	completion *agentproc.Result,
	claudeCwd, sessionID, model, owner, repo, triggerType, extraAllowedTools string,
) {
	// Yield branch (SKY-139): the agent emitted status:"yield" to
	// pause the run for user input rather than terminating. Skip the
	// memory gate (the agent isn't terminating; the gate runs at real
	// completion) and skip CompleteAgentRun. Park the run in
	// awaiting_input; the respond endpoint reopens the session via
	// ResumeAfterYield when the user answers.
	//
	// IsError takes precedence: a Claude-side error (e.g. max-turns
	// hit) that happens to carry yield-shaped JSON is still a failure,
	// not an intentional pause.
	if !completion.IsError {
		if parsed := parseAgentResult(completion.Result); parsed != nil && parsed.Status == "yield" && parsed.Yield != nil {
			if err := s.persistYield(runID, parsed.Yield, completion); err != nil {
				log.Printf("[delegate] failed to persist yield for run %s: %v", runID, err)
				s.failRun(runID, task.ID, triggerType, "failed to record yield: "+err.Error())
			}
			return
		}
	}

	// Enforce the pre-complete entity-memory write gate. If the agent
	// returned a completion JSON without writing
	// ./_scratch/entity-memory/<runID>.md, resume the session with a
	// correction message (up to 2 retries).
	// Retries that produce new completions are merged into the totals
	// so cost/duration accounting reflects the full invocation, not
	// just the initial call.
	//
	// Pass model + repoEnv explicitly rather than letting the gate
	// read live spawner state, so a concurrent UpdateCredentials
	// can't silently switch models or drop repo context mid-run.
	repoEnv := ""
	if owner != "" && repo != "" {
		repoEnv = owner + "/" + repo
	}
	completion = s.runMemoryGate(ctx, runID, task.ID, claudeCwd, completion, sessionID, model, repoEnv, extraAllowedTools)

	// Unconditional upsert of the run_memory row at termination
	// (SKY-204): row presence === "termination passed through the
	// memory gate", agent_content NULL === "agent didn't comply with
	// the gate after retries" (UpsertAgentMemory normalizes
	// empty/whitespace input to NULL on the way in).
	agentContent, fileState := readAgentMemoryFile(claudeCwd, runID)
	if err := db.UpsertAgentMemory(s.database, runID, task.EntityID, agentContent); err != nil {
		log.Printf("[delegate] warning: failed to upsert memory for run %s: %v", runID, err)
	}
	switch fileState {
	case memoryFileMissing:
		log.Printf("[delegate] run %s: memory file missing after gate retries (agent_content NULL)", runID)
	case memoryFileEmpty:
		log.Printf("[delegate] run %s: memory file present but empty after gate retries (agent_content NULL)", runID)
	case memoryFileReadErr:
		log.Printf("[delegate] run %s: memory file unreadable after gate retries (agent_content NULL)", runID)
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
		// Two side-tables can park a completed run in pending_approval:
		//   - pending_reviews: agent ran `pr submit-review` under
		//     TRIAGE_FACTORY_REVIEW_PREVIEW=1, queued the review for
		//     human approval.
		//   - pending_prs: agent ran `pr create` under the same flag,
		//     queued the PR for human approval.
		// Either gate flips the run to pending_approval; the user
		// approves via the UI and the server flips back to completed.
		// Frontend distinguishes by which side-table has a row (the
		// /api/agent/runs/{id} response carries pending_kind).
		hasPending := false
		if pendingReview, _ := db.PendingReviewByRunID(s.database, runID); pendingReview != nil {
			hasPending = true
		} else if pendingPR, _ := db.PendingPRByRunID(s.database, runID); pendingPR != nil {
			hasPending = true
		}
		if hasPending {
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
}

// persistYield records an agent yield request, accumulates the partial
// invocation totals onto the run row, and parks the run in
// awaiting_input. SKY-139.
//
// The status flip is guarded against concurrent terminal flips
// (cancellation, takeover) by MarkAgentRunAwaitingInput's
// status-NOT-IN filter. If the run already reached a terminal state
// while the agent was emitting the yield envelope (rare but possible
// — a user cancel raced the stream's last line), we still record the
// yield_request message for transcript completeness but skip the
// status flip and the toast. The terminal status the racing path set
// stands.
func (s *Spawner) persistYield(runID string, req *domain.YieldRequest, completion *agentproc.Result) error {
	if err := db.AddAgentRunPartialTotals(s.database, runID, completion.CostUSD, completion.DurationMs, completion.NumTurns); err != nil {
		log.Printf("[delegate] warning: failed to record partial totals for run %s: %v", runID, err)
	}

	msg, err := db.InsertYieldRequest(s.database, runID, req)
	if err != nil {
		return fmt.Errorf("insert yield request: %w", err)
	}
	s.broadcastMessage(runID, msg)

	flipped, err := db.MarkAgentRunAwaitingInput(s.database, runID)
	if err != nil {
		return fmt.Errorf("mark awaiting_input: %w", err)
	}
	if !flipped {
		// Terminal status was already set by a racing path (cancel,
		// takeover). The yield_request message is recorded for
		// transcript completeness but the run ends in whatever
		// terminal state the racing path chose; no toast or
		// broadcast needed (the racing path already broadcast).
		return nil
	}
	s.broadcastRunUpdate(runID, "awaiting_input")
	toast.Info(s.wsHub, fmt.Sprintf("Run %s waiting for response", shortRunID(runID)))
	return nil
}

// ErrYieldNotResumable is returned by ResumeAfterYield when the run
// can't be resumed in its current state — typically a concurrent
// cancel or takeover flipped it terminal between the handler's
// validation read and our status flip. The respond endpoint maps
// this to 409 Conflict so the client can refresh and see the actual
// state. SKY-139.
var ErrYieldNotResumable = errors.New("yield: run not in awaiting_input")

// ResumeAfterYield is the entry point used by the respond endpoint
// after the user records an answer to a yield. This method:
//  1. validates the run is resumable (session id, worktree path, task)
//  2. registers a cancellation handle in s.cancels[runID]
//  3. flips status awaiting_input → running (with race guard)
//  4. spawns the goroutine that re-invokes Claude with the user's
//     plain-text response and runs the resulting completion through
//     the same processCompletion path the initial run uses
//
// agentMessage is the plain-text rendering of the user's response
// shaped by domain.RenderYieldResponseForAgent.
//
// Cancel-during-resume is closed by ordering: the cancel handle is in
// place before the status flip, so any Cancel() arriving after the
// flip finds the registered ctx and calls cancel() rather than
// falling through to the DB-write path. The resume goroutine writes
// its own terminal cancelled status when it observes ctx.Err() —
// the registered-cancel path doesn't write to the DB itself, so
// without that we'd leak a "cancelled but row says running" state.
//
// SKY-139.
func (s *Spawner) ResumeAfterYield(runID, agentMessage string) error {
	run, err := db.GetAgentRun(s.database, runID)
	if err != nil {
		return fmt.Errorf("load run: %w", err)
	}
	if run == nil {
		return fmt.Errorf("run not found")
	}
	if run.SessionID == "" {
		return fmt.Errorf("run has no session id; cannot resume")
	}
	if run.WorktreePath == "" {
		return fmt.Errorf("run has no worktree path; cannot resume")
	}
	task, err := db.GetTask(s.database, run.TaskID)
	if err != nil {
		return fmt.Errorf("load task: %w", err)
	}
	if task == nil {
		return fmt.Errorf("task not found for run")
	}

	// Resolve owner/repo for repoEnv. Best-effort: Jira-only runs have
	// no resolvable repo and the resumed subprocess simply runs
	// without TRIAGE_FACTORY_REPO, the same way Jira-no-match runs do
	// today.
	owner, repo := "", ""
	entity, err := db.GetEntity(s.database, task.EntityID)
	if err == nil && entity != nil {
		owner, repo = parseOwnerRepo(entity.SourceID)
	}

	// Resolve extra allowed tools from the prompt used for this run.
	var extraTools string
	if run.PromptID != "" {
		if p, err := db.GetPrompt(s.database, run.PromptID); err == nil && p != nil {
			extraTools = s.collectExtraTools(p.AllowedTools)
		}
	}

	// Capture state needed inside the goroutine.
	sessionID := run.SessionID
	cwd := run.WorktreePath
	model := run.Model
	taskCopy := *task
	triggerType := run.TriggerType
	if triggerType == "" {
		triggerType = "manual"
	}

	// Step 1: register the cancel handle synchronously. Once this
	// runs, a concurrent Cancel(runID) finds the entry and calls
	// cancel() on the ctx instead of falling through to the
	// MarkAgentRunCancelledIfActive DB-write path. The goroutine
	// observes ctx.Err() and writes the terminal cancelled status
	// itself.
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	if _, ok := s.cancels[runID]; ok {
		s.mu.Unlock()
		cancel()
		// Should not happen for awaiting_input (the initial
		// goroutine exited when it parked the run); defend against
		// a double-respond or a stale entry.
		return fmt.Errorf("run already has an active goroutine")
	}
	s.cancels[runID] = cancel
	s.mu.Unlock()

	// Step 2: flip status awaiting_input → running. This must happen
	// AFTER cancel registration: if the order is reversed, a Cancel()
	// arriving in the gap sees no goroutine, falls through to the DB
	// path, and races the resume into the "row cancelled but
	// goroutine still running" state the review bot flagged. With
	// the cancel handle already in place, any Cancel() now hits
	// cancel(ctx) and the goroutine handles the terminal write.
	flipped, err := db.MarkAgentRunResuming(s.database, runID)
	if err != nil {
		s.mu.Lock()
		delete(s.cancels, runID)
		s.mu.Unlock()
		cancel()
		return fmt.Errorf("flip status: %w", err)
	}
	if !flipped {
		s.mu.Lock()
		delete(s.cancels, runID)
		s.mu.Unlock()
		cancel()
		return ErrYieldNotResumable
	}
	s.broadcastRunUpdate(runID, "running")

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.cancels, runID)
			s.mu.Unlock()
			cancel()
			// Drain the per-entity firing queue on terminal exit —
			// matches the initial-run defer in Delegate so a yield
			// resume that lands the run terminal still flushes any
			// queued auto-firings for the same entity.
			s.notifyDrainer(triggerType, taskCopy.EntityID)
		}()

		// markCancelled writes the terminal cancelled status iff the
		// run is still non-terminal. The registered-handle Cancel()
		// path doesn't touch the DB; this goroutine owns the
		// terminal write any time it observes ctx.Err().
		markCancelled := func() {
			ok, _ := db.MarkAgentRunCancelledIfActive(s.database, runID, "user_cancelled", "Run cancelled by user")
			if ok {
				s.broadcastRunUpdate(runID, "cancelled")
			}
		}

		// Cancel raced before the goroutine scheduled. Write the
		// cancelled status ourselves and exit without invoking
		// Claude.
		if ctx.Err() != nil {
			markCancelled()
			return
		}

		repoEnv := ""
		if owner != "" && repo != "" {
			repoEnv = owner + "/" + repo
		}

		outcome, err := s.ResumeWithMessage(ctx, runID, sessionID, cwd, agentMessage, ResumeOptions{
			Model:             model,
			RepoEnv:           repoEnv,
			ExtraAllowedTools: extraTools,
		})
		if ctx.Err() != nil {
			// User cancelled mid-resume. ResumeWithMessage SIGKILLed
			// the subprocess via its own ctx.Done() watcher; we own
			// the terminal status write.
			markCancelled()
			return
		}
		if err != nil {
			s.failRun(runID, taskCopy.ID, triggerType, "resume after yield failed: "+err.Error())
			return
		}
		if outcome == nil || outcome.Completion == nil {
			s.failRun(runID, taskCopy.ID, triggerType, "resume after yield produced no completion")
			return
		}

		s.processCompletion(ctx, runID, taskCopy, outcome.Completion, cwd, sessionID, model, owner, repo, triggerType, extraTools)
	}()
	return nil
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

	// ExtraAllowedTools carries the prompt/agent-derived tool extensions
	// so a resumed session has the same --allowedTools as the initial
	// invocation. Without this, MCP tools allowed on the first run
	// would be rejected on resume.
	ExtraAllowedTools string
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
	Completion *agentproc.Result
	Result     *agentResult
	StderrText string
}

// ResumeWithMessage resumes a prior headless claude session with a new
// user message and streams the result through the same message-
// persistence path as the initial invocation. Used by the SKY-141
// task-memory write-gate retry loop and the SKY-139 yield-to-user flow.
//
// Callers pass the sessionID captured during the initial run (read
// from runs.session_id, populated on the runSink during the original
// invocation), the cwd the original run used so the resumed
// subprocess sees the same worktree, and the user message to append
// to the conversation. The runID is reused so resumed messages append
// to the existing run_messages stream — the UI sees one coherent
// conversation.
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

	extraEnv := []string{
		"TRIAGE_FACTORY_RUN_ID=" + runID,
		"TRIAGE_FACTORY_REVIEW_PREVIEW=1",
		// Mirror runAgent's TRIAGE_FACTORY_RUN_ROOT setting. The resume
		// cwd IS the original run-root (runAgent passed runRoot as the
		// agentproc Cwd; for GitHub PR runs the worktree IS the run-root,
		// for Jira lazy runs the run-root is the throwaway parent of
		// per-repo worktrees). Without this, the memory-gate retry
		// message — which now references
		// $TRIAGE_FACTORY_RUN_ROOT/_scratch/entity-memory/ for
		// absolute-path resilience across `cd`s — would resolve to
		// an empty string in the resumed shell and the agent couldn't
		// follow the retry instructions. Same env shape as the initial
		// invocation so the agent sees a consistent environment across
		// every prompt of the conversation.
		"TRIAGE_FACTORY_RUN_ROOT=" + cwd,
	}
	// Preserve the initial run's GitHub repo context so gh subcommands
	// in the resumed session keep their implicit --repo default. Without
	// this, a resumed run on a GitHub task could suddenly fail any gh
	// invocation that relied on the env var set in runAgent.
	if opts.RepoEnv != "" {
		extraEnv = append(extraEnv, "TRIAGE_FACTORY_REPO="+opts.RepoEnv)
	}

	apOutcome, runErr := agentproc.Run(ctx, agentproc.RunOptions{
		Cwd:          cwd,
		Model:        model,
		SessionID:    sessionID,
		Message:      message,
		AllowedTools: agentproc.BuildAllowedToolsWithExtras(selfBin, opts.ExtraAllowedTools),
		MaxTurns:     100,
		ExtraEnv:     extraEnv,
		TraceID:      runID,
	}, newRunSink(s, runID))

	outcome := &ResumeOutcome{}
	if apOutcome != nil {
		outcome.Completion = apOutcome.Result
		outcome.StderrText = apOutcome.Stderr
		if apOutcome.Result != nil {
			outcome.Result = parseAgentResult(apOutcome.Result.Result)
		}
	}

	if runErr != nil && (apOutcome == nil || apOutcome.Result == nil) {
		// agentproc.Run returns ctx.Err() directly when ctx triggered
		// the kill before any completion was captured; preserve that
		// shape so the SKY-139 yield-resume goroutine's ctx.Err()
		// check still routes through markCancelled.
		if ctx.Err() != nil {
			return outcome, ctx.Err()
		}
		return outcome, fmt.Errorf("claude resume failed: %w (stderr: %s)", runErr, outcome.StderrText)
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

// memoryFileExists returns true iff the agent wrote
// ./_scratch/entity-memory/<runID>.md during the run. Used by the
// write-gate both before retrying (is another attempt needed?) and
// after (did the retry succeed?).
func memoryFileExists(cwd, runID string) bool {
	_, err := os.Stat(filepath.Join(cwd, "_scratch", "entity-memory", runID+".md"))
	return err == nil
}

// memoryFileState distinguishes the three reasons readAgentMemoryFile
// returns no usable content. They all map to the same DB signal
// (UpsertAgentMemory normalizes empty/whitespace to NULL agent_content
// === "agent didn't comply with the gate"), but each carries different
// diagnostic value when something looks wrong post-run, so the gate
// teardown logs them distinctly.
type memoryFileState int

const (
	memoryFilePresent memoryFileState = iota // file exists, has non-whitespace content
	memoryFileMissing                        // file does not exist on disk
	memoryFileEmpty                          // file exists but is empty / whitespace-only
	memoryFileReadErr                        // file exists, read failed (permissions, race, etc.)
)

// readAgentMemoryFile returns the agent-written
// ./_scratch/entity-memory/<runID>.md content along with a state
// classification. The content string is empty for every non-Present
// state — callers pass it straight to UpsertAgentMemory either way,
// but inspect the state to log distinctly rather than collapsing every
// form of noncompliance to the same line. Read errors that aren't a
// missing file are logged at the read site so they aren't lost when
// the caller picks a higher-level message.
func readAgentMemoryFile(cwd, runID string) (string, memoryFileState) {
	path := filepath.Join(cwd, "_scratch", "entity-memory", runID+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", memoryFileMissing
		}
		log.Printf("[delegate] warning: failed to read memory file %s: %v", path, err)
		return "", memoryFileReadErr
	}
	content := string(data)
	if strings.TrimSpace(content) == "" {
		return "", memoryFileEmpty
	}
	return content, memoryFilePresent
}

// runMemoryGate enforces the pre-complete entity-memory file requirement.
//
// If the agent wrote ./_scratch/entity-memory/<runID>.md during its initial
// invocation, returns the original completion unchanged. Otherwise
// resumes the session (up to maxMemoryRetries times) with a correction
// message and re-checks after each attempt. Completions from resumed
// sessions are merged into the returned completion so cost/duration/
// num_turns accounting reflects the full span of the run.
//
// The gate does not touch runs status — that remains the caller's
// responsibility. Side effects: (a) spawns resume subprocesses via
// ResumeWithMessage, whose messages land in run_messages via the
// runSink, (b) logs progress for operator diagnosis.
//
// Model and repoEnv are passed in rather than read from live spawner
// state so the gate's retries use the same model and repo context as
// the initial invocation. If we read s.model at resume time, a
// concurrent UpdateCredentials could silently switch models mid-run.
//
// If no session id is available (shouldn't happen in practice because
// the runSink persists the init event, but defensive), the gate
// logs and returns without retrying. The caller will see a missing
// memory file and flag memory_missing.
func (s *Spawner) runMemoryGate(
	ctx context.Context,
	runID, taskID, cwd string,
	initial *agentproc.Result,
	sessionID, model, repoEnv, extraAllowedTools string,
) *agentproc.Result {
	if memoryFileExists(cwd, runID) {
		return initial
	}

	if sessionID == "" {
		log.Printf("[delegate] run %s: memory file missing and no session id available — cannot gate-retry", runID)
		return initial
	}

	resumeOpts := ResumeOptions{Model: model, RepoEnv: repoEnv, ExtraAllowedTools: extraAllowedTools}

	current := initial
	for attempt := 1; attempt <= maxMemoryRetries; attempt++ {
		log.Printf("[delegate] run %s: memory file missing after attempt %d, resuming", runID, attempt-1)
		msg := fmt.Sprintf(
			"You returned a completion JSON but did not write your memory file to "+
				"$TRIAGE_FACTORY_RUN_ROOT/_scratch/entity-memory/%s.md. Write it now using the "+
				"absolute path (the env var resolves to the run-root regardless of "+
				"which worktree you have cd'd into) — one paragraph of what you did, "+
				"one of why, one of what to try next if this recurs — then return "+
				"your completion JSON again.",
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
			current = agentproc.MergeResult(current, outcome.Completion)
		}
		if memoryFileExists(cwd, runID) {
			return current
		}
	}

	return current
}

// materializePriorMemories writes any existing run_memory rows for the
// task into <cwd>/_scratch/entity-memory/<prior_run_id>.md as individual
// markdown files, so a fresh agent invocation sees what previous
// iterations on the same task have already tried. The agent is taught
// to read this directory by the envelope.
//
// The directory is created unconditionally — even on the very first run
// when there are no priors. Two reasons: the prompt instructs the agent
// to `ls _scratch/entity-memory/` early (fails noisily without the dir),
// and the memory-gate retry message tells the agent to write to
// `$TRIAGE_FACTORY_RUN_ROOT/_scratch/entity-memory/<run>.md` (which fails
// on a missing parent dir unless the agent guesses to mkdir first).
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
	memDir := filepath.Join(cwd, "_scratch", "entity-memory")
	if err := os.MkdirAll(memDir, 0755); err != nil {
		log.Printf("[delegate] warning: failed to create entity-memory dir at %s: %v", memDir, err)
		return
	}

	memories, err := db.GetMemoriesForEntity(database, entityID)
	if err != nil {
		log.Printf("[delegate] warning: failed to load prior memories for entity %s: %v", entityID, err)
		return
	}
	if len(memories) == 0 {
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

// lookupEntityProjectID returns the entity's project_id (or nil if the
// entity is unassigned, missing, or the lookup fails). Failure is
// logged and treated as "not assigned" — the spawner degrades gracefully
// rather than blocking the run on a non-essential context lookup.
func lookupEntityProjectID(database *sql.DB, entityID string) *string {
	entity, err := db.GetEntity(database, entityID)
	if err != nil {
		log.Printf("[delegate] warning: failed to load entity %s for project lookup: %v", entityID, err)
		return nil
	}
	if entity == nil {
		return nil
	}
	return entity.ProjectID
}

// projectKnowledgeWarnBytes is the soft cap on per-project knowledge-base
// total size. We log when crossed but still copy — curated KB content is
// the user's intent, and silently dropping it would be more surprising
// than a noisy log line.
const projectKnowledgeWarnBytes = 500 * 1024

// streamCopyFile copies src to dst via io.Copy so large knowledge-base
// files don't get buffered fully in the spawner's heap. Returns bytes
// written. Uses 0644 to mirror the previous os.WriteFile behavior.
func streamCopyFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return n, copyErr
	}
	return n, closeErr
}

// materializeProjectKnowledge stages the entity's project knowledge-base
// into <cwd>/_scratch/project-knowledge/ so the agent can read it as
// ambient context. Mirrors materializePriorMemories' "create the dir
// unconditionally" pattern so the agent's pre-flight `ls` doesn't fail
// noisily on ENOENT when no project is assigned.
//
// Reads from ~/.triagefactory/projects/<projectID>/knowledge-base/*.md
// (the path the Curator writes to per SKY-216) and copies each .md file
// flat into _scratch/project-knowledge/, preserving source filenames.
//
// Degrades gracefully: a nil projectID, a missing knowledge-base dir,
// or per-file copy failures are logged but never fail the run.
func materializeProjectKnowledge(cwd string, projectID *string) {
	dir := filepath.Join(cwd, "_scratch", "project-knowledge")
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("[delegate] warning: failed to create project-knowledge dir at %s: %v", dir, err)
		return
	}

	if projectID == nil || *projectID == "" {
		return
	}

	kbRoot, err := curator.KnowledgeDir(*projectID)
	if err != nil {
		log.Printf("[delegate] warning: resolve knowledge dir for project %s: %v", *projectID, err)
		return
	}
	srcDir := filepath.Join(kbRoot, "knowledge-base")

	entries, err := os.ReadDir(srcDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[delegate] warning: read project knowledge-base %s: %v", srcDir, err)
		}
		return
	}

	written := 0
	totalBytes := int64(0)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		src := filepath.Join(srcDir, e.Name())
		dst := filepath.Join(dir, e.Name())
		n, err := streamCopyFile(src, dst)
		if err != nil {
			log.Printf("[delegate] warning: copy project knowledge file %s -> %s: %v", src, dst, err)
			continue
		}
		written++
		totalBytes += n
	}

	if totalBytes > projectKnowledgeWarnBytes {
		log.Printf("[delegate] project %s knowledge-base is %d bytes — over the %d soft cap; consider trimming", *projectID, totalBytes, projectKnowledgeWarnBytes)
	}
	if written > 0 {
		log.Printf("[delegate] materialized %d project-knowledge files for project %s", written, *projectID)
	}
}

// resolvePrompt finds the prompt for a task from an explicit prompt ID.
// Manual delegation always requires the caller to pick a prompt; auto-delegation
// supplies the prompt_id from the trigger row.
func (s *Spawner) resolvePrompt(task domain.Task, explicitPromptID string) (*domain.Prompt, error) {
	if explicitPromptID == "" {
		return nil, fmt.Errorf("%w — select one from the prompt picker", ErrPromptUnspecified)
	}

	p, err := db.GetPrompt(s.database, explicitPromptID)
	if err != nil {
		return nil, fmt.Errorf("failed to load prompt %s: %w", explicitPromptID, err)
	}
	if p == nil {
		return nil, fmt.Errorf("%w: %s", ErrPromptNotFound, explicitPromptID)
	}
	return p, nil
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

	if _, err := db.InsertAgentMessage(s.database, &domain.AgentMessage{
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

	// Yield is populated when Status == "yield". The agent is asking
	// the user a question and the run should park in awaiting_input
	// rather than completing. See domain.YieldRequest and SKY-139 /
	// internal/ai/prompts/envelope.txt for the agent-facing contract.
	Yield *domain.YieldRequest `json:"yield,omitempty"`
}

// isValid reports whether the parsed envelope contains enough to act on.
// Two terminal shapes are accepted:
//   - completion / task_unsolvable: Summary is non-empty (the legacy
//     contract — every successful or unsolvable envelope has a summary)
//   - yield: Status == "yield" and the yield payload passes
//     YieldRequest.Validate (known type, non-empty message, well-formed
//     options for choice yields, no duplicate option ids)
//
// Anything else is treated as "didn't parse cleanly" — the parser
// falls through to its markdown-fence and brace-extraction paths
// before giving up. Rejecting malformed yield payloads at parse time
// matters because once a yield parks the run in awaiting_input, the
// user can't respond unless the modal can render meaningfully —
// e.g. a choice yield with no options has no buttons to click.
func (r *agentResult) isValid() bool {
	if r.Summary != "" {
		return true
	}
	if r.Status == "yield" && r.Yield != nil {
		return r.Yield.Validate() == nil
	}
	return false
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
// Recognizes both completion envelopes (status: completed | task_unsolvable
// with a non-empty summary) and yield envelopes (status: yield with a typed
// yield payload — SKY-139). See agentResult.isValid for the acceptance rule.
func parseAgentResult(text string) *agentResult {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	var result agentResult
	if json.Unmarshal([]byte(text), &result) == nil && result.isValid() {
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
		if json.Unmarshal([]byte(stripped), &result) == nil && result.isValid() {
			return &result
		}
	}

	if start := strings.Index(text, "{"); start >= 0 {
		if end := strings.LastIndex(text, "}"); end > start {
			candidate := text[start : end+1]
			if json.Unmarshal([]byte(candidate), &result) == nil && result.isValid() {
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

func (s *Spawner) cachedAgentTools() string {
	s.agentToolsOnce.Do(func() {
		s.agentToolsCache = skills.ScanAgentTools()
	})
	return s.agentToolsCache
}

// collectExtraTools merges a prompt's declared allowed_tools with tools
// discovered from agent definitions (~/.claude/agents/*.md).
func (s *Spawner) collectExtraTools(promptAllowedTools string) string {
	agentTools := s.cachedAgentTools()
	if promptAllowedTools == "" && agentTools == "" {
		return ""
	}
	return skills.NormalizeToolList(promptAllowedTools + "," + agentTools)
}

func parseOwnerRepo(s string) (string, string) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}
