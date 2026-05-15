package delegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "embed" // powers chainStepSystemPrompt

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

//go:embed prompts/chain-step-system.txt
var chainStepSystemPrompt string

// delegateChain is the chain analog of Delegate's single-prompt body.
// It loads the step list, sets up the shared worktree, creates the
// chain_runs row, and spawns the orchestrator goroutine. The returned
// id is the chain_run id (not a step run id) — the UI / API surfaces
// this as "the chain that was kicked off".
//
// Failures inside this function (empty step list, worktree setup
// failure, db write errors) terminate the chain immediately with a
// matching abort_reason rather than returning an error to the caller —
// the caller already has the chain_run id and the UI subscribes to
// the chain row by id, so a synchronous error wouldn't be reflected
// anywhere visible.
func (s *Spawner) delegateChain(task domain.Task, chainPrompt *domain.Prompt, triggerType, triggerID string, gh *ghclient.Client, model string) (string, error) {
	steps, err := s.chains.ListSteps(context.Background(), runmode.LocalDefaultOrg, chainPrompt.ID)
	if err != nil {
		return "", fmt.Errorf("load chain steps: %w", err)
	}
	if len(steps) == 0 {
		return "", fmt.Errorf("chain prompt %q has no steps", chainPrompt.Name)
	}

	// Allocate the chain id up front so the goroutine and the caller
	// both reference the same row — we want callers to be able to
	// subscribe to chain_runs/{id} immediately, not wait for a setup
	// round-trip.
	chainRunID := uuid.New().String()

	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancels[chainRunID] = cancel
	s.mu.Unlock()
	// Mark this id as a chain_run so the setup phase's status helpers
	// don't broadcast agent_run_update events for a non-existent runs row.
	s.markChainRunID(chainRunID)

	go func() {
		startTime := time.Now()
		defer func() {
			s.mu.Lock()
			delete(s.cancels, chainRunID)
			s.mu.Unlock()
			cancel()
			s.unmarkChainRunID(chainRunID)
		}()

		// Build the shared worktree exactly once. The same setupGitHub /
		// setupJira used by single runs — chain steps reuse the result.
		var cfg runConfig
		var setupErr error
		switch task.EntitySource {
		case "github":
			cfg, setupErr = s.setupGitHub(ctx, chainRunID, task, gh)
		case "jira":
			cfg, setupErr = s.setupJira(ctx, chainRunID, task, gh)
		default:
			setupErr = fmt.Errorf("unsupported task source: %s", task.EntitySource)
		}
		if setupErr != nil {
			// Persist a chain_runs row anyway so the UI has something to
			// show; write abort_reason and completed_at directly in the
			// insert — MarkRunStatus won't match a row that isn't 'running'.
			now := time.Now().UTC()
			_, _ = s.chains.CreateRun(ctx, runmode.LocalDefaultOrg, domain.ChainRun{
				ID:            chainRunID,
				ChainPromptID: chainPrompt.ID,
				TaskID:        task.ID,
				TriggerType:   domain.ChainTriggerType(triggerType),
				TriggerID:     triggerID,
				Status:        domain.ChainRunStatusFailed,
				AbortReason:   setupErr.Error(),
				CompletedAt:   &now,
				WorktreePath:  "",
			})
			if cfgEntity := taskEntityID(s.tasks, task.ID); cfgEntity != "" {
				s.notifyDrainer(triggerType, cfgEntity)
			}
			return
		}

		if _, err := s.chains.CreateRun(ctx, runmode.LocalDefaultOrg, domain.ChainRun{
			ID:            chainRunID,
			ChainPromptID: chainPrompt.ID,
			TaskID:        task.ID,
			TriggerType:   domain.ChainTriggerType(triggerType),
			TriggerID:     triggerID,
			Status:        domain.ChainRunStatusRunning,
			WorktreePath:  cfg.wtPath,
		}); err != nil {
			log.Printf("[chain] failed to persist chain_run %s: %v", chainRunID, err)
			s.runChainWorktreeCleanup(chainRunID, cfg)
			if cfgEntity := taskEntityID(s.tasks, task.ID); cfgEntity != "" {
				s.notifyDrainer(triggerType, cfgEntity)
			}
			return
		}

		verb := "Chain started"
		if triggerType == "event" {
			verb = "Auto-fired chain"
		}
		toast.Info(s.wsHub, fmt.Sprintf("%s: %s (%s)",
			verb, truncateToastMsg(chainPrompt.Name, 60), shortRunID(chainRunID)))

		s.runChain(ctx, chainRunID, task, chainPrompt, steps, cfg, startTime, model, triggerType)
	}()

	return chainRunID, nil
}

