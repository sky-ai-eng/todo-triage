package curator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
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

	project, err := db.GetProject(s.curator.database, s.projectID)
	if err != nil {
		s.failRequest(requestID, fmt.Sprintf("load project: %v", err))
		return
	}
	if project == nil {
		s.failRequest(requestID, "project missing")
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
		return
	}

	systemPrompt := buildSystemPrompt(project.Name)

	outcome, runErr := agentproc.Run(msgCtx, agentproc.RunOptions{
		Cwd:          cwd,
		Model:        model,
		SessionID:    project.CuratorSessionID,
		Message:      req.UserInput,
		SystemPrompt: systemPrompt,
		AllowedTools: BuildAllowedTools(),
		ExtraEnv: []string{
			"TRIAGE_FACTORY_CURATOR_PROJECT_ID=" + s.projectID,
			"TRIAGE_FACTORY_CURATOR_REQUEST_ID=" + requestID,
		},
		TraceID: requestID,
	}, newRequestSink(s.curator, s.projectID, requestID))

	// Cancellation observed → terminal cancelled status. Distinguish
	// between request-level cancellation and broader session/project
	// shutdown so the recorded terminal reason is accurate.
	if msgCtx.Err() != nil {
		cancelReason := "user cancelled"
		if s.ctx.Err() != nil {
			cancelReason = "session cancelled"
		}
		s.markCancelled(requestID, cancelReason)
		return
	}

	if runErr != nil && (outcome == nil || outcome.Result == nil) {
		stderr := ""
		if outcome != nil {
			stderr = outcome.Stderr
		}
		s.failRequest(requestID, fmt.Sprintf("%v\nstderr: %s", runErr, stderr))
		return
	}

	if outcome == nil || outcome.Result == nil {
		s.failRequest(requestID, "claude exited without producing a result event")
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
		return
	}
	if !flipped {
		// The row was already terminal — most likely a user cancel
		// landed during agentproc.Run and the handler beat us to the
		// DB. Don't broadcast a status change that doesn't match the
		// row's actual state; the cancel handler already broadcast
		// cancelled.
		log.Printf("[curator] request %s already terminal, skipping completion broadcast (intended status: %s)", requestID, status)
		return
	}
	s.curator.broadcastRequestUpdate(s.projectID, requestID, status)
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

// buildSystemPrompt is intentionally minimal for v1 (SKY-216). The
// per-entity classifier (SKY-220) and Curator tools (SKY-221) will
// flesh this out with actual project context. Until then the prompt
// just orients the agent — no knowledge files, no entity catalog,
// no tools beyond the default toolbelt scoped to the project dir.
func buildSystemPrompt(projectName string) string {
	if projectName == "" {
		return "You are the Curator for an unnamed project. Keep responses concise."
	}
	return fmt.Sprintf("You are the Curator for project %q. Keep responses concise.", projectName)
}
