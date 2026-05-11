package curator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// projectSession is the per-project goroutine handle. One queue, one
// in-flight cancel handle, one ctx that bounds the whole goroutine's
// lifetime. The Curator type holds these in a map keyed by project id.
type projectSession struct {
	curator   *Curator
	projectID string
	queue     chan string

	// ctx + stopAll bound the lifetime of the whole goroutine. Closed
	// during Curator.Shutdown or CancelProject; the goroutine drops
	// any in-flight subprocess via the per-message inFlightCancel and
	// then exits.
	ctx     context.Context
	stopAll context.CancelFunc

	// done closes when the run() goroutine returns. Shutdown blocks
	// on this so the process exits cleanly: the goroutine writes its
	// terminal cancelled status BEFORE we let the database close out
	// from under it. Without the wait, a graceful shutdown would
	// race with the goroutine's last DB write and log spurious
	// "database is closed" errors.
	done chan struct{}

	// inFlightMu guards inFlightCancel and inFlightRequestID — the
	// per-message ctx is recreated for each agentproc.Run invocation
	// and the cancel button reads it from outside the goroutine.
	inFlightMu        sync.Mutex
	inFlightCancel    context.CancelFunc
	inFlightRequestID string
}

// run drains the queue serially. Exits when ctx is cancelled (via
// shutdown) or the queue is closed. On exit, any in-flight subprocess
// has already been SIGKILLed via inFlightCancel (the dispatch path
// triggers it before exiting). Future SendMessage on the same project
// will spin up a fresh goroutine.
//
// Closing s.done on return is what unblocks Shutdown's wait — the
// pair guarantees a deterministic teardown order during process exit.
func (s *projectSession) run() {
	defer close(s.done)
	for {
		select {
		case <-s.ctx.Done():
			return
		case requestID, ok := <-s.queue:
			if !ok {
				return
			}
			s.dispatch(requestID)
		}
	}
}