// runChain orchestrates a chain prompt against one task. It owns the
// shared worktree (built once via setupGitHub / setupJira) and walks
// the ordered step list, creating one runs row per step. After each
// step terminates, it reads the latest chain:verdict artifact and
// decides whether to advance, abort, or fail.
//
// Yield mid-chain and pending_approval mid-chain are handled
// separately via ResumeChainAfterYield / ResumeChainAfterApproval —
// the orchestrator returns early when the step lands in awaiting_input
// or pending_approval, leaving chain_runs.status='running' and the
// shared worktree on disk for the eventual resume.
func (s *Spawner) runChain(
	ctx context.Context,
	chainRunID string,
	task domain.Task,
	chainPrompt *domain.Prompt,
	steps []domain.ChainStep,
	cfg runConfig,
	startTime time.Time,
	model string,
	triggerType string,
) {
	if len(steps) == 0 {
		s.terminateChain(chainRunID, task.ID, triggerType, startTime, cfg, domain.ChainRunStatusFailed,
			"chain has no steps", nil, false)
		return
	}

	for i, step := range steps {
		if ctx.Err() != nil {
			s.terminateChain(chainRunID, task.ID, triggerType, startTime, cfg, domain.ChainRunStatusCancelled,
				"cancelled", &step.StepIndex, false)
			return
		}

		stepPrompt, err := s.prompts.Get(ctx, runmode.LocalDefaultOrg, step.StepPromptID)
		if err != nil || stepPrompt == nil {
			s.terminateChain(chainRunID, task.ID, triggerType, startTime, cfg, domain.ChainRunStatusFailed,
				fmt.Sprintf("step %d prompt fetch failed", i), &step.StepIndex, false)
			return
		}
		if stepPrompt.Kind == domain.PromptKindChain {
			s.terminateChain(chainRunID, task.ID, triggerType, startTime, cfg, domain.ChainRunStatusAborted,
				"nested_chain_step", &step.StepIndex, false)
			return
		}

		// Wipe any prior step's materialized skill so step N+1 only
		// sees its own SKILL.md.
		if err := skills.WipeChainSkills(cfg.wtPath); err != nil {
			log.Printf("[chain] run %s step %d: wipe skills: %v", chainRunID, i, err)
		}
		slug := skills.SlugForChainStep(i, stepPrompt.Name)
		if err := skills.MaterializeStepSkill(cfg.wtPath, slug, stepPrompt, step.Brief); err != nil {
			s.terminateChain(chainRunID, task.ID, triggerType, startTime, cfg, domain.ChainRunStatusFailed,
				fmt.Sprintf("materialize step %d skill: %s", i, err.Error()), &step.StepIndex, false)
			return
		}

		// Create the per-step run row. prompt_id points at the leaf
		// step prompt (so per-step stats stay accurate); chain_run_id
		// + chain_step_index thread it back to the chain instance.
		stepRunID := uuid.New().String()
		stepIdxCopy := i
		if err := s.agentRuns.Create(context.Background(), runmode.LocalDefaultOrg, domain.AgentRun{
			ID:             stepRunID,
			TaskID:         task.ID,
			PromptID:       stepPrompt.ID,
			Status:         "initializing",
			Model:          model,
			TriggerType:    triggerType,
			ChainRunID:     chainRunID,
			ChainStepIndex: &stepIdxCopy,
			WorktreePath:   cfg.wtPath,
		}); err != nil {
			s.terminateChain(chainRunID, task.ID, triggerType, startTime, cfg, domain.ChainRunStatusFailed,
				fmt.Sprintf("create step %d run: %s", i, err.Error()), &step.StepIndex, false)
			return
		}
		s.broadcastRunUpdate(stepRunID, "initializing")
		if err := s.prompts.IncrementUsage(ctx, runmode.LocalDefaultOrg, stepPrompt.ID); err != nil {
			log.Printf("[chain] warning: failed to increment usage for step prompt %s: %v", stepPrompt.ID, err)
		}

		// Per-step cancel handle so Cancel(stepRunID) cancels just the
		// active step. The chain ctx itself stays alive across steps.
		stepCtx, stepCancel := context.WithCancel(ctx)
		s.mu.Lock()
		s.cancels[stepRunID] = stepCancel
		s.mu.Unlock()

		stepCfg := cfg
		stepCfg.isChainStep = true
		stepCfg.chainRunID = chainRunID
		stepCfg.chainStep = i
		stepCfg.appendSysPrompt = chainStepSystemPrompt
		stepCfg.extraAllowedTools = s.collectExtraTools(stepPrompt.AllowedTools)

		var nextStepName string
		if i+1 < len(steps) {
			if np, err := s.prompts.Get(ctx, runmode.LocalDefaultOrg, steps[i+1].StepPromptID); err == nil && np != nil {
				nextStepName = np.Name
			}
		}
		mission := buildChainStepWrapperPrompt(task, step, stepPrompt, slug, len(steps), nextStepName)

		toast.Info(s.wsHub, fmt.Sprintf("Chain step %d/%d: %s (%s)",
			i+1, len(steps), truncateToastMsg(stepPrompt.Name, 60), shortRunID(stepRunID)))

		s.runAgent(stepCtx, stepRunID, task, mission, stepCfg, time.Now(), model, triggerType)

		// Clear the cancel handle now that the step has returned.
		s.mu.Lock()
		delete(s.cancels, stepRunID)
		s.mu.Unlock()
		stepCancel()

		// Re-read the run row to learn its terminal status. runAgent's
		// return is unconditional — completion / failure / cancellation
		// / pending_approval / yield all come back through here.
		stepRun, err := s.agentRuns.Get(context.Background(), runmode.LocalDefaultOrg, stepRunID)
		if err != nil || stepRun == nil {
			s.terminateChain(chainRunID, task.ID, triggerType, startTime, cfg, domain.ChainRunStatusFailed,
				fmt.Sprintf("read step %d run after agent: %v", i, err), &step.StepIndex, false)
			return
		}

		// Yield / pending_approval mid-chain: leave the chain in
		// 'running' and the worktree on disk. The corresponding resume
		// hook (ResumeChainAfterYield / ResumeChainAfterApproval) will
		// pick up where we left off.
		if stepRun.Status == "awaiting_input" || stepRun.Status == "pending_approval" {
			log.Printf("[chain] run %s step %d paused at status=%s; chain remains running", chainRunID, i, stepRun.Status)
			return
		}

		if stepRun.Status == "cancelled" {
			s.terminateChain(chainRunID, task.ID, triggerType, startTime, cfg, domain.ChainRunStatusCancelled,
				"step cancelled", &step.StepIndex, false)
			return
		}
		if stepRun.Status == "failed" || stepRun.Status == "task_unsolvable" {
			s.terminateChain(chainRunID, task.ID, triggerType, startTime, cfg, domain.ChainRunStatusFailed,
				"step "+stepRun.Status, &step.StepIndex, false)
			return
		}
		if stepRun.Status != "completed" {
			// Defensive: any unexpected non-terminal status (taken_over
			// is the most likely candidate) ends the chain in failed
			// state. taken_over runs are owned by the user from here on,
			// so the chain can't sensibly continue.
			s.terminateChain(chainRunID, task.ID, triggerType, startTime, cfg, domain.ChainRunStatusFailed,
				"step ended with status "+stepRun.Status, &step.StepIndex, false)
			return
		}

		verdict, err := s.chains.GetLatestVerdict(ctx, runmode.LocalDefaultOrg, stepRunID)
		if err != nil {
			log.Printf("[chain] run %s step %d: read verdict: %v", chainRunID, i, err)
		}
		if verdict == nil {
			// Synthetic abort — record so the UI shows the same shape
			// as a real verdict, then halt.
			synthetic := domain.ChainVerdict{
				Outcome:   domain.ChainVerdictAbort,
				Reason:    "no-verdict",
				Synthetic: true,
			}
			if payload, err := json.Marshal(synthetic); err == nil {
				if insertErr := s.chains.InsertVerdict(ctx, runmode.LocalDefaultOrg, stepRunID, string(payload)); insertErr != nil {
					log.Printf("[chain] run %s step %d: insert synthetic verdict artifact: %v", chainRunID, i, insertErr)
				}
			}
			s.terminateChain(chainRunID, task.ID, triggerType, startTime, cfg, domain.ChainRunStatusAborted,
				"no-verdict", &step.StepIndex, false)
			return
		}
		switch verdict.Outcome {
		case domain.ChainVerdictFinal:
			// Step decided the chain's intended outcome is reached here.
			// Terminate as completed (closes the task) and record the
			// step index so the UI can show "exited early at step N".
			reason := verdict.Reason
			if reason == "" {
				reason = "step recorded --final"
			}
			s.terminateChain(chainRunID, task.ID, triggerType, startTime, cfg, domain.ChainRunStatusCompleted,
				reason, &step.StepIndex, false)
			return
		case domain.ChainVerdictAbort:
			reason := verdict.Reason
			if reason == "" {
				reason = "step recorded --abort"
			}
			s.terminateChain(chainRunID, task.ID, triggerType, startTime, cfg, domain.ChainRunStatusAborted,
				reason, &step.StepIndex, false)
			return
		case domain.ChainVerdictAdvance:
		default:
			// Unknown outcome — treat as abort.
			s.terminateChain(chainRunID, task.ID, triggerType, startTime, cfg, domain.ChainRunStatusAborted,
				"unknown verdict outcome: "+string(verdict.Outcome), &step.StepIndex, false)
			return
		}
	}

	s.terminateChain(chainRunID, task.ID, triggerType, startTime, cfg, domain.ChainRunStatusCompleted,
		"", nil, false)
}

