package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentmeta"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// pendingPRJSON is the JSON shape the pending-PR overlay consumes.
// Mirrors pendingReviewJSON but for PR queue state.
type pendingPRJSON struct {
	ID          string `json:"id"`
	RunID       string `json:"run_id,omitempty"`
	Owner       string `json:"owner"`
	Repo        string `json:"repo"`
	HeadBranch  string `json:"head_branch"`
	HeadSHA     string `json:"head_sha"`
	BaseBranch  string `json:"base_branch"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	Draft       bool   `json:"draft"`
	Locked      bool   `json:"locked"`
	SubmittedAt string `json:"submitted_at,omitempty"`
}

// handlePendingPRGet returns a single pending PR.
func (s *Server) handlePendingPRGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	pr, err := s.pendingPRs.Get(r.Context(), runmode.LocalDefaultOrgID, id)
	if err != nil {
		internalError(w, "pending-prs", err)
		return
	}
	if pr == nil {
		notFound(w, "pending PR")
		return
	}

	writeJSON(w, http.StatusOK, pendingPRToJSON(pr))
}

// handleRunPendingPR is the run-keyed lookup the frontend uses to find
// "the pending PR for this delegated run." Mirrors handleRunReview.
func (s *Server) handleRunPendingPR(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")

	pr, err := s.pendingPRs.ByRunID(r.Context(), runmode.LocalDefaultOrgID, runID)
	if err != nil {
		internalError(w, "pending-prs", err)
		return
	}
	if pr == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no pending PR for this run"})
		return
	}

	writeJSON(w, http.StatusOK, pendingPRToJSON(pr))
}

// handlePendingPRUpdate edits the human-visible title and body of a
// queued pending PR. Originals stay frozen via the COALESCE in
// UpdatePendingPRTitleBody so the human-feedback diff at submit time
// has a stable baseline.
func (s *Server) handlePendingPRUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req struct {
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}

	pr, err := s.pendingPRs.Get(r.Context(), runmode.LocalDefaultOrgID, id)
	if err != nil {
		internalError(w, "pending-prs", err)
		return
	}
	if pr == nil {
		notFound(w, "pending PR")
		return
	}

	title := pr.Title
	body := pr.Body
	if req.Title != nil {
		title = *req.Title
	}
	if req.Body != nil {
		body = *req.Body
	}
	// Trim before the empty check: GitHub rejects whitespace-only
	// titles ("Title is required") at submit time anyway, and silently
	// letting "   " through this PATCH means the user only finds out
	// after clicking Open PR. Fail fast and write the trimmed value
	// so we don't carry leading/trailing whitespace into the PR.
	title = strings.TrimSpace(title)
	if title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title cannot be empty or whitespace-only"})
		return
	}

	if err := s.pendingPRs.UpdateTitleBody(r.Context(), runmode.LocalDefaultOrgID, id, title, body); err != nil {
		if errors.Is(err, db.ErrPendingPRSubmitted) {
			// 409 Conflict so the overlay can show the user a clean
			// "submission in flight, your edit was dropped" message
			// instead of a green "saved" toast over a no-op.
			writeJSON(w, http.StatusConflict, map[string]string{"error": "this PR is already being submitted — your edit didn't apply"})
			return
		}
		internalError(w, "pending-prs", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handlePendingPRDiff returns the unified diff between the queued
// PR's base and head branches, computed against the bare clone we
// already maintain. The `git fetch origin <head>:<head>` first syncs
// the bare's local ref to whatever the agent pushed (the bare
// doesn't auto-update on push). Diff is capped at livePRDiffMaxBytes.
func (s *Server) handlePendingPRDiff(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	pr, err := s.pendingPRs.Get(r.Context(), runmode.LocalDefaultOrgID, id)
	if err != nil {
		internalError(w, "pending-prs", err)
		return
	}
	if pr == nil {
		notFound(w, "pending PR")
		return
	}

	diff, truncationNote, err := livePRDiff(r.Context(), pr.Owner, pr.Repo, pr.BaseBranch, pr.HeadBranch)
	if err != nil {
		// Server-side log with full row context so a 502 shows up
		// in console logs as something diagnosable instead of a
		// silent error. The pre-fix version returned the error in
		// the response body but never logged anything, which made
		// the "no errors in the backend" complaint accurate even
		// when the failure was right there in `err`.
		log.Printf("[pending-prs] diff failed for id=%s run=%s repo=%s/%s base=%s head=%s head_sha=%s: %v",
			pr.ID, pr.RunID, pr.Owner, pr.Repo, pr.BaseBranch, pr.HeadBranch, pr.HeadSHA, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "diff failed: " + err.Error()})
		return
	}

	// X-Diff-Truncated lets the frontend render a banner above the
	// file list when the cap kicked in. Without it parseDiff would
	// silently yield no files for a too-big-to-render diff and the
	// overlay would say "No diff available" — exactly the
	// confusing-when-it-matters case the cap is meant to handle.
	if truncationNote != "" {
		w.Header().Set("X-Diff-Truncated", truncationNote)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(diff))
}

// handlePendingPRSubmit is the user-clicked-Open-PR endpoint:
//
//  1. Concurrent-submit guard (MarkPendingPRSubmitted) — two browser
//     tabs both clicking can't both call CreatePR.
//  2. Build final body with agentmeta footer.
//  3. CreatePR on GitHub. On failure, release the guard so the user
//     can retry; do NOT delete the row.
//  4. On success, capture human verdict in run_memory.human_content,
//     delete the pending row, flip the run to completed, mark the
//     task done, broadcast the WS event.
func (s *Server) handlePendingPRSubmit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if s.ghClient == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub credentials not configured"})
		return
	}

	// Pointer so we can distinguish "client sent draft=false" from
	// "client didn't send draft." A missing field falls back to the
	// row's persisted value (the agent's queue-time --draft hint).
	var req struct {
		Draft *bool `json:"draft"`
	}
	if r.ContentLength > 0 {
		if !decodeJSON(w, r, &req, "") {
			return
		}
	}

	pr, err := s.pendingPRs.Get(r.Context(), runmode.LocalDefaultOrgID, id)
	if err != nil {
		internalError(w, "pending-prs", err)
		return
	}
	if pr == nil {
		notFound(w, "pending PR")
		return
	}

	// Concurrent-submit guard. Whichever caller wins this UPDATE goes
	// on to call GitHub; the loser sees 409 and can retry once the
	// winner finishes (which will release the guard on failure or
	// delete the row on success).
	if err := s.pendingPRs.MarkSubmitted(r.Context(), runmode.LocalDefaultOrgID, id); err != nil {
		if errors.Is(err, db.ErrPendingPRSubmitInFlight) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "another submit is in flight or has already completed"})
			return
		}
		internalError(w, "pending-prs", err)
		return
	}

	// Build the final body with footer using actual run cost data.
	finalBody := pr.Body + agentmeta.Build(s.agentRuns, pr.RunID, "PR")

	// Default draft to the row's persisted value (agent's hint). The
	// overlay's checkbox initializes from the same field, so a user
	// who clicks Open PR without touching the checkbox lands on the
	// agent's intent. A user who flipped the checkbox sends an
	// explicit `draft` in the request body.
	draft := pr.Draft
	if req.Draft != nil {
		draft = *req.Draft
	}
	number, htmlURL, err := s.ghClient.CreatePR(pr.Owner, pr.Repo, pr.HeadBranch, pr.BaseBranch, pr.Title, finalBody, draft)
	if err != nil {
		// Release the guard so the user can retry. Pending row stays
		// in place — they may want to edit title/body or push more
		// commits and retry without re-queueing.
		// Release the guard with a cancellation-detached context. The
		// adjacent CreatePR call already failed, but the user kept the
		// row queued — if the browser cancels the request after we get
		// here, r.Context() is dead and the guard never gets cleared,
		// leaving the row in a permanently "in flight" state that
		// requires a manual fix. Background lets the user retry.
		if clearErr := s.pendingPRs.ClearSubmitted(context.WithoutCancel(r.Context()), runmode.LocalDefaultOrgID, id); clearErr != nil {
			log.Printf("[pending-prs] failed to release submit guard for %s after CreatePR failure: %v", id, clearErr)
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error: " + err.Error()})
		return
	}

	// Capture the human's verdict (title/body diff vs agent draft)
	// while the originals are still in scope. SKY-205 parallel: the
	// next retry of this ticket should see what the human changed.
	if pr.RunID != "" {
		humanContent := FormatHumanFeedbackPR(pr, pr.Title, pr.Body)
		if err := s.taskMemory.UpdateRunMemoryHumanContent(r.Context(), runmode.LocalDefaultOrg, pr.RunID, humanContent); err != nil {
			log.Printf("[pending-prs] warning: failed to record human verdict for run %s: %v", pr.RunID, err)
		}
	}

	// Post-success cleanup uses a cancellation-detached context: the
	// GitHub PR is already open, so the user's "submit succeeded"
	// state has landed externally — bailing on r.Context() cancel
	// would leave the pending row + run/task in a half-cleaned state
	// (PR opened, queue still shows pending_approval). Matches the
	// reviews_handler.go SubmitReview pattern.
	cleanupCtx := context.WithoutCancel(r.Context())
	if err := s.pendingPRs.Delete(cleanupCtx, runmode.LocalDefaultOrgID, id); err != nil {
		log.Printf("[pending-prs] warning: failed to clean up pending PR %s after submit: %v", id, err)
	}

	if pr.RunID != "" {
		if _, err := s.db.Exec(`UPDATE runs SET status = 'completed' WHERE id = ? AND status = 'pending_approval'`, pr.RunID); err != nil {
			log.Printf("[pending-prs] warning: failed to update run %s status: %v", pr.RunID, err)
		}
		// Skip the blanket task-mark-done for chain steps; terminateChain
		// owns task closure so the chain_runs row finalizes first.
		chainRun, _, chainLookupErr := s.chains.GetRunForRun(r.Context(), runmode.LocalDefaultOrg, pr.RunID)
		if chainLookupErr != nil {
			// Don't blindly close the task — if this turns out to be a
			// chain step, closing it would race with terminateChain. Leave
			// the task open for human follow-up and skip the resume hook.
			log.Printf("[pending-prs] warning: chain lookup failed for run %s; skipping task closure: %v", pr.RunID, chainLookupErr)
		} else if chainRun == nil {
			if _, err := s.db.Exec(`UPDATE tasks SET status = 'done' WHERE id = (SELECT task_id FROM runs WHERE id = ?)`, pr.RunID); err != nil {
				log.Printf("[pending-prs] warning: failed to update task status for run %s: %v", pr.RunID, err)
			}
		}
		s.ws.Broadcast(websocket.Event{
			Type:  "agent_run_update",
			RunID: pr.RunID,
			Data:  map[string]string{"status": "completed"},
		})
		if chainRun != nil && s.spawner != nil {
			s.spawner.ResumeChainAfterApproval(pr.RunID)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"number":   number,
		"html_url": htmlURL,
	})
}

// pendingPRToJSON projects the domain row into the wire shape.
// Pulled out so both the by-id and by-run handlers stay symmetric.
func pendingPRToJSON(pr *domain.PendingPR) pendingPRJSON {
	out := pendingPRJSON{
		ID:         pr.ID,
		RunID:      pr.RunID,
		Owner:      pr.Owner,
		Repo:       pr.Repo,
		HeadBranch: pr.HeadBranch,
		HeadSHA:    pr.HeadSHA,
		BaseBranch: pr.BaseBranch,
		Title:      pr.Title,
		Body:       pr.Body,
		Draft:      pr.Draft,
		Locked:     pr.Locked,
	}
	if pr.SubmittedAt != nil {
		out.SubmittedAt = pr.SubmittedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return out
}
