package curator

import (
	"fmt"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// requestSink translates agentproc stream events into curator_messages
// rows + websocket pushes for one in-flight request. One sink per
// agentproc.Run call (constructed on each message dispatch in the
// per-project goroutine). The projectID and requestID are captured at
// construction so the broadcasts are keyed correctly when many
// projects' Curators are streaming simultaneously.
//
// Session id capture: only the very first message in a project's
// lifetime sees a fresh init event with a new session_id. Subsequent
// requests resume against the captured id so the init they emit
// re-broadcasts the same id. The persistOnce guard keeps us from
// writing the same value twice per request even though SetProjectCuratorSessionID
// is idempotent at the DB layer.
type requestSink struct {
	curator   *Curator
	projectID string
	requestID string

	// onceMu guards persistOnce + sessionID updates so a future
	// concurrent ParseLine wouldn't double-persist if the underlying
	// stream parser ever changed. agentproc drives the sink from one
	// goroutine today, but documenting the invariant defensively
	// matches how the delegate sink handles the same hazard.
	onceMu       sync.Mutex
	sessionState struct {
		persisted bool
	}
}

func newRequestSink(c *Curator, projectID, requestID string) *requestSink {
	return &requestSink{curator: c, projectID: projectID, requestID: requestID}
}

// OnSession persists the captured session_id on the project row the
// first time it's observed in the request's lifetime. Subsequent
// resumes within the same project re-emit the same id and the
// persisted-flag short-circuits the redundant write.
func (s *requestSink) OnSession(sessionID string) error {
	s.onceMu.Lock()
	if s.sessionState.persisted {
		s.onceMu.Unlock()
		return nil
	}
	s.onceMu.Unlock()

	if err := db.SetProjectCuratorSessionID(s.curator.database, s.projectID, sessionID); err != nil {
		return fmt.Errorf("persist curator session_id: %w", err)
	}

	s.onceMu.Lock()
	if !s.sessionState.persisted {
		s.sessionState.persisted = true
	}
	s.onceMu.Unlock()
	return nil
}

// OnMessage inserts the parsed assistant or tool message into
// curator_messages and broadcasts it to the websocket so the open
// project page paints it as it arrives. Per-row failures are
// returned to agentproc which logs and continues.
func (s *requestSink) OnMessage(msg *domain.AgentMessage) error {
	curatorMsg := &domain.CuratorMessage{
		RequestID:           s.requestID,
		Role:                msg.Role,
		Subtype:             msg.Subtype,
		Content:             msg.Content,
		ToolCalls:           msg.ToolCalls,
		ToolCallID:          msg.ToolCallID,
		IsError:             msg.IsError,
		Metadata:            msg.Metadata,
		Model:               msg.Model,
		InputTokens:         msg.InputTokens,
		OutputTokens:        msg.OutputTokens,
		CacheReadTokens:     msg.CacheReadTokens,
		CacheCreationTokens: msg.CacheCreationTokens,
		CreatedAt:           msg.CreatedAt,
	}
	id, err := db.InsertCuratorMessage(s.curator.database, curatorMsg)
	if err != nil {
		return fmt.Errorf("insert curator message: %w", err)
	}
	curatorMsg.ID = int(id)
	s.curator.broadcastMessage(s.projectID, curatorMsg)
	return nil
}

// Compile-time check that requestSink satisfies the agentproc.Sink
// contract.
var _ agentproc.Sink = (*requestSink)(nil)

// broadcastMessage pushes a CuratorMessage onto the websocket. Empty
// hub is tolerated (test harnesses construct curators without a hub).
func (c *Curator) broadcastMessage(projectID string, msg *domain.CuratorMessage) {
	if c.wsHub == nil {
		return
	}
	c.wsHub.Broadcast(websocket.Event{
		Type:      "curator_message",
		ProjectID: projectID,
		Data:      msg,
	})
}

// broadcastRequestUpdate pushes a status transition for a request.
// Frontend uses this to flip the UI from "queued" → "running" →
// terminal without re-fetching the request row.
func (c *Curator) broadcastRequestUpdate(projectID, requestID, status string) {
	if c.wsHub == nil {
		return
	}
	c.wsHub.Broadcast(websocket.Event{
		Type:      "curator_request_update",
		ProjectID: projectID,
		Data: map[string]string{
			"request_id": requestID,
			"status":     status,
		},
	})
}
