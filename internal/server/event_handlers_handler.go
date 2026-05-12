package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// /api/event-handlers — unified successor to /api/task-rules + /api/triggers
// (SKY-259). The two frontend pages (rules tab + triggers tab) keep their
// split UX but hit this one endpoint family with a kind filter.
//
// Wire shape:
//
//   GET    /api/event-handlers[?kind=rule|trigger]   — list
//   POST   /api/event-handlers                        — create (kind in body)
//   PATCH  /api/event-handlers/{id}                   — partial update
//   PUT    /api/event-handlers/{id}                   — replacement update
//                                                       (alias for PATCH for
//                                                       trigger-style "send
//                                                       the full mutable set"
//                                                       calls — same handler)
//   DELETE /api/event-handlers/{id}                   — hard delete; system
//                                                       rows soft-disable
//   POST   /api/event-handlers/{id}/toggle            — flip enabled bit
//   POST   /api/event-handlers/{id}/promote           — rule → trigger
//   PUT    /api/event-handlers/reorder                — rules-only sort_order

// GET /api/event-handlers
func (s *Server) handleEventHandlersList(w http.ResponseWriter, r *http.Request) {
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind != "" && kind != domain.EventHandlerKindRule && kind != domain.EventHandlerKindTrigger {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "kind must be 'rule', 'trigger', or omitted (returns both)",
		})
		return
	}
	handlers, err := s.eventHandlers.List(r.Context(), runmode.LocalDefaultOrg, kind)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if handlers == nil {
		handlers = []domain.EventHandler{}
	}
	writeJSON(w, http.StatusOK, handlers)
}

// POST /api/event-handlers
//
// kind in body; per-kind fields are validated accordingly. Rule fields
// (name, default_priority, sort_order) are required for kind=rule;
// trigger fields (prompt_id, breaker_threshold, min_autonomy_suitability)
// are required for kind=trigger.
type createEventHandlerRequest struct {
	Kind               string `json:"kind"`
	EventType          string `json:"event_type"`
	ScopePredicateJSON string `json:"scope_predicate_json"`
	Enabled            *bool  `json:"enabled"`

	// Rule-only.
	Name            *string  `json:"name"`
	DefaultPriority *float64 `json:"default_priority"`
	SortOrder       *int     `json:"sort_order"`

	// Trigger-only.
	PromptID               string   `json:"prompt_id"`
	BreakerThreshold       *int     `json:"breaker_threshold"`
	MinAutonomySuitability *float64 `json:"min_autonomy_suitability"`
}

func (s *Server) handleEventHandlerCreate(w http.ResponseWriter, r *http.Request) {
	var req createEventHandlerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.Kind != domain.EventHandlerKindRule && req.Kind != domain.EventHandlerKindTrigger {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "kind must be 'rule' or 'trigger'",
		})
		return
	}
	if req.EventType == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "event_type is required"})
		return
	}
	if _, ok := events.Get(req.EventType); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown event_type: " + req.EventType})
		return
	}
	canonical, err := events.ValidatePredicateJSON(req.EventType, req.ScopePredicateJSON)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	h := domain.EventHandler{
		ID:        uuid.New().String(),
		Kind:      req.Kind,
		EventType: req.EventType,
		Source:    domain.EventHandlerSourceUser,
	}
	if canonical != "" {
		h.ScopePredicateJSON = &canonical
	}

	switch req.Kind {
	case domain.EventHandlerKindRule:
		if req.Name == nil || strings.TrimSpace(*req.Name) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required for kind=rule"})
			return
		}
		h.Name = strings.TrimSpace(*req.Name)
		priority := 0.5
		if req.DefaultPriority != nil {
			if *req.DefaultPriority < 0 || *req.DefaultPriority > 1 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "default_priority must be between 0 and 1"})
				return
			}
			priority = *req.DefaultPriority
		}
		h.DefaultPriority = &priority
		sortOrder := 0
		if req.SortOrder != nil {
			sortOrder = *req.SortOrder
		}
		h.SortOrder = &sortOrder
		h.Enabled = true
		if req.Enabled != nil {
			h.Enabled = *req.Enabled
		}

	case domain.EventHandlerKindTrigger:
		if req.PromptID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt_id is required for kind=trigger"})
			return
		}
		// Verify the prompt exists for clearer 404 than the downstream FK
		// integrity error.
		prompt, err := s.prompts.Get(r.Context(), runmode.LocalDefaultOrg, req.PromptID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if prompt == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "prompt not found"})
			return
		}
		h.PromptID = req.PromptID
		h.TriggerType = domain.TriggerTypeEvent
		threshold := 4
		if req.BreakerThreshold != nil {
			if *req.BreakerThreshold <= 0 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "breaker_threshold must be positive"})
				return
			}
			threshold = *req.BreakerThreshold
		}
		h.BreakerThreshold = &threshold
		minAutonomy := 0.0
		if req.MinAutonomySuitability != nil {
			if *req.MinAutonomySuitability < 0 || *req.MinAutonomySuitability > 1 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "min_autonomy_suitability must be between 0 and 1"})
				return
			}
			minAutonomy = *req.MinAutonomySuitability
		}
		h.MinAutonomySuitability = &minAutonomy
		// Triggers default disabled (project convention) — explicit
		// opt-in via Enabled=true survives the default.
		h.Enabled = false
		if req.Enabled != nil {
			h.Enabled = *req.Enabled
		}
	}

	if err := s.eventHandlers.Create(r.Context(), runmode.LocalDefaultOrg, h); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") || strings.Contains(err.Error(), "duplicate key") {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	fresh, _ := s.eventHandlers.Get(r.Context(), runmode.LocalDefaultOrg, h.ID)
	if fresh != nil {
		writeJSON(w, http.StatusCreated, fresh)
		return
	}
	writeJSON(w, http.StatusCreated, h)
}

