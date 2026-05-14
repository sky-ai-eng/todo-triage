package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func (s *Server) handleEventTypes(w http.ResponseWriter, r *http.Request) {
	types, err := db.ListEventTypes(s.db)
	if err != nil {
		internalError(w, "prompts", err)
		return
	}
	if types == nil {
		types = []domain.EventType{}
	}
	writeJSON(w, http.StatusOK, types)
}

func (s *Server) handlePromptsList(w http.ResponseWriter, r *http.Request) {
	prompts, err := s.prompts.List(r.Context(), runmode.LocalDefaultOrg)
	if err != nil {
		internalError(w, "prompts", err)
		return
	}
	if prompts == nil {
		prompts = []domain.Prompt{}
	}
	writeJSON(w, http.StatusOK, prompts)
}

func (s *Server) handlePromptGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	prompt, err := s.prompts.Get(r.Context(), runmode.LocalDefaultOrg, id)
	if err != nil {
		internalError(w, "prompts", err)
		return
	}
	if prompt == nil {
		notFound(w, "prompt")
		return
	}

	writeJSON(w, http.StatusOK, prompt)
}

type createPromptRequest struct {
	Name  string `json:"name"`
	Body  string `json:"body"`
	Kind  string `json:"kind"`
	Model string `json:"model"`
}

// allowedPromptModelOverrides is the set of non-empty values accepted
// for prompts.model. "" is always allowed and means "inherit the
// global default from settings.AI.Model at dispatch". Kept aligned
// with the picker in frontend/src/pages/Settings.tsx.
var allowedPromptModelOverrides = []string{"haiku", "sonnet", "opus"}

func validPromptModel(m string) bool {
	if m == "" {
		return true
	}
	for _, v := range allowedPromptModelOverrides {
		if m == v {
			return true
		}
	}
	return false
}

func invalidPromptModelError() string {
	return `model must be "" or one of: ` + strings.Join(allowedPromptModelOverrides, ", ")
}

func (s *Server) handlePromptCreate(w http.ResponseWriter, r *http.Request) {
	var req createPromptRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	kind := normalizePromptKind(req.Kind)
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	// Leaf prompts must carry a body (the mission). Chain prompts may
	// store a description in body or leave it empty — the steps are
	// the real definition and live in prompt_chain_steps.
	if kind == domain.PromptKindLeaf && req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is required for leaf prompts"})
		return
	}
	if !validPromptModel(req.Model) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": invalidPromptModelError()})
		return
	}

	id := uuid.New().String()
	prompt := domain.Prompt{
		ID:     id,
		Name:   req.Name,
		Body:   req.Body,
		Source: "user",
		Kind:   kind,
		Model:  req.Model,
	}

	if err := s.prompts.Create(r.Context(), runmode.LocalDefaultOrg, prompt); err != nil {
		internalError(w, "prompts", err)
		return
	}

	created, _ := s.prompts.Get(r.Context(), runmode.LocalDefaultOrg, id)
	writeJSON(w, http.StatusCreated, created)
}

type updatePromptRequest struct {
	Name  string `json:"name"`
	Body  string `json:"body"`
	Kind  string `json:"kind"`
	Model string `json:"model"`
}

