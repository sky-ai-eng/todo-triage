package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Curator chat endpoints (SKY-216). Three operations the Projects
// page (SKY-217) needs:
//
//   - POST .../messages   queue a user turn → 202 + {request_id}
//   - GET  .../messages   chat history (requests + their messages)
//   - DELETE .../in-flight cancel the active turn → 204
//
// All three are scoped to a single project. The runtime in
// internal/curator owns concurrency: cross-project messages run in
// parallel; same-project messages queue serially behind one CC
// subprocess at a time.

type curatorSendRequest struct {
	Content string `json:"content"`
}

type curatorSendResponse struct {
	RequestID string `json:"request_id"`
}

// curatorRequestJSON is the wire shape for a Curator chat turn. Combines
// the request row (status + accounting) with its agent-side message
// stream so the frontend can render the entire conversation in one
// query rather than fetching messages per-request.
type curatorRequestJSON struct {
	domain.CuratorRequest
	Messages []domain.CuratorMessage `json:"messages"`
}

func (s *Server) handleCuratorSend(w http.ResponseWriter, r *http.Request) {
	if s.curator == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "curator runtime not started"})
		return
	}
	projectID := r.PathValue("id")
	project, err := db.GetProject(s.db, projectID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if project == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}

	var req curatorSendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	content := strings.TrimSpace(req.Content)
	if content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "content is required"})
		return
	}

	requestID, err := s.curator.SendMessage(projectID, content)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, curatorSendResponse{RequestID: requestID})
}

func (s *Server) handleCuratorHistory(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	project, err := db.GetProject(s.db, projectID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if project == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}

	requests, err := db.ListCuratorRequestsByProject(s.db, projectID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	out := make([]curatorRequestJSON, 0, len(requests))
	for _, req := range requests {
		messages, err := db.ListCuratorMessagesByRequest(s.db, req.ID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		out = append(out, curatorRequestJSON{
			CuratorRequest: req,
			Messages:       messages,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCuratorCancel terminates the active in-flight turn. Returns
// 404 if there's no queued or running request for the project — the
// frontend uses that to clear stale "cancelling…" state when the
// agent finished between the user's click and the request landing.
func (s *Server) handleCuratorCancel(w http.ResponseWriter, r *http.Request) {
	if s.curator == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "curator runtime not started"})
		return
	}
	projectID := r.PathValue("id")
	project, err := db.GetProject(s.db, projectID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if project == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}

	inFlight, err := db.InFlightCuratorRequestForProject(s.db, projectID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if inFlight == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no in-flight curator request"})
		return
	}

	// Two-step: tell the runtime to fire the cancel ctx (kills the
	// CC subprocess if one is running), then flip the row at the
	// DB level so a queued (not-yet-running) request is also handled.
	// The runtime's session goroutine writes the same terminal
	// status when it observes ctx.Err(); the status filter in
	// MarkCuratorRequestCancelledIfActive makes the second write
	// a no-op.
	s.curator.Cancel(projectID)
	if _, err := db.MarkCuratorRequestCancelledIfActive(s.db, inFlight.ID, "user cancelled"); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