// PATCH /api/event-handlers/{id} (also bound to PUT for trigger-style replace)
//
// Partial update. Any field left nil/absent is unchanged. kind and
// event_type are immutable (kind transitions go through /promote;
// event_type changes would invalidate the predicate schema). For
// triggers, prompt_id is also immutable here.
type patchEventHandlerRequest struct {
	ScopePredicateJSON json.RawMessage `json:"scope_predicate_json"`
	Enabled            *bool           `json:"enabled"`

	// Rule fields.
	Name            *string  `json:"name"`
	DefaultPriority *float64 `json:"default_priority"`
	SortOrder       *int     `json:"sort_order"`

	// Trigger fields.
	BreakerThreshold       *int     `json:"breaker_threshold"`
	MinAutonomySuitability *float64 `json:"min_autonomy_suitability"`
}

func (s *Server) handleEventHandlerUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req patchEventHandlerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	existing, err := s.eventHandlers.Get(r.Context(), runmode.LocalDefaultOrg, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if existing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "event handler not found"})
		return
	}

	updated := *existing

	if req.Enabled != nil {
		updated.Enabled = *req.Enabled
	}

	// Predicate update — three distinguishable cases:
	//   - absent (len==0):         leave unchanged
	//   - explicit null ("null"):  clear to match-all
	//   - JSON string / object:    validate + canonicalise
	if len(req.ScopePredicateJSON) > 0 {
		raw := string(req.ScopePredicateJSON)
		if raw == "null" {
			updated.ScopePredicateJSON = nil
		} else {
			var asString string
			if err := json.Unmarshal(req.ScopePredicateJSON, &asString); err == nil {
				raw = asString
			}
			canonical, err := events.ValidatePredicateJSON(existing.EventType, raw)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			if canonical == "" {
				updated.ScopePredicateJSON = nil
			} else {
				updated.ScopePredicateJSON = &canonical
			}
		}
	}

	switch existing.Kind {
	case domain.EventHandlerKindRule:
		if req.Name != nil {
			trimmed := strings.TrimSpace(*req.Name)
			if trimmed == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name cannot be empty"})
				return
			}
			updated.Name = trimmed
		}
		if req.DefaultPriority != nil {
			if *req.DefaultPriority < 0 || *req.DefaultPriority > 1 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "default_priority must be between 0 and 1"})
				return
			}
			v := *req.DefaultPriority
			updated.DefaultPriority = &v
		}
		if req.SortOrder != nil {
			v := *req.SortOrder
			updated.SortOrder = &v
		}

	case domain.EventHandlerKindTrigger:
		if req.BreakerThreshold != nil {
			if *req.BreakerThreshold <= 0 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "breaker_threshold must be positive"})
				return
			}
			v := *req.BreakerThreshold
			updated.BreakerThreshold = &v
		}
		if req.MinAutonomySuitability != nil {
			if *req.MinAutonomySuitability < 0 || *req.MinAutonomySuitability > 1 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "min_autonomy_suitability must be between 0 and 1"})
				return
			}
			v := *req.MinAutonomySuitability
			updated.MinAutonomySuitability = &v
		}
	}

	if err := s.eventHandlers.Update(r.Context(), runmode.LocalDefaultOrg, updated); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	fresh, _ := s.eventHandlers.Get(r.Context(), runmode.LocalDefaultOrg, id)
	if fresh != nil {
		writeJSON(w, http.StatusOK, fresh)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// DELETE /api/event-handlers/{id}
//
// User rows hard-delete; system rows soft-disable in place (Seed runs
// on every boot with INSERT-OR-IGNORE, so a hard delete would
// resurrect the row).
func (s *Server) handleEventHandlerDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	existing, err := s.eventHandlers.Get(r.Context(), runmode.LocalDefaultOrg, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if existing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "event handler not found"})
		return
	}

	if existing.Source == domain.EventHandlerSourceSystem {
		if err := s.eventHandlers.SetEnabled(r.Context(), runmode.LocalDefaultOrg, id, false); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"status": "disabled",
			"reason": "system handlers cannot be deleted (they would be resurrected on next boot); disabled instead",
		})
		return
	}

	if err := s.eventHandlers.Delete(r.Context(), runmode.LocalDefaultOrg, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// POST /api/event-handlers/{id}/toggle
func (s *Server) handleEventHandlerToggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	existing, err := s.eventHandlers.Get(r.Context(), runmode.LocalDefaultOrg, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if existing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "event handler not found"})
		return
	}
	if err := s.eventHandlers.SetEnabled(r.Context(), runmode.LocalDefaultOrg, id, req.Enabled); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": req.Enabled})
}

