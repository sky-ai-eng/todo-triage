package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

func (s *Server) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	run, err := db.GetAgentRun(s.db, runID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if run == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found"})
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleAgentMessages(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	messages, err := db.MessagesForRun(s.db, runID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if messages == nil {
		messages = []domain.AgentMessage{}
	}
	writeJSON(w, http.StatusOK, messages)
}

func (s *Server) handleAgentCancel(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	if s.spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}
	if err := s.spawner.Cancel(runID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

func (s *Server) handleAgentTakeover(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	if s.spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}

	cfg, err := config.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("load config: %v", err)})
		return
	}
	baseDir, err := cfg.Server.ResolvedTakeoverDir()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("resolve takeover dir: %v", err)})
		return
	}

	// Note: Takeover does NOT take r.Context(). Once it commits
	// (sets the takenOver flag and SIGKILLs the agent) the operation
	// must run to completion or roll back cleanly; tying it to the
	// request context would let a client disconnect destroy the run.
	result, err := s.spawner.Takeover(runID, baseDir)
	if err != nil {
		writeJSON(w, takeoverErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"takeover_path":  result.TakeoverPath,
		"session_id":     result.SessionID,
		"resume_command": fmt.Sprintf("cd %s && claude --resume %s", shellQuote(result.TakeoverPath), shellQuote(result.SessionID)),
	})
}

// shellQuote wraps a path in single quotes for safe shell pasting,
// escaping any embedded single quotes the standard way ('"'"'). Used so
// the resume_command we hand back to the UI is paste-safe even when the
// takeover dir contains spaces or apostrophes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// takeoverErrorStatus maps a Takeover() error to an HTTP status code.
// Validation failures (no session id, no worktree, run not active) are
// 400 — the client asked for something the run state doesn't support.
// Conflicts (already in progress, race-loss) are 409 — the resource
// state shifted in a way the client should re-check. Everything else
// is 500 — filesystem, git subprocess, DB and other internal failures
// are server-side and shouldn't be misclassified as bad client input.
func takeoverErrorStatus(err error) int {
	switch {
	case errors.Is(err, delegate.ErrTakeoverInvalidState):
		return http.StatusBadRequest
	case errors.Is(err, delegate.ErrTakeoverInProgress),
		errors.Is(err, delegate.ErrTakeoverRaceLost):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func (s *Server) handleAgentRuns(w http.ResponseWriter, r *http.Request) {
	taskID := r.URL.Query().Get("task_id")
	if taskID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task_id query parameter required"})
		return
	}
	runs, err := db.AgentRunsForTask(s.db, taskID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if runs == nil {
		runs = []domain.AgentRun{}
	}
	writeJSON(w, http.StatusOK, runs)
}

// handleAgentRespond accepts the user's answer to an open yield and
// resumes the run. SKY-139.
//
// Request body shape:
//
//	{
//	  "type": "confirmation"|"choice"|"prompt",
//	  "accepted": bool,            // confirmation
//	  "selected": ["id1","id2"],   // choice
//	  "value": "free text"          // prompt
//	}
//
// Validation:
//   - run exists and is in awaiting_input
//   - response.type matches the open yield_request's type
//   - choice responses with multi=false carry exactly one selected id;
//     multi=true carries 0+ ids drawn from the request's options
//
// Response: 200 with {run_id, status} on success. The actual resume
// runs in a background goroutine; the client refreshes via the
// existing run-update WS broadcast.
func (s *Server) handleAgentRespond(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	if s.spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}

	var resp domain.YieldResponse
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	run, err := db.GetAgentRun(s.db, runID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if run == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found"})
		return
	}
	if run.Status != "awaiting_input" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "run is not awaiting input (status=" + run.Status + ")"})
		return
	}

	req, err := db.LatestYieldRequest(s.db, runID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "load yield request: " + err.Error()})
		return
	}
	if req == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no open yield request for this run"})
		return
	}

	if resp.Type != req.Type {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("response type %q does not match request type %q", resp.Type, req.Type)})
		return
	}
	if errMsg := validateYieldResponse(req, &resp); errMsg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
		return
	}

	// Insert yield_response message + flip status atomically-enough:
	// if MarkAgentRunResuming returns ok=false (a concurrent cancel
	// raced us), the response message is still recorded on the run
	// for transcript completeness — the racing path took the run to
	// a terminal state and we just stop here.
	displayContent := domain.RenderYieldResponseForDisplay(req, &resp)
	msg, err := db.InsertYieldResponse(s.db, runID, &resp, displayContent)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "record response: " + err.Error()})
		return
	}
	s.ws.Broadcast(websocket.Event{Type: "agent_message", RunID: runID, Data: msg})

	flipped, err := db.MarkAgentRunResuming(s.db, runID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "flip status: " + err.Error()})
		return
	}
	if !flipped {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "run is no longer awaiting input"})
		return
	}
	s.ws.Broadcast(websocket.Event{Type: "agent_run_update", RunID: runID, Data: map[string]string{"status": "running"}})

	agentText := domain.RenderYieldResponseForAgent(req, &resp)
	if err := s.spawner.ResumeAfterYield(runID, agentText); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "resume: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"run_id": runID, "status": "running"})
}

// validateYieldResponse enforces type-specific shape rules. Returns
// "" on success, an error message otherwise.
func validateYieldResponse(req *domain.YieldRequest, resp *domain.YieldResponse) string {
	switch resp.Type {
	case domain.YieldTypeConfirmation:
		// Both true and false are valid; nothing else to check.
		return ""
	case domain.YieldTypeChoice:
		if !req.Multi && len(resp.Selected) != 1 {
			return fmt.Sprintf("single-choice yield requires exactly one selection, got %d", len(resp.Selected))
		}
		valid := make(map[string]struct{}, len(req.Options))
		for _, o := range req.Options {
			valid[o.ID] = struct{}{}
		}
		for _, id := range resp.Selected {
			if _, ok := valid[id]; !ok {
				return "selected id not in request options: " + id
			}
		}
		return ""
	case domain.YieldTypePrompt:
		// Empty prompt responses are accepted; the agent sees a
		// neutral "user submitted an empty response" message and
		// decides what to do.
		return ""
	}
	return "unknown yield type: " + resp.Type
}

// WSHub returns the websocket hub for use by the delegation spawner.
func (s *Server) WSHub() *websocket.Hub {
	return s.ws
}
