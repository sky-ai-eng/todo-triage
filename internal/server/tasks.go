package server

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// taskJSON is the API representation of a task. Maps entity-joined fields
// to the frontend's expected shape for backward compatibility.
type taskJSON struct {
	ID                  string   `json:"id"`
	EntityID            string   `json:"entity_id"`   // FK to entities.id — lets callers correlate tasks back to their entity
	Source              string   `json:"source"`      // from entity
	SourceID            string   `json:"source_id"`   // from entity
	SourceURL           string   `json:"source_url"`  // from entity
	Title               string   `json:"title"`       // from entity
	EntityKind          string   `json:"entity_kind"` // "pr" | "issue"
	EventType           string   `json:"event_type"`
	DedupKey            string   `json:"dedup_key,omitempty"`
	Severity            string   `json:"severity,omitempty"`
	RelevanceReason     string   `json:"relevance_reason,omitempty"`
	ScoringStatus       string   `json:"scoring_status"`
	CreatedAt           string   `json:"created_at"`
	Status              string   `json:"status"`
	PriorityScore       *float64 `json:"priority_score"`
	AutonomySuitability *float64 `json:"autonomy_suitability"`
	AISummary           string   `json:"ai_summary,omitempty"`
	PriorityReasoning   string   `json:"priority_reasoning,omitempty"`
	CloseReason         string   `json:"close_reason,omitempty"`
	// OpenSubtaskCount lets the UI flag a task whose Jira entity has open
	// subtasks — the "consider decomposing" signal (SKY-173). Zero for
	// GitHub tasks and Jira tickets without subtasks.
	OpenSubtaskCount int `json:"open_subtask_count"`
}