func (s *Server) handlePromptPut(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req updatePromptRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	kind := normalizePromptKind(req.Kind)
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if kind == domain.PromptKindLeaf && req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is required for leaf prompts"})
		return
	}
	if !validPromptModel(req.Model) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": invalidPromptModelError()})
		return
	}

	existing, err := s.prompts.Get(r.Context(), runmode.LocalDefaultOrg, id)
	if err != nil {
		internalError(w, "prompts", err)
		return
	}
	if existing == nil {
		notFound(w, "prompt")
		return
	}

	if existing.Kind != kind {
		if existing.Kind == domain.PromptKindChain {
			// Reject chain→leaf if any chain steps exist.
			steps, err := s.chains.ListSteps(r.Context(), runmode.LocalDefaultOrg, id)
			if err != nil {
				internalError(w, "prompts", err)
				return
			}
			if len(steps) > 0 {
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": fmt.Sprintf("cannot change kind: prompt has %d chain step(s)", len(steps)),
				})
				return
			}
		} else {
			// Reject leaf→chain if triggers, runs, or *other* chains
			// reference this prompt. The chain-step check matters because
			// existing chains that embed this prompt as a step would
			// suddenly point at a chain-kind prompt; the chain-step API
			// explicitly rejects nested chains, so the chain would fail
			// at delegate time instead of definition time.
			triggers, err := s.eventHandlers.ListForPrompt(r.Context(), runmode.LocalDefaultOrg, id)
			if err != nil {
				internalError(w, "prompts", err)
				return
			}
			runCount, err := s.prompts.CountRunReferences(r.Context(), runmode.LocalDefaultOrg, id)
			if err != nil {
				internalError(w, "prompts", err)
				return
			}
			stepRefs, err := s.chains.CountStepReferences(r.Context(), runmode.LocalDefaultOrg, id)
			if err != nil {
				internalError(w, "prompts", err)
				return
			}
			if len(triggers) > 0 || runCount > 0 || stepRefs > 0 {
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": fmt.Sprintf("cannot change kind: prompt is referenced by %d trigger(s), %d run(s), and %d chain step(s)", len(triggers), runCount, stepRefs),
				})
				return
			}
		}
	}

	if err := s.prompts.Update(r.Context(), runmode.LocalDefaultOrg, id, req.Name, req.Body, string(kind), req.Model); err != nil {
		internalError(w, "prompts", err)
		return
	}

	updated, _ := s.prompts.Get(r.Context(), runmode.LocalDefaultOrg, id)
	writeJSON(w, http.StatusOK, updated)
}

// normalizePromptKind defaults to leaf for blank or unknown values so
// legacy clients that don't send `kind` keep working.
func normalizePromptKind(k string) domain.PromptKind {
	switch domain.PromptKind(k) {
	case domain.PromptKindChain:
		return domain.PromptKindChain
	default:
		return domain.PromptKindLeaf
	}
}

func (s *Server) handlePromptDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	prompt, err := s.prompts.Get(r.Context(), runmode.LocalDefaultOrg, id)
	if err != nil {
		internalError(w, "prompts", err)
		return
	}
	if prompt == nil {
		notFound(w, "prompt")
		return
	}

	// Block deletion if the prompt is referenced by any auto-triggers.
	// Post-SKY-259 triggers live in event_handlers with kind='trigger';
	// ListForPrompt returns only those.
	triggers, err := s.eventHandlers.ListForPrompt(r.Context(), runmode.LocalDefaultOrg, id)
	if err != nil {
		internalError(w, "prompts", err)
		return
	}
	if len(triggers) > 0 {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "This prompt is used by an auto-delegation trigger. Remove the trigger first.",
		})
		return
	}

	// Block deletion if this prompt is a step inside any chain. The FK
	// is ON DELETE RESTRICT so the underlying constraint would fire
	// anyway; we surface a friendlier message and the count of chains.
	chainRefs, err := s.chains.CountStepReferences(r.Context(), runmode.LocalDefaultOrg, id)
	if err != nil {
		internalError(w, "prompts", err)
		return
	}
	if chainRefs > 0 {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "This prompt is used as a step in one or more chains. Remove it from those chains first.",
		})
		return
	}

	// System and imported prompts are soft-deleted (hidden), user prompts are hard-deleted
	if prompt.Source == "system" || prompt.Source == "imported" {
		if err := s.prompts.Hide(r.Context(), runmode.LocalDefaultOrg, id); err != nil {
			internalError(w, "prompts", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "hidden"})
		return
	}

	if err := s.prompts.Delete(r.Context(), runmode.LocalDefaultOrg, id); err != nil {
		internalError(w, "prompts", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handlePromptStats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	stats, err := s.prompts.Stats(r.Context(), runmode.LocalDefaultOrg, id)
	if err != nil {
		internalError(w, "prompts", err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}
