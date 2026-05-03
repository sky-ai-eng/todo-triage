package delegate

import (
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// runSink adapts an agentproc invocation to the delegate's storage:
// session ids land on runs.session_id, parsed messages land in
// run_messages, and both fan out to the websocket so the UI can
// react in real time.
//
// One sink per agentproc.Run call (initial invocation + each resume
// own a fresh sink). The runID is captured at construction so every
// row + broadcast is keyed correctly even when the spawner is
// servicing many runs concurrently.
type runSink struct {
	spawner *Spawner
	runID   string

	// sessionDelivered guards against double-write on resume — a
	// resumed stream re-emits system/init for the same session_id,
	// and while SetAgentRunSession is idempotent at the DB layer,
	// skipping the redundant write also avoids a second running-
	// status broadcast that the historical inline implementation
	// gated on this being a first-time event.
	sessionDelivered bool
}

func newRunSink(s *Spawner, runID string) *runSink {
	return &runSink{spawner: s, runID: runID}
}

// OnSession persists the captured session_id and re-broadcasts the
// running status so the UI re-fetches the run row and picks up
// SessionID. The "Take over" button is gated on session id presence;
// without this nudge it stays hidden until the next status flip
// (often "running" → terminal), which is too late to be useful.
func (k *runSink) OnSession(sessionID string) error {
	if k.sessionDelivered {
		return nil
	}
	k.sessionDelivered = true
	if err := db.SetAgentRunSession(k.spawner.database, k.runID, sessionID); err != nil {
		return fmt.Errorf("persist session_id: %w", err)
	}
	k.spawner.broadcastRunUpdate(k.runID, "running")
	return nil
}

// OnMessage inserts the parsed assistant/tool message into
// run_messages and pushes it onto the websocket. Per-row failures
// are returned to agentproc, which logs and continues — losing one
// row is preferable to abandoning the run.
func (k *runSink) OnMessage(msg *domain.AgentMessage) error {
	id, err := db.InsertAgentMessage(k.spawner.database, msg)
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	msg.ID = int(id)
	k.spawner.broadcastMessage(k.runID, msg)
	return nil
}

// Compile-time check that runSink satisfies the agentproc.Sink
// contract.
var _ agentproc.Sink = (*runSink)(nil)