func taskToJSON(t domain.Task) taskJSON {
	return taskJSON{
		ID:                  t.ID,
		EntityID:            t.EntityID,
		Source:              t.EntitySource,
		SourceID:            t.EntitySourceID,
		SourceURL:           t.SourceURL,
		Title:               t.Title,
		EntityKind:          t.EntityKind,
		EventType:           t.EventType,
		DedupKey:            t.DedupKey,
		Severity:            t.Severity,
		RelevanceReason:     t.RelevanceReason,
		ScoringStatus:       t.ScoringStatus,
		CreatedAt:           t.CreatedAt.Format(time.RFC3339),
		Status:              t.Status,
		PriorityScore:       t.PriorityScore,
		AutonomySuitability: t.AutonomySuitability,
		AISummary:           t.AISummary,
		PriorityReasoning:   t.PriorityReasoning,
		CloseReason:         t.CloseReason,
		OpenSubtaskCount:    t.OpenSubtaskCount,
	}
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	tasks, err := db.QueuedTasks(s.db)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	result := make([]taskJSON, len(tasks))
	for i, t := range tasks {
		result[i] = taskToJSON(t)
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	var tasks []domain.Task
	var err error
	if status != "" {
		tasks, err = db.TasksByStatus(s.db, status)
	} else {
		tasks, err = db.QueuedTasks(s.db)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	result := make([]taskJSON, len(tasks))
	for i, t := range tasks {
		result[i] = taskToJSON(t)
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task, err := db.GetTask(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if task == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}
	writeJSON(w, http.StatusOK, taskToJSON(*task))
}

type swipeRequest struct {
	Action       string `json:"action"`
	HesitationMs int    `json:"hesitation_ms"`
	PromptID     string `json:"prompt_id,omitempty"`
}

func (s *Server) handleSwipe(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req swipeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	switch req.Action {
	case "claim", "dismiss", "snooze", "delegate":
		// valid
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid action: must be claim, dismiss, snooze, or delegate"})
		return
	}

	newStatus, err := db.RecordSwipe(s.db, id, req.Action, req.HesitationMs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Dismiss is a terminal state — if the user swipes away a task mid-run
	// (rare, but possible on a delegated card via the Board gesture rather
	// than the AgentCard cancel button), the run must stop. Mirrors the
	// inline-close and entity-close cascades: task state is authoritative;
	// runs follow.
	//
	// Two complementary paths here:
	//   - ActiveRunIDsForTask + spawner.Cancel covers in-flight runs
	//     (running, agent_starting, etc.) that still have a goroutine in
	//     s.cancels.
	//   - cleanupPendingApprovalRun covers pending_approval runs, which
	//     ActiveRunIDsForTask deliberately excludes (the agent process
	//     has exited, there's nothing to cancel — but the DB cleanup
	//     is still needed). SKY-206 closed the gap that left
	//     pending_reviews + a phantom run on a dismissed task.
	if req.Action == "dismiss" {
		s.cleanupPendingApprovalRun(id, discardOutcomeDismissed)
		if s.spawner != nil {
			ids, err := db.ActiveRunIDsForTask(s.db, id)
			if err != nil {
				log.Printf("[swipe] active-run lookup for task %s failed: %v", id, err)
			} else {
				for _, runID := range ids {
					if err := s.spawner.Cancel(runID); err != nil {
						log.Printf("[swipe] cancel run %s on dismiss of task %s: %v", runID, id, err)
					}
				}
			}
		}
	}

	response := map[string]any{"status": newStatus}

	// On claim: if Jira task, assign to self and transition to in-progress.
	// Claim guard: with multiple tasks per entity, a second claim on the same
	// Jira issue would re-assign + re-transition redundantly (and probably
	// error). Skip the transition when the ticket is already in ANY member of
	// the in-progress rule — if the user (or an earlier claim) moved it to
	// "In Review" while canonical is "In Progress", transitioning back to the
	// canonical would be a spurious status change that would confuse watchers.
	rule := s.jiraInProgressRule
	if req.Action == "claim" && s.jiraClient != nil && rule.Canonical != "" {
		task, err := db.GetTask(s.db, id)
		if err == nil && task != nil && task.EntitySource == "jira" {
			go func(issueKey string, rule config.JiraStatusRule) {
				state := s.jiraClient.GetClaimState(issueKey)

				needAssign := state == nil || !state.AssignedToSelf
				needTransition := state == nil || !rule.Contains(state.StatusName)

				if !needAssign && !needTransition {
					log.Printf("[jira] claim guard: %s already assigned to self and already in in-progress (%q), skipping", issueKey, state.StatusName)
					return
				}

				if needAssign {
					if err := s.jiraClient.AssignToSelf(issueKey); err != nil {
						log.Printf("[jira] failed to assign %s: %v", issueKey, err)
						return
					}
				}
				if needTransition {
					if err := s.jiraClient.TransitionTo(issueKey, rule.Canonical); err != nil {
						log.Printf("[jira] failed to transition %s to %q: %v", issueKey, rule.Canonical, err)
					}
				}
			}(task.EntitySourceID, rule)
		}
	}

	// Trigger delegation on swipe-up
	if req.Action == "delegate" && s.spawner != nil {
		task, err := db.GetTask(s.db, id)
		if err == nil && task != nil {
			runID, err := s.spawner.Delegate(*task, req.PromptID, "manual", "")
			if err != nil {
				response["delegate_error"] = err.Error()
			} else {
				response["run_id"] = runID
			}
		}
	}

	writeJSON(w, http.StatusOK, response)
}

type snoozeRequest struct {
	Until        string `json:"until"`
	HesitationMs int    `json:"hesitation_ms"`
}

func (s *Server) handleSnooze(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req snoozeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	until, err := parseSnoozeUntil(req.Until)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid snooze duration: " + err.Error()})
		return
	}

	if err := db.SnoozeTask(s.db, id, until, req.HesitationMs); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "snoozed", "until": until.Format(time.RFC3339)})
}

// handleUndo backs the Cards swipe-toast UX: the user just swiped
// claim/dismiss/delegate/snooze, sees the 5s "Undo" toast (or hits
// Cmd-Z), and we reverse the swipe. This endpoint is specifically
// for undoing a discrete user gesture — it records a swipe_events
// row tagged 'undo' for the swipe analytics, then runs the same
// requeue cleanup that /requeue does.
//
// State-driven requeue (Board's drag-to-Queue, SKY-207's "Return
// to queue" button) lives at /requeue and skips the swipe row.
// Same finalizer, same observable outcome — different audit shape.
func (s *Server) handleUndo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// GetTask up front does double duty: existence check for the
	// 404 response AND loads the row needed for finalizeRequeue's
	// Jira reversal context. Without the explicit nil check
	// UndoLastSwipe would still fail on the swipe_events FK, but
	// we'd surface the SQLite error string as a 500 — leaking
	// implementation detail and confusing legitimate 404 callers.
	task, err := db.GetTask(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if task == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}

	if err := db.UndoLastSwipe(s.db, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	s.finalizeRequeue(id, task)

	writeJSON(w, http.StatusOK, map[string]string{"status": "queued"})
}

// handleRequeue is the state-driven counterpart to handleUndo: same
// task-back-to-queue outcome, no swipe_events row. Used by Board's
// drag-to-Queue gesture and (once SKY-207 lands) the AgentCard's
// "Return to queue" button on pending_approval runs. Both of those
// are deliberate state changes, not "reverse my last swipe," so
// audit-logging them as undo events would muddy the swipe-UX
// analytics.
//
// Belt-and-suspenders existence check: GetTask up front catches the
// common bogus-id case and returns 404 with a clean error body;
// RequeueTask's ok-bool catches the race where the task gets
// deleted between the GetTask and the UPDATE. Without the second
// check, that race would surface as a misleading 200/queued
// response for an id that no longer exists.
func (s *Server) handleRequeue(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	task, err := db.GetTask(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if task == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}

	ok, err := db.RequeueTask(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}

	s.finalizeRequeue(id, task)

	writeJSON(w, http.StatusOK, map[string]string{"status": "queued"})
}

// discardOutcome describes how the task ended up after the user
// rejected the agent's prepared review. The cleanup path is
// identical for both, but the human_content note baked into
// run_memory differs — the next agent reading prior memory needs
// to know whether the human re-queued the task (still on the
// docket) or dismissed it outright (the entity is done with). The
// distinction is the load-bearing signal in the post-run memory.
type discardOutcome int

const (
	discardOutcomeRequeued discardOutcome = iota
	discardOutcomeDismissed
)

// finalizeRequeue runs the side-effect cleanup that both /undo and
// /requeue need after the task status flips back to queued:
//
//   - pending_approval cleanup: if the task had a delegated agent run
//     in pending_approval (review prepared, awaiting human submit),
//     write the discard verdict to run_memory.human_content, delete
//     the pending_reviews row, mark the run cancelled, and broadcast
//     the run-status change to the websocket. SKY-206 — closes the
//     bug where a discarded review left a stale pending_reviews row
//     and a phantom pending_approval run on a now-queued task.
//
//   - Jira reversal: if the task is Jira-backed and we have a
//     SourceStatus snapshot (recorded at claim time), unassign and
//     transition back. Guarded against external mutations: skip if
//     someone else now owns the ticket, or if the ticket has
//     progressed out of the in-progress rule entirely (done, back to
//     pickup, etc.).
//
// Both halves are best-effort and logged-not-failed: the task is
// already queued by the time we get here; failing the response would
// confuse callers about what actually changed.
//
// taskID is taken separately from task because the pending_approval
// cleanup only needs the id — running it under a nil-task short-
// circuit (e.g. when db.GetTask transiently fails or the task row was
// deleted concurrently) would silently strand the very state this
// helper is meant to clean up. Jira reversal needs the loaded row so
// it nil-guards internally.
func (s *Server) finalizeRequeue(taskID string, task *domain.Task) {
	s.cleanupPendingApprovalRun(taskID, discardOutcomeRequeued)
	s.revertJiraStateIfApplicable(task)
}

// cleanupPendingApprovalRun handles the SKY-206 case: the user
// returned a task to the queue (or dismissed it) while it had a
// pending_approval agent run — i.e. the agent prepared a PR review
// and the user threw it away rather than submitting. The agent
// process has long since exited (pending_approval is reached after
// the spawner's runAgent defer ran), so there's nothing to cancel
// at the process level — this is purely a DB cleanup: write the
// discard outcome to human_content, delete the pending_reviews +
// comments, flip the run row to cancelled with a discriminating
// stop_reason.
//
// outcome shapes the human_content note baked into run_memory so
// the next agent reading memory can distinguish "still on the
// docket, the human just didn't like this verdict" (requeued) from
// "the entity is done with — the human chose to walk away from it
// entirely" (dismissed). Same on-disk DB cleanup either way.
//
// Run-status broadcast lets the AgentCard collapse and the
// requeued/dismissed TaskCard reflect the new state without a
// manual refetch.
//
// All failures here are logged, not fatal: the calling handler has
// already flipped the task to its new state and the response should
// reflect that. Idempotent — a repeat call against an already-
// cancelled run finds no pending_approval row (the lookup filters on
// status='pending_approval') and exits silently.
func (s *Server) cleanupPendingApprovalRun(taskID string, outcome discardOutcome) {
	runID, err := db.PendingApprovalRunIDForTask(s.db, taskID)
	if err != nil {
		log.Printf("[review-discard] pending_approval lookup for task %s failed: %v", taskID, err)
		return
	}
	if runID == "" {
		return
	}

	// Write the discard outcome to run_memory.human_content BEFORE
	// the row teardown. The next agent reading memory on this
	// entity should see the human's verdict as authoritative —
	// alongside the existing agent_content (the agent's self-
	// report) — so it can recalibrate. Format mirrors the SKY-205
	// submit-time block so the parsing contract is uniform.
	humanContent := buildDiscardHumanContent(outcome)
	if err := db.UpdateRunMemoryHumanContent(s.db, runID, humanContent); err != nil {
		log.Printf("[review-discard] human_content write for run %s failed: %v", runID, err)
	}

	// Tear down the pending review by run_id directly. Older shape
	// did a separate PendingReviewByRunID + DeletePendingReview
	// two-step, which left the review row stranded if the lookup
	// failed transiently — exactly the stale-state bug this whole
	// path is meant to fix. The DELETE-by-run-id helper is
	// transactional across comments + review and is a no-op when
	// no review exists.
	//
	// On delete failure (transient SQLite lock, etc.) bail BEFORE
	// MarkAgentRunDiscarded. The flip off status='pending_approval'
	// is the cleanup's only hand-off back to the entry-point
	// query: PendingApprovalRunIDForTask filters on it, so once the
	// run is cancelled no subsequent user action can rediscover
	// this run for retry. Holding the run in pending_approval when
	// the delete fails keeps the next /undo, /requeue, or dismiss
	// on this task able to re-enter here and retry the delete +
	// mark together. UpdateRunMemoryHumanContent above is
	// idempotent on re-entry (UPDATE overwrites with the same or
	// refined verdict).
	if err := db.DeletePendingReviewByRunID(s.db, runID); err != nil {
		log.Printf("[review-discard] DeletePendingReviewByRunID for run %s failed (run held in pending_approval for retry): %v", runID, err)
		return
	}

	// Flip the run row terminal. ok=false here means the row was
	// already cancelled by a concurrent path (idempotent re-call,
	// rare race) — skip the broadcast in that case so we don't
	// double-fire.
	ok, err := db.MarkAgentRunDiscarded(s.db, runID, "review_discarded_by_user")
	if err != nil {
		log.Printf("[review-discard] MarkAgentRunDiscarded %s failed: %v", runID, err)
		return
	}
	if !ok {
		return
	}

	s.ws.Broadcast(websocket.Event{
		Type:  "agent_run_update",
		RunID: runID,
		Data:  map[string]string{"status": "cancelled"},
	})
}

// buildDiscardHumanContent renders the post-run human verdict
// recorded when the user rejects an agent-prepared review. The two
// shapes — requeued vs. dismissed — give the next agent on this
// entity different recalibration signals: requeued says "try
// again, but not like that," dismissed says "this entity wasn't
// worth pursuing."
func buildDiscardHumanContent(outcome discardOutcome) string {
	switch outcome {
	case discardOutcomeDismissed:
		return "**Outcome:** Human discarded the prepared review and dismissed the task entirely.\n" +
			"**Implication:** The verdict you proposed was not accepted, and the human chose to walk away from this entity rather than re-queue it. Future runs on similar entities should reconsider whether the situation warrants action at all."
	default: // discardOutcomeRequeued
		return "**Outcome:** Human discarded the prepared review without submitting it; task returned to the triage queue.\n" +
			"**Implication:** The verdict you proposed was not accepted. Reconsider whether this entity warrants any review at all, or whether a different framing is needed."
	}
}

// revertJiraStateIfApplicable was the body of handleUndo's Jira
// reversal block. Factored so /requeue picks up the same behavior —
// dragging a claimed Jira-backed task back to Queue should unassign
// and transition the ticket the same way Cmd-Z does. The guards
// against external mutations (someone else claimed it, status has
// progressed out of the in-progress rule) apply equally to both
// entry points.
func (s *Server) revertJiraStateIfApplicable(task *domain.Task) {
	if task == nil || task.EntitySource != "jira" || task.SourceStatus == "" || s.jiraClient == nil {
		return
	}
	go func(issueKey, originalStatus string) {
		state := s.jiraClient.GetClaimState(issueKey)

		// Three assignee cases:
		//   - assigned to someone else -> skip undo entirely (manual reassignment)
		//   - unassigned -> skip Unassign (already unassigned), still transition
		//   - assigned to self -> proceed normally (unassign + transition)
		if state != nil && !state.AssignedToSelf && !state.Unassigned {
			log.Printf("[jira] requeue guard: %s reassigned to someone else, skipping", issueKey)
			return
		}
		// Skip if the ticket has moved out of the in-progress rule
		// entirely — that means someone progressed it (to done, back to
		// pickup, etc.) and we shouldn't yank it back. Membership rather
		// than strict-canonical match, because a user moving Claim →
		// "In Review" is still "working on it on my plate" and the
		// requeue should still unwind to the original status.
		if state != nil && len(s.jiraInProgressRule.Members) > 0 && !s.jiraInProgressRule.Contains(state.StatusName) {
			log.Printf("[jira] requeue guard: %s status is %q (not in in-progress members %v), skipping", issueKey, state.StatusName, s.jiraInProgressRule.Members)
			return
		}

		if state == nil || state.AssignedToSelf {
			if err := s.jiraClient.Unassign(issueKey); err != nil {
				log.Printf("[jira] failed to unassign %s on requeue: %v", issueKey, err)
			}
		}
		if err := s.jiraClient.TransitionTo(issueKey, originalStatus); err != nil {
			log.Printf("[jira] failed to transition %s back to %q on requeue: %v", issueKey, originalStatus, err)
		}
	}(task.EntitySourceID, task.SourceStatus)
}

func parseSnoozeUntil(s string) (time.Time, error) {
	now := time.Now()
	switch s {
	case "1h":
		return now.Add(1 * time.Hour), nil
	case "2h":
		return now.Add(2 * time.Hour), nil
	case "4h":
		return now.Add(4 * time.Hour), nil
	case "tomorrow":
		tomorrow := now.AddDate(0, 0, 1)
		return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 9, 0, 0, 0, tomorrow.Location()), nil
	default:
		return time.Parse(time.RFC3339, s)
	}
}
