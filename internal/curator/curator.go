package curator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// Curator owns the per-project Claude Code chat session lifecycle.
// One instance per process; each project gets its own goroutine on
// first SendMessage. Cross-project messages run in parallel; same-
// project messages queue serially so the conversation history stays
// coherent.
type Curator struct {
	database *sql.DB
	wsHub    *websocket.Hub

	mu       sync.Mutex
	model    string
	sessions map[string]*projectSession // projectID → goroutine handle

	// closed is set during Shutdown; SendMessage rejects after this.
	closed bool
}

// New constructs a Curator. Call db.CancelOrphanedNonTerminalCuratorRequests
// at startup before constructing — see main.go wiring.
func New(database *sql.DB, wsHub *websocket.Hub, model string) *Curator {
	return &Curator{
		database: database,
		wsHub:    wsHub,
		model:    model,
		sessions: make(map[string]*projectSession),
	}
}

// UpdateCredentials hot-swaps the model used for new requests. Mirrors
// delegate.Spawner.UpdateCredentials so a config change applies
// without restarting the binary.
func (c *Curator) UpdateCredentials(model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.model = model
}

// SendMessage records the user's input as a queued curator_request,
// hands it to the project's goroutine, and returns the request id.
// The HTTP handler returns 202 + this id; the per-project goroutine
// flips status to running on pickup and to terminal on completion.
//
// The user's content is required (empty/whitespace-only is rejected
// at the handler before reaching us); the project must exist
// (handler checks). This function does not validate either —
// callers are trusted to pre-check.
//
// Shutdown safety: getOrStartSession holds c.mu and refuses to
// hand back a session once c.closed flips, so a SendMessage that
// races Shutdown either (a) wins the lock first and gets a session
// that Shutdown then tears down — the session ctx kills the dispatch
// before it spawns claude — or (b) loses the lock and gets nil back,
// in which case the persisted row is flipped to cancelled before we
// return. Either way, no message reaches a non-running goroutine.
func (c *Curator) SendMessage(projectID, content string) (string, error) {
	requestID, err := db.CreateCuratorRequest(c.database, projectID, content)
	if err != nil {
		return "", fmt.Errorf("create curator request: %w", err)
	}

	session := c.getOrStartSession(projectID)
	if session == nil {
		_, _ = db.MarkCuratorRequestCancelledIfActive(c.database, requestID, "curator is shut down")
		return "", errors.New("curator is shut down")
	}

	select {
	case session.queue <- requestID:
		c.broadcastRequestUpdate(projectID, requestID, "queued")
		return requestID, nil
	default:
		// Queue is full — should not happen at the per-project depth
		// we configure, but if it ever does, fail the row up-front
		// rather than blocking the HTTP handler.
		_, _ = db.CompleteCuratorRequest(c.database, requestID, "failed", "curator queue full", 0, 0, 0)
		c.broadcastRequestUpdate(projectID, requestID, "failed")
		return "", errors.New("curator queue is full")
	}
}

// Cancel fires the per-project cancel func, terminating the in-flight
// agentproc.Run. The goroutine flips the row to cancelled when it
// observes ctx.Err(). Returns nil even if no in-flight goroutine
// exists — the typical race between user click and goroutine
// scheduling means "nothing to cancel" is a routine outcome rather
// than an error. Caller decides whether to surface it as 404 by
// checking InFlightCuratorRequestForProject first.
func (c *Curator) Cancel(projectID string) {
	c.mu.Lock()
	session, ok := c.sessions[projectID]
	c.mu.Unlock()
	if !ok {
		return
	}
	session.cancelInFlight()
}

// CancelProject is the project-delete hook: cancel any in-flight
// request, drain queued requests to cancelled (so the deleted
// project doesn't have ghost queued rows), and stop the goroutine
// so nothing runs after the project row is gone.
//
// Called BEFORE the projects DELETE so the FK cascade doesn't
// race a still-running goroutine. The DB cascade (curator_requests
// → curator_messages) takes care of the row removal once the
// project row is dropped.
func (c *Curator) CancelProject(projectID string) {
	c.mu.Lock()
	session, ok := c.sessions[projectID]
	if ok {
		delete(c.sessions, projectID)
	}
	c.mu.Unlock()

	if !ok {
		// No goroutine ever started — but there may still be queued
		// rows from a previous process that the goroutine never got
		// a chance to drain. Cancel them at the DB level so the
		// FK cascade on project delete doesn't leave behind status
		// confusion.
		c.cancelQueuedRows(projectID, "project deleted")
		return
	}
	session.shutdown("project deleted")
	c.cancelQueuedRows(projectID, "project deleted")
}

// Shutdown stops every per-project goroutine and rejects further
// SendMessage calls. Called from main.go on graceful shutdown so
// in-flight CC subprocesses are SIGKILLed before the process exits.
// In-flight rows land as cancelled with reason "process shutting
// down"; queued rows stay queued and are picked up by the next
// process via QueuedCuratorRequestsForProject (recovery path).
func (c *Curator) Shutdown() {
	c.mu.Lock()
	c.closed = true
	sessions := c.sessions
	c.sessions = make(map[string]*projectSession)
	c.mu.Unlock()

	for _, s := range sessions {
		s.shutdown("process shutting down")
	}
}

func (c *Curator) cancelQueuedRows(projectID, reason string) {
	queued, err := db.QueuedCuratorRequestsForProject(c.database, projectID)
	if err != nil {
		log.Printf("[curator] warning: failed to list queued requests for project %s: %v", projectID, err)
		return
	}
	for _, req := range queued {
		flipped, err := db.MarkCuratorRequestCancelledIfActive(c.database, req.ID, reason)
		if err != nil {
			log.Printf("[curator] warning: cancel queued request %s: %v", req.ID, err)
			continue
		}
		if flipped {
			c.broadcastRequestUpdate(projectID, req.ID, "cancelled")
		}
	}
}

// getOrStartSession returns the per-project session, starting a new
// goroutine if needed. Holding c.mu across the start prevents two
// concurrent SendMessage calls for the same project from spawning
// two goroutines, and folds the closed-check into the same critical
// section as the map mutation so a racing Shutdown can't observe a
// "no sessions to stop" snapshot while a fresh session is being
// inserted. Returns nil iff the curator has been shut down — caller
// flips the persisted row to cancelled so it doesn't dangle.
func (c *Curator) getOrStartSession(projectID string) *projectSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	if existing, ok := c.sessions[projectID]; ok {
		return existing
	}
	ctx, cancel := context.WithCancel(context.Background())
	session := &projectSession{
		curator:   c,
		projectID: projectID,
		queue:     make(chan string, sessionQueueDepth),
		ctx:       ctx,
		stopAll:   cancel,
		done:      make(chan struct{}),
	}
	c.sessions[projectID] = session
	go session.run()
	return session
}

// sessionQueueDepth bounds how many user messages can be queued for
// one project ahead of the active one. Set generously for human-
// driven chat — a person can't reasonably backlog more than a
// handful of follow-ups before the answer to the first arrives.
const sessionQueueDepth = 64
