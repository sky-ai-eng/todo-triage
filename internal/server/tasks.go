package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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
	// SnoozeUntil — populated when the task is in a snoozed state.
	// SKY-261 B+: snooze is orthogonal to claim, so a claimed task
	// can be snoozed (e.g., "bot owns this but wait until Tuesday").
	// The Board renders snoozed cards with a "wakes at X" badge so
	// the user sees them in their owner's lane (You / Agent) without
	// needing a separate Snoozed column.
	SnoozeUntil string `json:"snooze_until,omitempty"`
	// OpenSubtaskCount lets the UI flag a task whose Jira entity has open
	// subtasks — the "consider decomposing" signal (SKY-173). Zero for
	// GitHub tasks and Jira tickets without subtasks.
	OpenSubtaskCount int `json:"open_subtask_count"`
}

func taskToJSON(t domain.Task) taskJSON {
	snoozeUntil := ""
	if t.SnoozeUntil != nil {
		snoozeUntil = t.SnoozeUntil.Format(time.RFC3339)
	}
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
		SnoozeUntil:         snoozeUntil,
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
	case "claim", "dismiss", "snooze", "delegate", "complete":
		// valid
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid action: must be claim, dismiss, snooze, delegate, or complete"})
		return
	}

	newStatus, err := s.swipes.RecordSwipe(r.Context(), runmode.LocalDefaultOrg, id, req.Action, req.HesitationMs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// SKY-261 D-Claims: stamp the claim column matching the swipe action.
	//   claim    → user takes responsibility; clears any agent claim
	//              (user-takes-back path; the per-Board "claim a queued
	//              task" gesture lands here too).
	//   delegate → bot takes responsibility; clears any user claim
	//              (paired with the status flip + run spawn just below).
	// dismiss / snooze / complete leave the claim alone: their state
	// transition doesn't change "who's responsible," it changes
	// lifecycle (status). Sticky claims past close preserve the
	// audit shape: status='done' + claim populated = "this person
	// or bot was on it when it finished."
	//
	// Errors here are surfaced as 500 so the client retries rather than
	// silently diverging — RecordSwipe already updated status, so a
	// silent claim-stamp failure would leave status+claim out of sync.
	// Both UPDATEs are idempotent on retry. Full atomicity (single
	// transaction across status + claim) would require extending
	// SwipeStore.RecordSwipe to accept claim params; deferred.
	switch req.Action {
	case "claim":
		// Race-safe claim: branch on the task's current claim state
		// and use the guarded helpers so concurrent claimants can't
		// steal from each other. Three legitimate transitions land
		// here:
		//   - unclaimed → user-claim (the common case; uses
		//     ClaimQueuedTaskForUser's anti-steal SQL guard).
		//   - bot-claim → user-claim ("I'll handle this" takeover;
		//     uses TakeoverClaimFromAgent's guard so a concurrent
		//     user takeover or requeue doesn't get stolen).
		//   - same user already claims → idempotent no-op.
		// A different user owning the task is a 409 — refuse rather
		// than overwrite. The cleanupPendingApprovalRun + spawner.Cancel
		// teardown below still runs in all success branches.
		task, lerr := db.GetTask(s.db, id)
		if lerr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": lerr.Error()})
			return
		}
		if task == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		userID := runmode.LocalDefaultUserID
		claimChanged := false
		switch {
		case task.ClaimedByUserID == userID:
			// Idempotent: same user already owns it.
		case task.ClaimedByUserID != "":
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "task is already claimed by another user",
			})
			return
		case task.ClaimedByAgentID != "":
			ok, err := db.TakeoverClaimFromAgent(s.db, id, userID)
			if err != nil {
				log.Printf("[swipe] takeover claim flip failed on task %s: %v", id, err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "claim stamp failed: " + err.Error()})
				return
			}
			if !ok {
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": "claim race lost; refetch task and retry",
				})
				return
			}
			claimChanged = true
		default:
			ok, err := db.ClaimQueuedTaskForUser(s.db, id, userID)
			if err != nil {
				log.Printf("[swipe] user claim stamp failed on task %s: %v", id, err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "claim stamp failed: " + err.Error()})
				return
			}
			if !ok {
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": "claim race lost; refetch task and retry",
				})
				return
			}
			claimChanged = true
		}
		// SKY-261 B+: broadcast on the claim axis (not the status
		// axis). The Board listens for task_claimed to re-render the
		// per-claim lanes; status didn't change so task_updated would
		// be misleading. Only broadcast when the claim actually
		// changed — the same-user idempotent path is a no-op and
		// shouldn't churn listeners.
		if claimChanged {
			s.ws.Broadcast(websocket.Event{
				Type: "task_claimed",
				Data: map[string]any{
					"task_id":             id,
					"claimed_by_agent_id": "",
					"claimed_by_user_id":  userID,
				},
			})
		}
	case "delegate":
		if s.agents == nil {
			log.Printf("[swipe] agent claim skipped on task %s: AgentStore not configured", id)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "delegate failed: agent store not configured"})
			return
		}
		a, aerr := s.agents.GetForOrg(r.Context(), runmode.LocalDefaultOrg)
		if aerr != nil {
			log.Printf("[swipe] agent lookup failed on task %s delegate: %v", id, aerr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "delegate failed: agent lookup: " + aerr.Error()})
			return
		}
		if a == nil {
			log.Printf("[swipe] delegate aborted on task %s: no agent bootstrapped yet", id)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "delegate failed: no agent bootstrapped — set up the bot first"})
			return
		}
		// HandoffAgentClaim covers all three legitimate user→bot
		// transitions:
		//   - unclaimed → bot (queue → Agent drag, swipe-up on an
		//     unclaimed card)
		//   - user-claimed-by-me → bot (the SKY-133 "You → Agent"
		//     drag; user is explicitly transferring their own claim
		//     to the bot)
		//   - bot-already-claimed → no-op (idempotent)
		// And one refusal: a DIFFERENT user owns it — the bot
		// shouldn't steal what a teammate took on.
		result, err := db.HandoffAgentClaim(s.db, id, a.ID, runmode.LocalDefaultUserID)
		if err != nil {
			log.Printf("[swipe] failed to stamp agent claim on task %s: %v", id, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "claim stamp failed: " + err.Error()})
			return
		}
		switch result {
		case db.HandoffChanged:
			s.ws.Broadcast(websocket.Event{
				Type: "task_claimed",
				Data: map[string]any{
					"task_id":             id,
					"claimed_by_agent_id": a.ID,
					"claimed_by_user_id":  "",
				},
			})
		case db.HandoffNoOp:
			// Bot already owns it — fall through to the run-spawn
			// step below without broadcasting.
		case db.HandoffRefused:
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "task is claimed by another user; refusing to steal",
			})
			return
		}
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
	// Any user gesture that takes a task off the agent's hands —
	// dismiss, complete, or claim — must tear down a pending_approval
	// review if one exists and cancel any in-flight run. Otherwise a
	// race in the frontend (agentRuns map briefly stale during a
	// fetchTasks refresh) lets the user issue /swipe claim against a
	// pending_approval card without going through /requeue first,
	// stranding the prepared review row and the phantom
	// pending_approval run that SKY-206 closed.
	//
	// Backend-authoritative is the right shape here: the swipe
	// handler already loaded the task; cleanupPendingApprovalRun is
	// idempotent (filters on status='pending_approval') and a no-op
	// when no review exists. The discard memory note differs per
	// action so the next agent reading run_memory can tell apart
	// "human walked away from this entity" (dismiss) from "human
	// resolved it themselves" (complete) from "human took over and
	// will handle it manually" (claim) — three distinct
	// recalibration signals.
	if req.Action == "dismiss" || req.Action == "complete" || req.Action == "claim" {
		outcome := discardOutcomeDismissed
		switch req.Action {
		case "complete":
			outcome = discardOutcomeCompleted
		case "claim":
			outcome = discardOutcomeClaimed
		}
		s.cleanupPendingApprovalRun(id, outcome)
		if s.spawner != nil {
			ids, err := db.ActiveRunIDsForTask(s.db, id)
			if err != nil {
				log.Printf("[swipe] active-run lookup for task %s failed: %v", id, err)
			} else {
				for _, runID := range ids {
					if err := s.spawner.Cancel(runID); err != nil {
						log.Printf("[swipe] cancel run %s on %s of task %s: %v", runID, req.Action, id, err)
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

	ok, err := s.swipes.SnoozeTask(r.Context(), runmode.LocalDefaultOrg, id, until, req.HesitationMs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		// SKY-261 B+: snooze is queue-only ("snoozed ↔ both claim
		// cols NULL"). The store's atomic UPDATE refused because
		// the task is currently claimed by a user or the bot.
		// Requeue first (releases the claim) then snooze.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "can't snooze a claimed task; requeue or complete it first",
		})
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

	if err := s.swipes.UndoLastSwipe(r.Context(), runmode.LocalDefaultOrg, id); err != nil {
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

	ok, err := s.swipes.RequeueTask(r.Context(), runmode.LocalDefaultOrg, id)
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
// rejected the agent's prepared review. The DB cleanup path is the
// same across all four values, but the human_content note baked
// into run_memory differs — the next agent reading prior memory
// needs to know whether the human:
//
//   - re-queued the task (still on the docket; verdict was wrong),
//   - dismissed it outright (the entity isn't worth pursuing),
//   - marked it complete (the entity was resolved, but not via the
//     agent's prepared verdict),
//   - or claimed it themselves (the human took over and will handle
//     the entity manually rather than re-attempting agent work).
//
// The distinction is the load-bearing signal in post-run memory:
// each shape implies a different recalibration for future runs.
type discardOutcome int

const (
	discardOutcomeRequeued discardOutcome = iota
	discardOutcomeDismissed
	// discardOutcomeCompleted: user marked the task done from a
	// terminal-state AgentCard (failed, cancelled, taken_over) by
	// dragging it to the Done column. The agent's prepared review,
	// if any, is being discarded — the user is signalling "the work
	// is finished" without applying the agent's verdict to GitHub.
	discardOutcomeCompleted
	// discardOutcomeClaimed: user claimed the task while it had a
	// pending_approval run (Board's drag-to-You from Agent/Done, or
	// the Cards swipe-right against a delegated task). The agent's
	// prepared review is being thrown away in favor of the human
	// handling the entity themselves. This case exists primarily
	// to close the SKY-206 race where a stale frontend agentRuns
	// map could let /swipe claim slip past without /requeue's
	// cleanup; the swipe handler now runs the cleanup on every
	// claim regardless of frontend state.
	discardOutcomeClaimed
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
		log.Printf("[approval-discard] pending_approval lookup for task %s failed: %v", taskID, err)
		return
	}
	if runID == "" {
		return
	}

	// Detect which side-table held the row BEFORE deleting, so the
	// stop_reason and human_content can name the right kind.
	// Without this, a discarded PR ends up tagged "review_discarded_
	// by_user" — confusing in the UI and breaks any downstream
	// logic keyed on stop_reason that needs to tell the two apart.
	kind := "review"
	if pr, err := db.PendingPRByRunID(s.db, runID); err != nil {
		log.Printf("[approval-discard] PendingPRByRunID lookup for run %s failed: %v", runID, err)
	} else if pr != nil {
		kind = "pr"
	}

	// Write the discard outcome to run_memory.human_content BEFORE
	// the row teardown. The next agent reading memory on this
	// entity should see the human's verdict as authoritative —
	// alongside the existing agent_content (the agent's self-
	// report) — so it can recalibrate. Format mirrors the SKY-205
	// submit-time block so the parsing contract is uniform.
	humanContent := buildDiscardHumanContent(outcome, kind)
	if err := db.UpdateRunMemoryHumanContent(s.db, runID, humanContent); err != nil {
		log.Printf("[approval-discard] human_content write for run %s failed: %v", runID, err)
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
		log.Printf("[approval-discard] DeletePendingReviewByRunID for run %s failed (run held in pending_approval for retry): %v", runID, err)
		return
	}
	// Same hold-for-retry semantics for the pending-PR side table.
	// A run can have at most one pending entry across the two tables
	// (the spawner flips on either), but cleanupPendingApprovalRun
	// runs against the run id without first determining which kind —
	// so we attempt both deletes. Both are idempotent no-ops when no
	// row exists, so calling unconditionally is safe.
	if err := db.DeletePendingPRByRunID(s.db, runID); err != nil {
		log.Printf("[approval-discard] DeletePendingPRByRunID for run %s failed (run held in pending_approval for retry): %v", runID, err)
		return
	}

	// Discriminating stop_reason: review vs PR. Existing review
	// callers / tests still see "review_discarded_by_user"; PR
	// discards become a distinct value so downstream queries can
	// tell them apart.
	stopReason := "review_discarded_by_user"
	if kind == "pr" {
		stopReason = "pr_discarded_by_user"
	}

	// Flip the run row terminal. ok=false here means the row was
	// already cancelled by a concurrent path (idempotent re-call,
	// rare race) — skip the broadcast in that case so we don't
	// double-fire.
	ok, err := db.MarkAgentRunDiscarded(s.db, runID, stopReason)
	if err != nil {
		log.Printf("[approval-discard] MarkAgentRunDiscarded %s failed: %v", runID, err)
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
// recorded when the user rejects an agent-prepared approval. The
// four shapes — requeued, dismissed, completed, claimed — give
// the next agent on this entity different recalibration signals:
//
//   - requeued: "try again, but not like that" (verdict was wrong;
//     the task is back in the queue).
//   - dismissed: "this entity wasn't worth pursuing" (the human
//     walked away from the entity entirely).
//   - completed: "you reached the right ballpark but I resolved
//     this myself" (the human accepted the task as done without
//     applying the agent's prepared review/PR).
//   - claimed: "I'll handle this myself" (the human took over the
//     task; the entity is still being worked on, just by hand).
//
// kind is "review" or "pr" — picks the right artifact noun so the
// next agent reading memory sees text that matches what was
// actually discarded (a review verdict vs a queued PR). Defaults
// to review wording for any unknown value.
func buildDiscardHumanContent(outcome discardOutcome, kind string) string {
	artifact := "review"
	verdictNoun := "verdict"
	if kind == "pr" {
		artifact = "PR"
		verdictNoun = "PR"
	}
	switch outcome {
	case discardOutcomeDismissed:
		return fmt.Sprintf(
			"**Outcome:** Human discarded the prepared %s and dismissed the task entirely.\n"+
				"**Implication:** The %s you proposed was not accepted, and the human chose to walk away from this entity rather than re-queue it. Future runs on similar entities should reconsider whether the situation warrants action at all.",
			artifact, verdictNoun)
	case discardOutcomeCompleted:
		return fmt.Sprintf(
			"**Outcome:** Human marked the task complete without submitting the prepared %s.\n"+
				"**Implication:** The human acknowledged the task as resolved but chose not to apply your %s to the entity. They likely handled it manually or via a different framing. Future runs should consider whether the agent's path was the right one or whether the human's resolution implies a gap in the prompt's approach.",
			artifact, verdictNoun)
	case discardOutcomeClaimed:
		return fmt.Sprintf(
			"**Outcome:** Human discarded the prepared %s and claimed the task to handle it themselves.\n"+
				"**Implication:** The %s you proposed was not accepted. The human took over to work the entity manually rather than apply your %s or re-queue it for another agent attempt — a sign that automation wasn't the right fit for this case.",
			artifact, verdictNoun, artifact)
	default: // discardOutcomeRequeued
		return fmt.Sprintf(
			"**Outcome:** Human discarded the prepared %s without submitting it; task returned to the triage queue.\n"+
				"**Implication:** The %s you proposed was not accepted. Reconsider whether this entity warrants any %s at all, or whether a different framing is needed.",
			artifact, verdictNoun, artifact)
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
