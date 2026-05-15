// User-driven cancellation and the failure-finalization helpers a
// cancelled or errored run uses to reach a terminal DB state +
// surface a toast.

package delegate

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

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

	flipped, err := s.agentRuns.MarkCancelledIfActive(context.Background(), runmode.LocalDefaultOrg, runID, "user_cancelled", "Run cancelled by user")
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
	if err := s.agentRuns.Complete(context.Background(), runmode.LocalDefaultOrg, runID, "cancelled", 0, elapsed, 0, "cancelled", "Cancelled by user"); err != nil {
		log.Printf("[delegate] warning: failed to record cancellation for run %s: %v", runID, err)
	}
	s.broadcastRunUpdate(runID, "cancelled")
	if wtPath != "" {
		// Best-effort cleanup; same rationale as the defer in runAgent.
		_ = worktree.RemoveAt(wtPath, runID)
	}
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

	if _, err := s.agentRuns.InsertMessage(context.Background(), runmode.LocalDefaultOrg, &domain.AgentMessage{
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
