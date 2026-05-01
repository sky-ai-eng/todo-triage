package server

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
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
	rules, err := db.GetEnabledRulesForEvent(s.db, req.EventType)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	for _, rule := range rules {
		if !schemaOK {
			// No registered schema → predicate can't be evaluated.
			// Mirrors matchPredicate's quietly-permissive behavior:
			// the rule is skipped, falling back to 0.5.
			continue
		}
		predJSON := ""
		if rule.ScopePredicateJSON != nil {
			predJSON = *rule.ScopePredicateJSON
		}
		matched, err := schema.Match(predJSON, primaryEvent.MetadataJSON)
		if err != nil {
			log.Printf("[factory] rule %s predicate error: %v", rule.ID, err)
			continue
		}
		if matched && rule.DefaultPriority > defaultPriority {
			defaultPriority = rule.DefaultPriority
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
