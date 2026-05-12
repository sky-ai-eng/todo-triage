package server

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// factoryDelegateRequest is the body for POST /api/factory/delegate.
// All four fields are required; dedup_key may be empty for non-
// discriminator event types (the common case).
type factoryDelegateRequest struct {
	EntityID  string `json:"entity_id"`
	EventType string `json:"event_type"`
	DedupKey  string `json:"dedup_key"`
	PromptID  string `json:"prompt_id"`
}

// factoryDelegateResponse mirrors the /api/tasks/{id}/swipe delegate
// response: on partial success (claim stamped, run didn't fire),
// DelegateError carries the spawner error and RunID stays empty. The
// FE renders the "delegate failed — retry" affordance on the bot-
// claimed card regardless of whether the failure was a 400-class
// (ErrPromptNotFound) or 500-class (DB / spawner internal) error.
// ClaimStamped is always true on a 200 response — the user's gesture
// committed at the claim axis even when the run didn't materialize.
type factoryDelegateResponse struct {
	TaskID        string `json:"task_id"`
	RunID         string `json:"run_id"`
	DelegateError string `json:"delegate_error,omitempty"`
	ClaimStamped  bool   `json:"claim_stamped"`
}

// handleFactoryDelegate is the drag-to-delegate endpoint behind the
// station drawer's drop-on-runs gesture. Find-or-create on the task
// keeps the UX uniform: every queued chip is draggable, and dropping
// either reuses the existing task at this station or synthesizes a
// new one anchored on the most recent matching event.
//
// Race-safe via the partial unique index on
// (entity_id, event_type, dedup_key) WHERE status NOT IN ('done',
// 'dismissed') — concurrent drops resolve to the same task.
func (s *Server) handleFactoryDelegate(w http.ResponseWriter, r *http.Request) {
	var req factoryDelegateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.EntityID == "" || req.EventType == "" || req.PromptID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "entity_id, event_type, and prompt_id are required"})
		return
	}

	// Entity must exist and be active. The factory snapshot's 60s
	// soft-close grace window means a chip can ride the final
	// animation hop after its entity already flipped to closed; if the
	// user drags during that window, we shouldn't synthesize a fresh
	// task on a merged/closed entity (no second close-cascade would
	// clean it up — it'd run to completion against a closed PR).
	// Mirrors the router's "task creation requires active entity"
	// contract at routing/router.go.
	entity, err := db.GetEntity(s.db, req.EntityID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if entity == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "entity not found"})
		return
	}
	if entity.State != "active" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "entity is closed; cannot delegate"})
		return
	}

	// Anchor the (possibly synthesized) task on the most recent event
	// matching all three of (entity_id, event_type, dedup_key). The
	// dedup_key filter is pushed into the SQL — picking the latest
	// event by type alone and rejecting a mismatch would 400 every
	// time a sibling discriminator (e.g. label_added "help wanted")
	// fired more recently than the dragged chip's discriminator
	// (label_added "bug"). If no matching event exists the entity
	// isn't actually at this station; refuse rather than fabricate
	// an anchor.
	primaryEvent, err := db.LatestEventForEntityTypeAndDedupKey(s.db, req.EntityID, req.EventType, req.DedupKey)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if primaryEvent == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no matching event for entity at this station"})
		return
	}

	// Spawner availability gate runs after every request + state
	// validation (400/404/409) so callers learn about bad input
	// before they hit the infrastructure gap. Sits before the
	// FindOrCreateTask + RecordSwipe writes so a missing spawner
	// can't leave a half-applied delegate (task + swipe row but no
	// run). Tests rely on this ordering to exercise the 404/409/400
	// paths without installing a spawner.
	if s.spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "spawner not configured"})
		return
	}

	// Default priority — mirrors internal/routing/router.go:210-215,
	// including the predicate-match filter. Iterating *all* enabled
	// rules for the event type would inflate priority whenever a
	// high-priority rule's scope_predicate doesn't actually match
	// this event's metadata (e.g., a 0.9-priority rule scoped to a
	// specific repo would lift priority for every event of that type
	// even on unrelated entities). Empty predJSON always matches per
	// the events package contract.
	defaultPriority := 0.5
	schema, schemaOK := events.Get(req.EventType)
	handlers, err := s.eventHandlers.GetEnabledForEvent(r.Context(), runmode.LocalDefaultOrg, req.EventType)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	for _, h := range handlers {
		if h.Kind != domain.EventHandlerKindRule || h.DefaultPriority == nil {
			// Trigger rows have no DefaultPriority; skip.
			continue
		}
		if !schemaOK {
			// No registered schema → predicate can't be evaluated.
			// Mirrors matchPredicate's quietly-permissive behavior:
			// the rule is skipped, falling back to 0.5.
			continue
		}
		predJSON := ""
		if h.ScopePredicateJSON != nil {
			predJSON = *h.ScopePredicateJSON
		}
		matched, err := schema.Match(predJSON, primaryEvent.MetadataJSON)
		if err != nil {
			log.Printf("[factory] event_handler %s predicate error: %v", h.ID, err)
			continue
		}
		if matched && *h.DefaultPriority > defaultPriority {
			defaultPriority = *h.DefaultPriority
		}
	}

	task, created, err := db.FindOrCreateTask(s.db, req.EntityID, req.EventType, req.DedupKey, primaryEvent.ID, defaultPriority)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Mirror the router's audit linkage: when a brand-new task is
	// synthesized, record a task_events row linking it to the event
	// it was anchored on. Same kind="spawned" the router uses at
	// internal/routing/router.go:223-227, so a future timeline UI
	// reading task_events sees a uniform shape regardless of which
	// path created the task. Non-fatal — audit gap is preferable to
	// failing the delegate after the row is already in tasks.
	//
	// We don't record kind="bumped" on the find branch: the drag is
	// the user's gesture (already captured in swipe_events via
	// RecordSwipe), not a fresh event landing — there's nothing new
	// to link the existing task to.
	if created {
		if err := db.RecordTaskEvent(s.db, task.ID, primaryEvent.ID, "spawned"); err != nil {
			log.Printf("[factory] failed to record spawned task_event for %s: %v", task.ID, err)
		}
	}

	// SKY-261 B+ alignment with swipe-delegate: the user's gesture is
	// commitment regardless of run outcome. Stamp the agent claim
	// BEFORE attempting the spawn so a failed Delegate leaves the
	// task in the bot's lane (with no run, surfacing as a "delegate
	// failed — retry" card on the Board) rather than disappearing
	// the gesture entirely. The user-facing semantic: "I told the bot
	// to take this. The bot took the assignment but couldn't get the
	// run going on this attempt."
	//
	// claim is the responsibility axis (commitment); runs are the
	// execution axis. They're orthogonal; a failed run doesn't
	// invalidate the assignment.
	if s.agents == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "delegate failed: agent store not configured"})
		return
	}
	a, aerr := s.agents.GetForOrg(r.Context(), runmode.LocalDefaultOrg)
	if aerr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "delegate failed: agent lookup: " + aerr.Error()})
		return
	}
	if a == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "delegate failed: no agent bootstrapped — set up the bot first"})
		return
	}
	// Race-safe stamp: refuses to steal a user claim that landed
	// during the race window between drag-to-bot and this UPDATE.
	// If a user claimed the task simultaneously, return 409 — the
	// bot's commitment never lands, and the user's claim wins.
	// Same-agent rewrites are skipped as no-ops.
	stampOK, serr := db.StampAgentClaimIfUnclaimed(s.db, task.ID, a.ID)
	if serr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "claim stamp failed: " + serr.Error()})
		return
	}
	if !stampOK {
		// Re-read to figure out who currently owns it.
		cur, ferr := db.GetTask(s.db, task.ID)
		if ferr != nil || cur == nil {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "claim race lost; refetch task and retry",
			})
			return
		}
		if cur.ClaimedByUserID != "" {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "task was claimed by a user during delegate; refusing to steal",
			})
			return
		}
		// Bot already owns it (e.g., a sibling factory drop landed
		// first) — idempotent no-op, skip the broadcast and
		// continue to the spawn so the user's gesture still gets
		// a run if one isn't already underway.
	} else {
		s.ws.Broadcast(websocket.Event{
			Type: "task_claimed",
			Data: map[string]any{
				"task_id":             task.ID,
				"claimed_by_agent_id": a.ID,
				"claimed_by_user_id":  "",
			},
		})
	}

	// Now attempt the spawn. Delegate's failure modes (prompt not
	// found, DB error creating the run row) DON'T unstamp the claim
	// — the user's commitment is real, the run just didn't fire.
	// The response carries delegate_error so the FE can render the
	// "delegate failed — retry" affordance on the now-bot-claimed card.
	// task.ClaimedByAgentID is set so spawner.Delegate's actor stamping
	// reads it correctly.
	task.ClaimedByAgentID = a.ID
	runID, err := s.spawner.Delegate(*task, req.PromptID, "manual", "")
	if err != nil {
		// Claim is already stamped (and broadcast). The run didn't
		// fire — mirror the swipe-delegate convention: 200 OK with
		// delegate_error populated and run_id empty. The FE reads
		// delegate_error to render the "delegate didn't fire, retry"
		// affordance on the now-bot-claimed card; refetch still fires
		// so the Board picks up the new claim col + the task surfaces
		// in the Agent lane immediately. Whether the underlying error
		// was a 400-class (ErrPromptNotFound, ErrPromptUnspecified) or
		// 500-class (DB, spawner internal) is irrelevant to the
		// caller — the response shape is the same.
		writeJSON(w, http.StatusOK, factoryDelegateResponse{
			TaskID:        task.ID,
			RunID:         "",
			DelegateError: err.Error(),
			ClaimStamped:  true,
		})
		return
	}

	// Run is real; commit the swipe_events audit row. RecordSwipe
	// failure is non-fatal — the run is in flight and the claim is
	// already stamped, so the gesture is fully captured at the
	// state level even without the audit row.
	if _, err := s.swipes.RecordSwipe(r.Context(), runmode.LocalDefaultOrg, task.ID, "delegate", 0); err != nil {
		log.Printf("[factory] failed to record delegate swipe for task %s after run %s started: %v",
			task.ID, runID, err)
	}

	writeJSON(w, http.StatusOK, factoryDelegateResponse{
		TaskID:       task.ID,
		RunID:        runID,
		ClaimStamped: true,
	})
}
