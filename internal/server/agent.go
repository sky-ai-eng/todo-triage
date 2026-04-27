package server

import (
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
		"takeover_path":   result.TakeoverPath,
		"session_id":      result.SessionID,
		"resume_command":  fmt.Sprintf("cd %s && claude --resume %s", shellQuote(result.TakeoverPath), shellQuote(result.SessionID)),
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

// WSHub returns the websocket hub for use by the delegation spawner.
func (s *Server) WSHub() *websocket.Hub {
	return s.ws
}
