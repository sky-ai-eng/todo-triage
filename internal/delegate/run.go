// Generic agent execution loop and the post-stream branching that turns
// either a yield (park in awaiting_input) or a terminal completion (run
// the memory gate, finalize the run row) into the right DB state. Shared
// between the initial Delegate path and the SKY-139 yield-resume flow.

package delegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

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
			if cfg.isChainStep {
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
			if cfg.isChainStep {
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
		if cfg.isChainStep {
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
		SystemPrompt: cfg.appendSysPrompt,
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

	s.failRun(runID, task.ID, triggerType, "agent runtime exited cleanly without producing a result event")
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
		//
		// Non-final chain steps must not submit reviews/PRs (the wrapper
		// prompt forbids it) UNLESS they recorded a --final verdict, which
		// is the explicit early-exit channel: the step is allowed one
		// terminal external action (e.g., a SKIP review) and the action
		// still flows through this same human-approval gate.
		//
		// If a non-final step creates a pending artifact without recording
		// --final, the agent mislabelled its verdict: the pending artifact
		// IS the chain's terminal external action. Auto-promote to --final
		// (writing a synthetic verdict that supersedes the agent's) so the
		// chain terminates at this step on human approval instead of
		// advancing past stale handoff narrative into a no-op step.
		hasPending := false
		if pendingReview, _ := db.PendingReviewByRunID(s.database, runID); pendingReview != nil {
			hasPending = true
		} else if pendingPR, _ := db.PendingPRByRunID(s.database, runID); pendingPR != nil {
			hasPending = true
		}
		if hasPending {
			if s.isNonFinalChainStep(runID) {
				verdict, _ := s.chains.GetLatestVerdict(ctx, runmode.LocalDefaultOrg, runID)
				if verdict == nil || verdict.Outcome != domain.ChainVerdictFinal {
					synthetic := domain.ChainVerdict{
						Outcome:   domain.ChainVerdictFinal,
						Reason:    "auto-promoted: non-final step submitted external action without --final",
						Synthetic: true,
					}
					if payload, err := json.Marshal(synthetic); err == nil {
						if insertErr := s.chains.InsertVerdict(ctx, runmode.LocalDefaultOrg, runID, string(payload)); insertErr != nil {
							log.Printf("[delegate] warning: insert synthetic --final verdict for run %s: %v", runID, insertErr)
						} else {
							log.Printf("[delegate] run %s: non-final chain step submitted pending artifact; auto-promoted verdict to --final", runID)
						}
					}
				}
			}
			status = "pending_approval"
			if _, err := s.database.Exec(`UPDATE runs SET status = ? WHERE id = ?`, status, runID); err != nil {
				log.Printf("[delegate] warning: failed to set pending_approval for run %s: %v", runID, err)
			}
		}
	}

	if status == "completed" {
		// Two guards prevent a stale completion from flipping the task:
		//
		// 1. Chain step: the chain orchestrator owns task lifecycle —
		//    individual step completions must not close the task.
		//
		// 2. Re-delegation race: a newer run already exists on this task
		//    (the user re-delegated while this run was in flight). The
		//    newer run's CreateAgentRun row is already in the DB by the
		//    time the old run reaches processCompletion, so the EXISTS
		//    check is deterministic. Chain step runs are excluded from
		//    the active-run check because the chain orchestrator creates
		//    them sequentially (step N+1's row doesn't exist yet when
		//    step N completes).
		var chainRunID sql.NullString
		_ = s.database.QueryRow(`SELECT chain_run_id FROM runs WHERE id = ?`, runID).Scan(&chainRunID)
		if chainRunID.Valid {
			// Chain step — skip; terminateChain handles task closure.
		} else {
			var hasOtherActiveRun bool
			_ = s.database.QueryRow(`
				SELECT EXISTS(
					SELECT 1 FROM runs
					WHERE task_id = ? AND id != ?
					AND status NOT IN ('completed','failed','cancelled','task_unsolvable','taken_over','pending_approval')
				)
			`, task.ID, runID).Scan(&hasOtherActiveRun)
			if !hasOtherActiveRun {
				if _, err := s.database.Exec(`UPDATE tasks SET status = 'done' WHERE id = ?`, task.ID); err != nil {
					log.Printf("[delegate] warning: failed to update task %s to done: %v", task.ID, err)
				}
			}
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