// dispatch processes one queued request. Owns the row's lifecycle
// from queued → running → terminal; broadcasts each transition so
// the Projects page can update without re-fetching.
//
// Cancel ordering: msgCtx and inFlightCancel are registered BEFORE
// MarkCuratorRequestRunning so that by the time any external observer
// can see the row in `running` state, the cancel handle is already
// armed. Without this, a cancel that landed in the window between
// "row is running" and "inFlightCancel registered" would see a nil
// cancel handle and be a no-op — the goroutine would then run
// agentproc to completion, and even though the cancel handler also
// flips the row at the DB level, the goroutine's terminal write
// could clobber it. The SQL filter on CompleteCuratorRequest
// belt-and-suspenders that, but registering early closes the race
// window in the first place.
func (s *projectSession) dispatch(requestID string) {
	if err := s.ctx.Err(); err != nil {
		// Shutdown raced ahead of the dequeue — flip the row to
		// cancelled so it doesn't sit forever in queued.
		s.markCancelled(requestID, "process shutting down")
		return
	}

	// Per-message ctx is a child of the session ctx. SIGKILL of the
	// in-flight subprocess goes through this; cancelInFlight fires
	// it from outside the goroutine.
	msgCtx, msgCancel := context.WithCancel(s.ctx)
	s.inFlightMu.Lock()
	s.inFlightCancel = msgCancel
	s.inFlightRequestID = requestID
	s.inFlightMu.Unlock()

	defer func() {
		s.inFlightMu.Lock()
		s.inFlightCancel = nil
		s.inFlightRequestID = ""
		s.inFlightMu.Unlock()
		msgCancel()
	}()

	if err := db.MarkCuratorRequestRunning(s.curator.database, requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Already terminal — usually because Cancel raced before
			// pickup. Skip; the canceller already wrote the row.
			return
		}
		// The request has already been dequeued from the in-memory
		// project queue. If we return here without a terminal state,
		// the row remains stuck in queued with no retry path.
		log.Printf("[curator] warning: mark request %s running: %v", requestID, err)
		s.failRequest(requestID, fmt.Sprintf("mark running: %v", err))
		return
	}
	s.curator.broadcastRequestUpdate(s.projectID, requestID, "running")

	// Cancel could have fired during MarkRunning's DB call. Check
	// before doing any further work so we don't pointlessly load
	// the project / spawn claude on a cancelled request.
	if msgCtx.Err() != nil {
		s.markCancelled(requestID, "user cancelled")
		return
	}

	req, err := db.GetCuratorRequest(s.curator.database, requestID)
	if err != nil {
		s.failRequest(requestID, fmt.Sprintf("load request: %v", err))
		return
	}
	if req == nil {
		s.failRequest(requestID, "request not found")
		return
	}

	cwd, err := ensureKnowledgeDir(s.projectID)
	if err != nil {
		s.failRequest(requestID, fmt.Sprintf("knowledge dir: %v", err))
		return
	}

	// Consume pending context-change rows AND read the project state in
	// one transaction. This is intentional: the diff at the bottom of
	// this function compares each pending row's baseline against the
	// project's current value, and if those two reads were independent
	// a PATCH that landed between them could be claimed here while the
	// envelope (built from the older read) showed values matching the
	// baseline — the diff would suppress the note, finalize on `done`,
	// and the user's delta would be lost. ConsumePendingContext returns
	// the project state alongside the claimed rows so every downstream
	// step (materialize, envelope render, diff) sees the same snapshot.
	//
	// Two-phase consume: rows are *claimed* here (consumed_at +
	// consumed_by_request_id stamped) but not deleted. On terminal
	// `done` we finalize (purge); on `cancelled` or `failed` we
	// revert (un-consume) so a transient agentproc failure doesn't
	// silently lose the user's deltas. The merge logic in
	// RevertPendingContext handles the case where a NEW PATCH lands
	// during dispatch.
	project, pending, err := db.ConsumePendingContext(s.curator.database, s.projectID, requestID)
	if err != nil {
		s.failRequest(requestID, fmt.Sprintf("consume pending context: %v", err))
		return
	}
	if project == nil {
		s.failRequest(requestID, "project missing")
		return
	}

	// Refresh pinned-repo worktrees before spawning the agent so its
	// view of the world matches upstream HEAD on the user-configured
	// branch (profile.BaseBranch || profile.DefaultBranch). One fetch
	// + reset --hard per repo per dispatch — bounded by the bare's
	// per-repo lock so concurrent dispatches in different projects
	// pinning the same repo queue rather than race. Per-repo
	// failures are non-fatal: the agent still gets the project's
	// knowledge files plus whatever subset of repos materialized.
	materializePinnedRepos(msgCtx, s.curator.database, s.projectID, cwd, project.PinnedRepos)
	if msgCtx.Err() != nil {
		// Cancel fired during repo refresh (one big bare clone can
		// take seconds on a fresh fetch). Don't waste cycles spawning
		// claude only to immediately cancel it.
		s.markCancelled(requestID, "user cancelled")
		s.revertPendingFor(requestID)
		return
	}

	s.curator.mu.Lock()
	model := s.curator.model
	s.curator.mu.Unlock()

	// Pre-flight model check before we spawn claude. The Curator
	// constructor takes "" until config loads (mirroring Spawner),
	// and a SendMessage that lands during that window would
	// otherwise reach agentproc.Run and emit a confusing
	// "missing --model" error from claude itself. Fail the row up
	// front so the user sees a clear message.
	if model == "" {
		s.failRequest(requestID, "curator AI model is not configured")
		s.revertPendingFor(requestID)
		return
	}

	// Resolve selfBin so the allowlist's `Bash(<selfBin> exec *)`
	// pattern matches the same absolute path the agent will invoke
	// for SKY-221's "ticket as a spec" skill. Falling back to a
	// hard fail rather than running with a broken allowlist —
	// `os.Executable()` errors are vanishingly rare, but if one
	// happens we'd silently disable curator tooling.
	selfBin, err := os.Executable()
	if err != nil {
		s.failRequest(requestID, fmt.Sprintf("resolve own binary path: %v", err))
		s.revertPendingFor(requestID)
		return
	}

	envelope := envelopeInputs{
		ProjectName:        project.Name,
		ProjectDescription: project.Description,
		PinnedRepos:        project.PinnedRepos,
		JiraProjectKey:     project.JiraProjectKey,
		LinearProjectKey:   project.LinearProjectKey,
		BinaryPath:         selfBin,
	}
	systemPrompt := renderEnvelope(envelope)

	message := req.UserInput
	contextNote := pendingChangesNote(pending, envelope)
	if contextNote != "" {
		message = contextNote + "\n\n" + message
		// Persist the rendered note as a curator_messages audit row
		// keyed to the consuming request. Frontend filters subtype
		// `context_change` out of rendered chat (SKY-226), but having
		// the row keyed to request_id makes the chat history
		// reproducible: replay shows exactly what the agent saw.
		// Best-effort — failing to write the audit row should not
		// abort the dispatch.
		auditMsg := &domain.CuratorMessage{
			RequestID: requestID,
			Role:      "system",
			Subtype:   "context_change",
			Content:   contextNote,
		}
		if _, auditErr := db.InsertCuratorMessage(s.curator.database, auditMsg); auditErr != nil {
			log.Printf("[curator] warning: insert context_change audit row for %s: %v", requestID, auditErr)
		}
	}

	// Allow the rm guard (and other path-scoped tool checks) to reach
	// the knowledge-base + repos subtrees explicitly. By default Claude
	// Code's rm policy treats the cwd as the sole allowed dir; without
	// these the agent can read/write files via Edit/Write but cannot
	// delete obsolete knowledge notes.
	addDirs := []string{
		filepath.Join(cwd, "knowledge-base"),
		filepath.Join(cwd, "repos"),
	}

	// Materialize the project's spec-authorship prompt as a Claude Code
	// skill at <cwd>/.claude/skills/ticket-spec/SKILL.md. Written fresh
	// on every dispatch so prompt edits + per-project re-targeting both
	// take effect on the next turn without a session reset (SKY-221).
	// Failure is non-fatal: the user's chat turn should still answer
	// even if skill writing hits a permission glitch.
	if err := materializeSpecSkill(s.curator.database, s.curator.prompts, project, cwd); err != nil {
		log.Printf("[curator] warning: materialize spec skill for project %s: %v", s.projectID, err)
	}
	if err := materializeJiraFormattingSkill(cwd); err != nil {
		log.Printf("[curator] warning: materialize jira formatting skill for project %s: %v", s.projectID, err)
	}

	outcome, runErr := agentproc.Run(msgCtx, agentproc.RunOptions{
		Cwd:          cwd,
		Model:        model,
		SessionID:    project.CuratorSessionID,
		Message:      message,
		SystemPrompt: systemPrompt,
		AllowedTools: agentproc.BuildAllowedTools(selfBin),
		AddDirs:      addDirs,
		ExtraEnv: []string{
			"TRIAGE_FACTORY_CURATOR_PROJECT_ID=" + s.projectID,
			"TRIAGE_FACTORY_CURATOR_REQUEST_ID=" + requestID,
		},
		TraceID: requestID,
	}, newRequestSink(s.curator, s.projectID, requestID))

	// Cancellation observed → terminal cancelled status. Distinguish
	// between request-level cancellation and broader session/project
	// shutdown so the recorded terminal reason is accurate. Pending
	// rows are reverted so the next user message picks them up again.
	if msgCtx.Err() != nil {
		cancelReason := "user cancelled"
		if s.ctx.Err() != nil {
			cancelReason = "session cancelled"
		}
		s.markCancelled(requestID, cancelReason)
		s.revertPendingFor(requestID)
		return
	}

	if runErr != nil && (outcome == nil || outcome.Result == nil) {
		stderr := ""
		if outcome != nil {
			stderr = outcome.Stderr
		}
		s.failRequest(requestID, fmt.Sprintf("%v\nstderr: %s", runErr, stderr))
		s.revertPendingFor(requestID)
		return
	}

	if outcome == nil || outcome.Result == nil {
		s.failRequest(requestID, "claude exited without producing a result event")
		s.revertPendingFor(requestID)
		return
	}

	status := "done"
	errMsg := ""
	if outcome.Result.IsError {
		status = "failed"
		errMsg = outcome.Result.Result
	}
	flipped, err := db.CompleteCuratorRequest(
		s.curator.database, requestID, status, errMsg,
		outcome.Result.CostUSD, outcome.Result.DurationMs, outcome.Result.NumTurns,
	)
	if err != nil {
		log.Printf("[curator] warning: complete request %s: %v", requestID, err)
		// We don't know whether the row landed terminal. Revert the
		// pending rows on the conservative assumption that the agent
		// did not see them — if the row turns out to be `done` after
		// all, the worst case is the user gets a duplicate diff on
		// their next message, which is far better than silently
		// losing the deltas.
		s.revertPendingFor(requestID)
		return
	}
	if !flipped {
		// The row was already terminal — most likely a user cancel
		// landed during agentproc.Run and the handler beat us to the
		// DB. Don't broadcast a status change that doesn't match the
		// row's actual state; the cancel handler already broadcast
		// cancelled. Pending rows: the cancel path will revert them
		// when it observes msgCtx.Err() above, but we may have
		// reached this branch from a successful agentproc with the
		// cancel landing concurrently — revert here too as a
		// belt-and-suspenders for the "row was already cancelled
		// before our completion write" race.
		log.Printf("[curator] request %s already terminal, skipping completion broadcast (intended status: %s)", requestID, status)
		s.revertPendingFor(requestID)
		return
	}
	if status == "done" {
		s.finalizePendingFor(requestID)
	} else {
		// Terminal `failed` from agentproc's IsError result: the agent
		// emitted a result event marking the turn as a failure. Treat
		// the same as a process-level failure for pending-row
		// purposes — user retry should re-see the deltas.
		s.revertPendingFor(requestID)
	}
	s.curator.broadcastRequestUpdate(s.projectID, requestID, status)
}