// terminateChain finalizes the chain run row and runs the shared
// worktree cleanup that runAgent's per-step defers skipped. taskDone
// distinguishes "all steps green, mark task done like a single run
// would" (status=completed) from "stopped early — leave the task open
// for human review" (any other terminal). skipCleanup short-circuits
// when the worktree itself is already gone (worktree_lost path).
func (s *Spawner) terminateChain(
	chainRunID, taskID, triggerType string,
	startTime time.Time,
	cfg runConfig,
	status domain.ChainRunStatus,
	abortReason string,
	abortedAtStep *int,
	skipCleanup bool,
) {
	if _, err := s.chains.MarkRunStatus(context.Background(), runmode.LocalDefaultOrg, chainRunID, status, abortReason, abortedAtStep); err != nil {
		log.Printf("[chain] FATAL: mark chain_run %s status=%s: %v — skipping cleanup to keep chain row consistent", chainRunID, status, err)
		return
	}

	if status == domain.ChainRunStatusCompleted {
		// Mirror single-run behavior: a clean chain finalization closes the task.
		if err := s.tasks.Close(context.Background(), runmode.LocalDefaultOrg, taskID, "run_completed", ""); err != nil {
			log.Printf("[chain] close task %s: %v", taskID, err)
		}
	}
	// Aborted / failed / cancelled chains intentionally do NOT mark
	// the task done — leave it in the queue so a human can inspect
	// _scratch/handoff.md and decide what to do next.

	if !skipCleanup {
		s.runChainWorktreeCleanup(chainRunID, cfg)
	}

	// Drain the per-entity queue exactly once for the chain (independent
	// of how many steps ran).
	if cfgEntity := taskEntityID(s.tasks, taskID); cfgEntity != "" {
		s.notifyDrainer(triggerType, cfgEntity)
	}

	dur := time.Since(startTime)
	log.Printf("[chain] chain_run %s terminated status=%s reason=%q duration=%s",
		chainRunID, status, abortReason, dur)
}