// POST /api/event-handlers/{id}/promote
//
// Rule → trigger transition. Body carries the trigger-side fields the
// promoted row needs (prompt_id, breaker_threshold, min_autonomy_suitability,
// optionally a new predicate). The store enforces atomicity via a single
// UPDATE that flips kind and populates the trigger fields together.
type promoteEventHandlerRequest struct {
	PromptID               string   `json:"prompt_id"`
	BreakerThreshold       *int     `json:"breaker_threshold"`
	MinAutonomySuitability *float64 `json:"min_autonomy_suitability"`
	ScopePredicateJSON     *string  `json:"scope_predicate_json"`
}

func (s *Server) handleEventHandlerPromote(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req promoteEventHandlerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.PromptID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt_id is required"})
		return
	}
	if req.BreakerThreshold == nil || req.MinAutonomySuitability == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "breaker_threshold and min_autonomy_suitability are required"})
		return
	}

	existing, err := s.eventHandlers.Get(r.Context(), runmode.LocalDefaultOrg, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if existing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "event handler not found"})
		return
	}
	if existing.Kind != domain.EventHandlerKindRule {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "only rules can be promoted"})
		return
	}

	prompt, err := s.prompts.Get(r.Context(), runmode.LocalDefaultOrg, req.PromptID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if prompt == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "prompt not found"})
		return
	}

	predicate := existing.ScopePredicateJSON
	if req.ScopePredicateJSON != nil {
		canonical, verr := events.ValidatePredicateJSON(existing.EventType, *req.ScopePredicateJSON)
		if verr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": verr.Error()})
			return
		}
		if canonical == "" {
			predicate = nil
		} else {
			predicate = &canonical
		}
	}

	target := domain.EventHandler{
		Kind:                   domain.EventHandlerKindTrigger,
		PromptID:               req.PromptID,
		BreakerThreshold:       req.BreakerThreshold,
		MinAutonomySuitability: req.MinAutonomySuitability,
		ScopePredicateJSON:     predicate,
	}
	if err := s.eventHandlers.Promote(r.Context(), runmode.LocalDefaultOrg, id, target); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	fresh, _ := s.eventHandlers.Get(r.Context(), runmode.LocalDefaultOrg, id)
	writeJSON(w, http.StatusOK, fresh)
}

// PUT /api/event-handlers/reorder
//
// Rules-only — trigger IDs in the list are silently skipped by the
// store (sort_order is rule-only by CHECK constraint).
func (s *Server) handleEventHandlerReorder(w http.ResponseWriter, r *http.Request) {
	var ids []string
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected array of handler IDs"})
		return
	}
	if len(ids) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty ID list"})
		return
	}
	if err := s.eventHandlers.Reorder(r.Context(), runmode.LocalDefaultOrg, ids); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reordered"})
}
