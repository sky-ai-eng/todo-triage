// The Spawner type — central coordinator for delegated agent runs — and
// the small cross-cutting helpers (status broadcasts, status updates,
// drainer/classification wiring) every other file in this package
// reaches for. The lifecycle methods (Delegate, Cancel, Takeover,
// Release, ResumeAfterYield) live in their own files; this one is the
// type definition + the bits that don't belong anywhere else.

package delegate

import (
	"context"
	"database/sql"
	"log"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
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
	prompts  db.PromptStore
	agents   db.AgentStore // SKY-261: resolves actor for run.actor_agent_id stamping
	chains   db.ChainStore
	wsHub    *websocket.Hub

	mu                    sync.Mutex
	ghClient              *ghclient.Client
	model                 string
	cancels               map[string]context.CancelFunc              // runID → cancel the entire run
	drainer               QueueDrainer                               // nil-safe; set post-construction via SetQueueDrainer
	takenOver             map[string]bool                            // runIDs claimed by Takeover. Sticky-on for the rest of the goroutine's lifetime even after rollback — clearing the entry would let late-firing goroutine gates race the takeover/abort lifecycle. Suppresses every cleanup path in runAgent so Takeover/abortTakeover own the row's terminal state.
	chainRunIDs           map[string]bool                            // chain_run IDs whose setup phase reuses the per-run status helpers but is not backed by a runs row. broadcastRunUpdate skips wsHub emission for these so clients don't fetch /api/runs/{id} and 404.
	waitForClassification func(ctx context.Context, entityID string) // SKY-220 hook: blocks until the project classifier has decided this entity, or a timeout/ctx-cancel elapses. Nil-safe (test setups skip it). Wired in main.go via SetWaitForClassification — keeps internal/delegate from importing internal/projectclassify.

	agentToolsOnce  sync.Once
	agentToolsCache string
}

func NewSpawner(database *sql.DB, prompts db.PromptStore, agents db.AgentStore, chains db.ChainStore, ghClient *ghclient.Client, wsHub *websocket.Hub, model string) *Spawner {
	return &Spawner{
		database:    database,
		prompts:     prompts,
		agents:      agents,
		chains:      chains,
		ghClient:    ghClient,
		wsHub:       wsHub,
		model:       model,
		cancels:     make(map[string]context.CancelFunc),
		takenOver:   make(map[string]bool),
		chainRunIDs: make(map[string]bool),
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

// markChainRunID flags a chain_run id so broadcastRunUpdate skips
// wsHub emission for it. The setup phase of a chain reuses the per-run
// status helpers with the chain_run id (the first step's runs row
// doesn't exist yet) — those UPDATEs are harmless no-ops, but the
// matching WS event causes clients to fetch /api/runs/{id} and 404.
func (s *Spawner) markChainRunID(id string) {
	s.mu.Lock()
	s.chainRunIDs[id] = true
	s.mu.Unlock()
}

func (s *Spawner) unmarkChainRunID(id string) {
	s.mu.Lock()
	delete(s.chainRunIDs, id)
	s.mu.Unlock()
}

func (s *Spawner) isChainRunID(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chainRunIDs[id]
}

func (s *Spawner) broadcastRunUpdate(runID, status string) {
	if s.wsHub == nil {
		return
	}
	if s.isChainRunID(runID) {
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
