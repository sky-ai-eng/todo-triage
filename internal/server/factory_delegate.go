package server

import (
	"encoding/json"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/db"
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

type factoryDelegateResponse struct {
	TaskID string `json:"task_id"`
	RunID  string `json:"run_id"`
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

	// Spawner availability gate runs after request + state validation
	// so callers learn about bad input before they hit infrastructure
	// gaps. Tests rely on this ordering to exercise 404/409 paths
	// without installing a spawner.
	if s.spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "spawner not configured"})
		return
	}

	// Anchor the (possibly synthesized) task on the most recent event
	// of this type for the entity. For discriminator/open-set event
	// types, a non-empty dedup_key must also match the selected event;
	// otherwise we could create a task whose dedup_key does not match
	// its primary_event_id. If no matching event exists, the entity
	// isn't actually at this station — refuse rather than fabricate an
	// event row.
	primaryEvent, err := db.LatestEventForEntityAndType(s.db, req.EntityID, req.EventType)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if primaryEvent == nil || (req.DedupKey != "" && primaryEvent.DedupKey != req.DedupKey) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no matching event for entity at this station"})
		return
	}

	// Default priority — mirrors internal/routing/router.go:210-215.
	// Use the highest enabled task_rule's default_priority for this
	// event type, or 0.5 if no rules match.
	defaultPriority := 0.5
	rules, err := db.GetEnabledRulesForEvent(s.db, req.EventType)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	for _, rule := range rules {
		if rule.DefaultPriority > defaultPriority {
			defaultPriority = rule.DefaultPriority
		}
	}

	task, _, err := db.FindOrCreateTask(s.db, req.EntityID, req.EventType, req.DedupKey, primaryEvent.ID, defaultPriority)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Flip task.status to 'delegated' and emit a swipe_events audit
	// row, matching the swipe-to-delegate path on Board. Without this
	// the task stays 'queued' even with an active run, which leaks a
	// stale row into the queue surfaces (Board, factory drawer's own
	// queue tray) until the run terminates.
	//
	// hesitation_ms = 0 because the drop itself is the action — the
	// drag-to-delegate UX has no analogue to the Board's
	// "time-to-commit" metric.
	if _, err := db.RecordSwipe(s.db, task.ID, "delegate", 0); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	runID, err := s.spawner.Delegate(*task, req.PromptID, "manual", "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, factoryDelegateResponse{
		TaskID: task.ID,
		RunID:  runID,
	})
}
