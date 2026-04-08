package server

import (
	"log"
	"net/http"

	"github.com/sky-ai-eng/todo-tinder/internal/db"
	ghclient "github.com/sky-ai-eng/todo-tinder/internal/github"
	"github.com/sky-ai-eng/todo-tinder/pkg/websocket"
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if review == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "review not found"})
		return
	}

	comments, err := db.ListPendingReviewComments(s.db, reviewID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if review == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "review not found"})
		return
	}
	if review.ReviewEvent == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "review has not been submitted by the agent yet"})
		return
	}

	// Load comments (potentially edited by the user)
	comments, err := db.ListPendingReviewComments(s.db, reviewID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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

	// Submit to GitHub
	ghReviewID, actualEvent, err := s.ghClient.SubmitReview(
		review.Owner, review.Repo, review.PRNumber,
		review.CommitSHA, review.ReviewEvent, review.ReviewBody, ghComments,
	)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error: " + err.Error()})
		return
	}

	// Clean up local state
	if err := db.DeletePendingReview(s.db, reviewID); err != nil {
		log.Printf("[reviews] warning: failed to clean up review %s: %v", reviewID, err)
	}

	// If this review was associated with an agent run, update the run status
	if review.RunID != "" {
		if _, err := s.db.Exec(`UPDATE agent_runs SET status = 'completed' WHERE id = ? AND status = 'pending_approval'`, review.RunID); err != nil {
			log.Printf("[reviews] warning: failed to update run %s status: %v", review.RunID, err)
		}
		// Also mark the task as done
		if _, err := s.db.Exec(`UPDATE tasks SET status = 'done' WHERE id = (SELECT task_id FROM agent_runs WHERE id = ?)`, review.RunID); err != nil {
			log.Printf("[reviews] warning: failed to update task status for run %s: %v", review.RunID, err)
		}
		s.ws.Broadcast(websocket.Event{
			Type:  "agent_run_update",
			RunID: review.RunID,
			Data:  map[string]string{"status": "completed"},
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"github_review_id": ghReviewID,
		"event":            actualEvent,
		"comments_posted":  len(ghComments),
	})
}