// finalizePendingFor purges every pending-context row consumed by this
// request. Best-effort logging — finalization failure leaves stale
// rows that the next user message will skip (they are already marked
// consumed) but does not poison the chat or block other dispatches.
func (s *projectSession) finalizePendingFor(requestID string) {
	if err := db.FinalizePendingContext(s.curator.database, requestID); err != nil {
		log.Printf("[curator] warning: finalize pending context for %s: %v", requestID, err)
	}
}

// revertPendingFor un-consumes every pending-context row claimed by
// this request so the next user message picks them up again. Also
// removes the curator_messages audit row keyed to this request so the
// chat history doesn't show a phantom "context noted" entry for a
// turn that never delivered the deltas.
func (s *projectSession) revertPendingFor(requestID string) {
	if err := db.RevertPendingContext(s.curator.database, requestID); err != nil {
		log.Printf("[curator] warning: revert pending context for %s: %v", requestID, err)
	}
	if err := db.DeleteCuratorMessagesBySubtype(s.curator.database, requestID, "context_change"); err != nil {
		log.Printf("[curator] warning: delete context_change audit for %s: %v", requestID, err)
	}
}

// cancelInFlight fires the active message's ctx if one exists.
// Called from Curator.Cancel (user click) and Curator.CancelProject
// (project delete). The goroutine observes msgCtx.Err() in its
// agentproc.Run return and writes the cancelled terminal status
// itself — cancelInFlight only sends the signal.
func (s *projectSession) cancelInFlight() {
	s.inFlightMu.Lock()
	cancel := s.inFlightCancel
	s.inFlightMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// shutdown cancels the session ctx (kills any in-flight subprocess),
// stops the goroutine, and waits for it to fully drain before
// returning. Reason becomes the error message on any in-flight row's
// terminal cancellation.
//
// Blocking on the goroutine's exit matters for graceful shutdown:
// Shutdown is called as part of process teardown, and the goroutine's
// terminal write to curator_requests must happen before the DB closes
// underneath it. The wait is bounded by the agentproc subprocess
// honoring ctx.Done() promptly (it does — exec.CommandContext
// SIGKILLs the process group) so callers don't have to time out.
func (s *projectSession) shutdown(reason string) {
	// Capture the in-flight request id before the goroutine has a
	// chance to clear it on its own ctx.Err observation, so the
	// terminal status carries the shutdown reason rather than the
	// goroutine's default.
	s.inFlightMu.Lock()
	inFlightID := s.inFlightRequestID
	s.inFlightMu.Unlock()

	s.stopAll()

	// If a request was in flight, flip it explicitly with the
	// reason. The goroutine's own ctx.Err handler may also flip
	// it; MarkCuratorRequestCancelledIfActive's status filter
	// makes the second write a no-op.
	if inFlightID != "" {
		if flipped, err := db.MarkCuratorRequestCancelledIfActive(s.curator.database, inFlightID, reason); err == nil && flipped {
			s.curator.broadcastRequestUpdate(s.projectID, inFlightID, "cancelled")
		}
	}

	<-s.done
}

func (s *projectSession) markCancelled(requestID, reason string) {
	flipped, err := db.MarkCuratorRequestCancelledIfActive(s.curator.database, requestID, reason)
	if err != nil {
		log.Printf("[curator] warning: cancel request %s: %v", requestID, err)
		return
	}
	if flipped {
		s.curator.broadcastRequestUpdate(s.projectID, requestID, "cancelled")
	}
}

func (s *projectSession) failRequest(requestID, errMsg string) {
	flipped, err := db.CompleteCuratorRequest(s.curator.database, requestID, "failed", errMsg, 0, 0, 0)
	if err != nil {
		log.Printf("[curator] warning: fail request %s: %v", requestID, err)
		return
	}
	if !flipped {
		// Cancel raced ahead of the failure write. Cancelled wins;
		// the handler already broadcast that.
		return
	}
	s.curator.broadcastRequestUpdate(s.projectID, requestID, "failed")
}
