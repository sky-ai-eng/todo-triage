package server

import (
	"log"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/agentmeta"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

type pendingReviewJSON struct {
	ID          string                     `json:"id"`
	PRNumber    int                        `json:"pr_number"`
	Owner       string                     `json:"owner"`
	Repo        string                     `json:"repo"`
	CommitSHA   string                     `json:"commit_sha"`
	RunID       string                     `json:"run_id,omitempty"`
	ReviewBody  string                     `json:"review_body"`
	ReviewEvent string                     `json:"review_event"`
	Comments    []pendingReviewCommentJSON `json:"comments"`
}

type pendingReviewCommentJSON struct {
	ID        string `json:"id"`
	ReviewID  string `json:"review_id"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	StartLine *int   `json:"start_line,omitempty"`
	Body      string `json:"body"`
}

// handleReviewGet returns a pending review and its comments.
func (s *Server) handleReviewGet(w http.ResponseWriter, r *http.Request) {
	reviewID := r.PathValue("id")

	review, err := db.GetPendingReview(s.db, reviewID)
	if err != nil {
		internalError(w, "reviews", err)
		return
	}
	if review == nil {
		notFound(w, "review")
		return
	}

	comments, err := db.ListPendingReviewComments(s.db, reviewID)
	if err != nil {
		internalError(w, "reviews", err)
		return
	}

	result := pendingReviewJSON{
		ID:          review.ID,
		PRNumber:    review.PRNumber,
		Owner:       review.Owner,
		Repo:        review.Repo,
		CommitSHA:   review.CommitSHA,
		RunID:       review.RunID,
		ReviewBody:  review.ReviewBody,
		ReviewEvent: review.ReviewEvent,
		Comments:    make([]pendingReviewCommentJSON, len(comments)),
	}
	for i, c := range comments {
		result.Comments[i] = pendingReviewCommentJSON{
			ID:        c.ID,
			ReviewID:  c.ReviewID,
			Path:      c.Path,
			Line:      c.Line,
			StartLine: c.StartLine,
			Body:      c.Body,
		}
	}

	writeJSON(w, http.StatusOK, result)
}

// handleReviewSubmit posts a pending review to GitHub, then cleans up local state.
func (s *Server) handleReviewSubmit(w http.ResponseWriter, r *http.Request) {
	reviewID := r.PathValue("id")

	if s.ghClient == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub credentials not configured"})
		return
	}

	review, err := db.GetPendingReview(s.db, reviewID)
	if err != nil {
		internalError(w, "reviews", err)
		return
	}
	if review == nil {
		notFound(w, "review")
		return
	}
	if review.ReviewEvent == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "review has not been submitted by the agent yet"})
		return
	}

	// Load comments (potentially edited by the user)
	comments, err := db.ListPendingReviewComments(s.db, reviewID)
	if err != nil {
		internalError(w, "reviews", err)
		return
	}

	ghComments := make([]ghclient.SubmitReviewComment, len(comments))
	for i, c := range comments {
		ghComments[i] = ghclient.SubmitReviewComment{
			Path:      c.Path,
			Line:      c.Line,
			StartLine: c.StartLine,
			Body:      c.Body,
		}
	}

	// Build the final review body with header + footer using actual run data
	body := review.ReviewBody + agentmeta.Build(s.agentRuns, review.RunID, "review")

	// Submit to GitHub
	ghReviewID, actualEvent, err := s.ghClient.SubmitReview(
		review.Owner, review.Repo, review.PRNumber,
		review.CommitSHA, review.ReviewEvent, body, ghComments,
	)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error: " + err.Error()})
		return
	}

	// Persist the human's verdict to run_memory.human_content while
	// the originals are still in scope (DeletePendingReview below
	// removes them). SKY-205. Skipped for non-agent reviews
	// (review.RunID empty — standalone CLI path) since there's no
	// run_memory row to attach to. Logged-not-failed: the GitHub
	// submit already succeeded, so a memory-write failure shouldn't
	// surface as a 5xx and confuse the user about what landed.
	if review.RunID != "" {
		humanContent := FormatHumanFeedback(buildHumanFeedbackInput(review, comments, actualEvent))
		if err := db.UpdateRunMemoryHumanContent(s.db, review.RunID, humanContent); err != nil {
			log.Printf("[reviews] warning: failed to record human verdict for run %s: %v", review.RunID, err)
		}
	}

	// Clean up local state
	if err := db.DeletePendingReview(s.db, reviewID); err != nil {
		log.Printf("[reviews] warning: failed to clean up review %s: %v", reviewID, err)
	}

	// If this review was associated with an agent run, update the run status
	if review.RunID != "" {
		if _, err := s.db.Exec(`UPDATE runs SET status = 'completed' WHERE id = ? AND status = 'pending_approval'`, review.RunID); err != nil {
			log.Printf("[reviews] warning: failed to update run %s status: %v", review.RunID, err)
		}
		// Mark the task as done — except for chain steps, where the chain
		// orchestrator owns task closure (single closer guarantees the
		// chain_runs row terminates first, so the UI never shows a "done"
		// task with a still-running chain).
		chainRun, _, chainLookupErr := s.chains.GetRunForRun(r.Context(), runmode.LocalDefaultOrg, review.RunID)
		if chainLookupErr != nil {
			// Don't blindly close the task — if this turns out to be a
			// chain step, closing it would race with terminateChain. Leave
			// the task open for human follow-up and skip the resume hook.
			log.Printf("[reviews] warning: chain lookup failed for run %s; skipping task closure: %v", review.RunID, chainLookupErr)
		} else if chainRun == nil {
			if _, err := s.db.Exec(`UPDATE tasks SET status = 'done' WHERE id = (SELECT task_id FROM runs WHERE id = ?)`, review.RunID); err != nil {
				log.Printf("[reviews] warning: failed to update task status for run %s: %v", review.RunID, err)
			}
		}
		s.ws.Broadcast(websocket.Event{
			Type:  "agent_run_update",
			RunID: review.RunID,
			Data:  map[string]string{"status": "completed"},
		})
		if chainRun != nil && s.spawner != nil {
			s.spawner.ResumeChainAfterApproval(review.RunID)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"github_review_id": ghReviewID,
		"event":            actualEvent,
		"comments_posted":  len(ghComments),
	})
}

// handleRunReview looks up the pending review associated with an agent run.
func (s *Server) handleRunReview(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")

	review, err := db.PendingReviewByRunID(s.db, runID)
	if err != nil {
		internalError(w, "reviews", err)
		return
	}
	if review == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no pending review for this run"})
		return
	}

	// Delegate to the full review GET which includes comments
	comments, err := db.ListPendingReviewComments(s.db, review.ID)
	if err != nil {
		internalError(w, "reviews", err)
		return
	}

	result := pendingReviewJSON{
		ID:          review.ID,
		PRNumber:    review.PRNumber,
		Owner:       review.Owner,
		Repo:        review.Repo,
		CommitSHA:   review.CommitSHA,
		RunID:       review.RunID,
		ReviewBody:  review.ReviewBody,
		ReviewEvent: review.ReviewEvent,
		Comments:    make([]pendingReviewCommentJSON, len(comments)),
	}
	for i, c := range comments {
		result.Comments[i] = pendingReviewCommentJSON{
			ID:        c.ID,
			ReviewID:  c.ReviewID,
			Path:      c.Path,
			Line:      c.Line,
			StartLine: c.StartLine,
			Body:      c.Body,
		}
	}

	writeJSON(w, http.StatusOK, result)
}

// handleReviewUpdate updates the review body and/or event type.
func (s *Server) handleReviewUpdate(w http.ResponseWriter, r *http.Request) {
	reviewID := r.PathValue("id")

	var req struct {
		ReviewBody  *string `json:"review_body"`
		ReviewEvent *string `json:"review_event"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}

	review, err := db.GetPendingReview(s.db, reviewID)
	if err != nil {
		internalError(w, "reviews", err)
		return
	}
	if review == nil {
		notFound(w, "review")
		return
	}

	body := review.ReviewBody
	event := review.ReviewEvent
	if req.ReviewBody != nil {
		body = *req.ReviewBody
	}
	if req.ReviewEvent != nil {
		event = *req.ReviewEvent
	}

	if err := db.SetPendingReviewSubmission(s.db, reviewID, body, event); err != nil {
		internalError(w, "reviews", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleReviewCommentUpdate edits the body of a pending review comment.
func (s *Server) handleReviewCommentUpdate(w http.ResponseWriter, r *http.Request) {
	commentID := r.PathValue("commentId")

	var req struct {
		Body string `json:"body"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is required"})
		return
	}

	if err := db.UpdatePendingReviewComment(s.db, commentID, req.Body); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleReviewCommentDelete removes a pending review comment.
func (s *Server) handleReviewCommentDelete(w http.ResponseWriter, r *http.Request) {
	commentID := r.PathValue("commentId")

	if err := db.DeletePendingReviewComment(s.db, commentID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleReviewDiff proxies the PR diff from GitHub for the review's PR.
func (s *Server) handleReviewDiff(w http.ResponseWriter, r *http.Request) {
	reviewID := r.PathValue("id")

	if s.ghClient == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub credentials not configured"})
		return
	}

	review, err := db.GetPendingReview(s.db, reviewID)
	if err != nil {
		internalError(w, "reviews", err)
		return
	}
	if review == nil {
		notFound(w, "review")
		return
	}

	file := r.URL.Query().Get("file")
	diff, err := s.ghClient.GetPRDiff(review.Owner, review.Repo, review.PRNumber, file)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error: " + err.Error()})
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(diff))
}