// runChainWorktreeCleanup performs the cleanup runAgent would have done
// per-step, except now once for the whole chain.
func (s *Spawner) runChainWorktreeCleanup(chainRunID string, cfg runConfig) {
	if cfg.hasWT {
		if err := worktree.RemoveAt(cfg.wtPath, chainRunID); err != nil {
			log.Printf("[chain] worktree remove failed for chain %s: %v", chainRunID, err)
			return
		}
		if cfg.prNumber > 0 && cfg.owner != "" && cfg.repo != "" {
			worktree.CleanupPRConfig(cfg.owner, cfg.repo, cfg.headRef, cfg.prNumber)
		}
	} else if cfg.runRoot != "" {
		// Jira chains materialize worktrees lazily via `workspace add`,
		// which keys run_worktrees rows by each *step's* run_id (the
		// agent's TRIAGE_FACTORY_RUN_ID), not by the chain_run_id.
		// Iterate every step run in the chain so we actually find and
		// remove their reservations.
		stepRuns, err := s.chains.RunsForChain(context.Background(), runmode.LocalDefaultOrg, chainRunID)
		if err != nil {
			log.Printf("[chain] run %s: list step runs for cleanup: %v", chainRunID, err)
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, sr := range stepRuns {
			rows, err := db.GetRunWorktrees(s.database, sr.ID)
			if err != nil {
				log.Printf("[chain] run %s: list run_worktrees for step %s: %v", chainRunID, sr.ID, err)
				// Log but continue to attempt DB row deletion below.
				rows = nil
			}
			for _, w := range rows {
				if err := worktree.RemoveAt(w.Path, sr.ID); err != nil && !errors.Is(err, os.ErrNotExist) {
					log.Printf("[chain] run %s: remove worktree %s: %v", chainRunID, w.Path, err)
					// Still attempt the DB row deletion even if the worktree remove failed.
				}
				if _, err := s.database.ExecContext(cleanupCtx,
					"DELETE FROM run_worktrees WHERE run_id = ? AND path = ?", sr.ID, w.Path); err != nil {
					log.Printf("[chain] run %s: delete run_worktrees row for %s: %v", chainRunID, w.Path, err)
				}
			}
		}
		worktree.RemoveRunRoot(chainRunID)
	}
	worktree.RemoveClaudeProjectDir(cfg.wtPath)
}

// taskEntityID resolves the entity_id for a task. Used to drain the
// per-entity firing queue at chain terminal.
func taskEntityID(tasks db.TaskStore, taskID string) string {
	t, err := tasks.Get(context.Background(), runmode.LocalDefaultOrg, taskID)
	if err != nil {
		log.Printf("[chain] taskEntityID: failed to resolve entity for task %s: %v", taskID, err)
		return ""
	}
	if t == nil {
		return ""
	}
	return t.EntityID
}

// buildChainStepWrapperPrompt produces the per-step user prompt carrying
// step-specific data. The system prompt (chain-step-system.txt) owns the
// protocol contract; this wrapper supplies only the step's context.
func buildChainStepWrapperPrompt(task domain.Task, step domain.ChainStep, stepPrompt *domain.Prompt, slug string, total int, nextStepName string) string {
	mission := strings.TrimSpace(step.Brief)
	if mission == "" {
		mission = stepPrompt.Name
	}
	binaryPath, _ := os.Executable()
	if binaryPath == "" {
		binaryPath = "triagefactory"
	}
	binaryPath = filepath.Clean(binaryPath)

	var b strings.Builder
	fmt.Fprintf(&b, "You are step %d of %d in a chain firing on this task.\n\n", step.StepIndex+1, total)
	fmt.Fprintf(&b, "Task: %s\n", strings.TrimSpace(task.Title))
	fmt.Fprintf(&b, "Mission for this step: %s\n\n", mission)
	fmt.Fprintf(&b, "Skill slug: %q (materialized at ./.claude/skills/%s/SKILL.md)\n", slug, slug)
	isFinal := step.StepIndex+1 == total
	fmt.Fprintf(&b, "Is final step: %v\n", isFinal)
	if !isFinal {
		nextLabel := nextStepName
		if nextLabel == "" {
			nextLabel = fmt.Sprintf("step %d", step.StepIndex+2)
		}
		fmt.Fprintf(&b, "Next step: %q\n", nextLabel)
	}
	fmt.Fprintf(&b, "Binary path for verdict commands: %s\n", binaryPath)
	b.WriteString("Handoff notes from prior steps: ./_scratch/handoff.md\n")
	return b.String()
}

// CancelChain cancels every step inside a chain run, marks the chain
// row cancelled, and lets the active step's runAgent return naturally.
// Safe to call when the chain is already terminal.
func (s *Spawner) CancelChain(chainRunID string) error {
	cr, err := s.chains.GetRun(context.Background(), runmode.LocalDefaultOrg, chainRunID)
	if err != nil {
		return fmt.Errorf("load chain run: %w", err)
	}
	if cr == nil {
		return fmt.Errorf("chain run %s not found", chainRunID)
	}
	if cr.Status != domain.ChainRunStatusRunning {
		return nil
	}

	// Cancel any cancel handles registered by the orchestrator for this
	// chain. The orchestrator stores per-step cancels under the step
	// run_id; we sweep all active step runs and cancel them. We also
	// cancel the chain's own ctx so the setup phase and inter-step
	// checks see the cancellation.
	//
	// anyActive tracks whether at least one orchestrator-owned context
	// got canceled. If nothing was active — the chain is paused on a
	// pending_approval or awaiting_input step — the orchestrator
	// goroutine has already exited, so no later path will run cleanup.
	// In that case we drive terminateChain ourselves below.
	var anyActive bool
	rows, err := s.database.Query(`SELECT id FROM runs WHERE chain_run_id = ? AND status NOT IN
		('completed','failed','cancelled','task_unsolvable','pending_approval','taken_over','awaiting_input')`,
		chainRunID)
	if err == nil {
		defer rows.Close()
		s.mu.Lock()
		for rows.Next() {
			var runID string
			if err := rows.Scan(&runID); err == nil {
				if cancel, ok := s.cancels[runID]; ok {
					cancel()
					anyActive = true
				}
			}
		}
		// Also cancel the chain-level context registered at delegateChain.
		if chainCancel, ok := s.cancels[chainRunID]; ok {
			chainCancel()
			anyActive = true
		}
		s.mu.Unlock()
	}

	if anyActive {
		// Orchestrator goroutine is still alive — it will observe the
		// cancellation, the step's runAgent will return, and the loop
		// will call terminateChain (which marks the chain cancelled and
		// runs cleanup). Avoid double-marking here so terminateChain's
		// MarkRunStatus succeeds.
		return nil
	}

	// Paused chain: rebuild just enough cfg for terminateChain's worktree
	// cleanup (mirrors ResumeChainAfterApproval — owner/repo/prNumber
	// aren't persisted on chain_runs, so CleanupPRConfig is skipped).
	task, err := s.tasks.Get(context.Background(), runmode.LocalDefaultOrg, cr.TaskID)
	if err != nil || task == nil {
		log.Printf("[chain] CancelChain: load task for paused chain_run %s: %v", chainRunID, err)
		_, markErr := s.chains.MarkRunStatus(context.Background(), runmode.LocalDefaultOrg, chainRunID, domain.ChainRunStatusCancelled, "user_cancelled", nil)
		return markErr
	}
	cfg := runConfig{wtPath: cr.WorktreePath}
	if task.EntitySource == "github" {
		cfg.hasWT = true
	}
	s.terminateChain(cr.ID, cr.TaskID, string(cr.TriggerType), cr.StartedAt, cfg,
		domain.ChainRunStatusCancelled, "user_cancelled", nil, false)
	return nil
}

// ResumeChainAfterYield re-enters the orchestrator loop for the
// remaining steps after a yield-resume completes successfully.
// Currently not fully implemented: marks the chain aborted so it
// doesn't silently stall in 'running'.
func (s *Spawner) ResumeChainAfterYield(stepRunID string) {
	cr, stepIdx, err := s.chains.GetRunForRun(context.Background(), runmode.LocalDefaultOrg, stepRunID)
	if err != nil || cr == nil {
		return
	}
	if cr.Status != domain.ChainRunStatusRunning {
		return
	}
	log.Printf("[chain] yield-resume not yet implemented for chain_run %s step run %s; aborting chain", cr.ID, stepRunID)
	task, err := s.tasks.Get(context.Background(), runmode.LocalDefaultOrg, cr.TaskID)
	if err != nil || task == nil {
		log.Printf("[chain] yield-resume: load task for chain_run %s: %v", cr.ID, err)
		// Fall back to a bare MarkChainRunStatus without full cleanup.
		_, _ = s.chains.MarkRunStatus(context.Background(), runmode.LocalDefaultOrg, cr.ID, domain.ChainRunStatusAborted, "yield_resume_not_implemented", stepIdx)
		return
	}
	cfg := runConfig{wtPath: cr.WorktreePath}
	if task.EntitySource == "github" {
		cfg.hasWT = true
	}
	s.terminateChain(cr.ID, cr.TaskID, string(cr.TriggerType), cr.StartedAt, cfg,
		domain.ChainRunStatusAborted, "yield_resume_not_implemented", stepIdx, false)
}

// ResumeChainAfterApproval is invoked by the reviews / pending-PR
// approval handlers after they flip a step run from pending_approval
// back to completed. It only handles the --final verdict case (the only
// shape under which a chain step is allowed to land in pending_approval
// — see the guard in spawner.processCompletion): terminate the chain
// as completed, close the task, and clean the shared worktree.
//
// If the verdict is missing or is not Final, the chain stays in
// 'running' on the assumption that something raced or the agent
// recorded the wrong verdict; a human can inspect chain_runs and
// resolve manually rather than have us guess.
func (s *Spawner) ResumeChainAfterApproval(stepRunID string) {
	cr, stepIdx, err := s.chains.GetRunForRun(context.Background(), runmode.LocalDefaultOrg, stepRunID)
	if err != nil || cr == nil {
		return
	}
	if cr.Status != domain.ChainRunStatusRunning {
		return
	}

	verdict, err := s.chains.GetLatestVerdict(context.Background(), runmode.LocalDefaultOrg, stepRunID)
	if err != nil {
		log.Printf("[chain] approval-resume run %s: read verdict: %v", stepRunID, err)
		return
	}
	if verdict == nil || verdict.Outcome != domain.ChainVerdictFinal {
		log.Printf("[chain] approval-resume chain_run %s step run %s: verdict not --final (%+v); chain left running", cr.ID, stepRunID, verdict)
		return
	}

	task, err := s.tasks.Get(context.Background(), runmode.LocalDefaultOrg, cr.TaskID)
	if err != nil || task == nil {
		log.Printf("[chain] approval-resume chain_run %s: load task: %v", cr.ID, err)
		return
	}

	// Reconstruct just enough runConfig for terminateChain's worktree
	// cleanup. The original orchestrator goroutine (which held the full
	// cfg) returned when the step landed in pending_approval, so we
	// rebuild from durable state. owner/repo/prNumber/headRef are not
	// stored on chain_runs; CleanupPRConfig is best-effort and skipped
	// here — leaves a few stale git config entries but no user-visible
	// effect.
	cfg := runConfig{wtPath: cr.WorktreePath}
	if task.EntitySource == "github" {
		cfg.hasWT = true
	}

	reason := verdict.Reason
	if reason == "" {
		reason = "step recorded --final"
	}
	s.terminateChain(cr.ID, cr.TaskID, string(cr.TriggerType), cr.StartedAt, cfg,
		domain.ChainRunStatusCompleted, reason, stepIdx, false)
}

// isNonFinalChainStep returns true when the run is a chain step that
// is not the last step in the chain. Used as a guard in
// processCompletion to prevent mid-chain approval stalls.
//
// Returns true (safe default) on DB error: treating an unknown step as
// non-final ensures the pending-approval guard still engages even when
// the DB is flaky, preventing unintended mid-chain external actions.
func (s *Spawner) isNonFinalChainStep(runID string) bool {
	var chainRunID sql.NullString
	var stepIndex sql.NullInt64
	if err := s.database.QueryRow(
		`SELECT chain_run_id, chain_step_index FROM runs WHERE id = ?`, runID,
	).Scan(&chainRunID, &stepIndex); err != nil || !chainRunID.Valid || !stepIndex.Valid {
		if err != nil && err != sql.ErrNoRows {
			log.Printf("[chain] isNonFinalChainStep: query run %s: %v", runID, err)
			return true
		}
		return false
	}
	var chainPromptID string
	if err := s.database.QueryRow(
		`SELECT chain_prompt_id FROM chain_runs WHERE id = ?`, chainRunID.String,
	).Scan(&chainPromptID); err != nil {
		log.Printf("[chain] isNonFinalChainStep: query chain_run %s for run %s: %v", chainRunID.String, runID, err)
		return true
	}
	steps, err := s.chains.ListSteps(context.Background(), runmode.LocalDefaultOrg, chainPromptID)
	if err != nil {
		log.Printf("[chain] isNonFinalChainStep: list steps for chain %s run %s: %v", chainRunID.String, runID, err)
		return true
	}
	return int(stepIndex.Int64)+1 < len(steps)
}
