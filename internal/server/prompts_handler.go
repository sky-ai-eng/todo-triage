package server

import (
	"encoding/json"
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if prompt == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "prompt not found"})
		return
	}

	writeJSON(w, http.StatusOK, prompt)
}

type createPromptRequest struct {
	Name  string `json:"name"`
	Body  string `json:"body"`
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.Name == "" || req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and body are required"})
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
		Model:  req.Model,
	}

	if err := s.prompts.Create(r.Context(), runmode.LocalDefaultOrg, prompt); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	created, _ := s.prompts.Get(r.Context(), runmode.LocalDefaultOrg, id)
	writeJSON(w, http.StatusCreated, created)
}

type updatePromptRequest struct {
	Name  string `json:"name"`
	Body  string `json:"body"`
	Model string `json:"model"`
}

func (s *Server) handlePromptPut(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req updatePromptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.Name == "" || req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and body are required"})
		return
	}
	if !validPromptModel(req.Model) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": invalidPromptModelError()})
		return
	}

	if err := s.prompts.Update(r.Context(), runmode.LocalDefaultOrg, id, req.Name, req.Body, req.Model); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	updated, _ := s.prompts.Get(r.Context(), runmode.LocalDefaultOrg, id)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handlePromptDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	prompt, err := s.prompts.Get(r.Context(), runmode.LocalDefaultOrg, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if prompt == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "prompt not found"})
		return
	}

	// Block deletion if the prompt is referenced by any auto-triggers.
	// Post-SKY-259 triggers live in event_handlers with kind='trigger';
	// ListForPrompt returns only those.
	triggers, err := s.eventHandlers.ListForPrompt(r.Context(), runmode.LocalDefaultOrg, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if len(triggers) > 0 {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "This prompt is used by an auto-delegation trigger. Remove the trigger first.",
		})
		return
	}

	// System and imported prompts are soft-deleted (hidden), user prompts are hard-deleted
	if prompt.Source == "system" || prompt.Source == "imported" {
		if err := s.prompts.Hide(r.Context(), runmode.LocalDefaultOrg, id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "hidden"})
		return
	}

	if err := s.prompts.Delete(r.Context(), runmode.LocalDefaultOrg, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handlePromptStats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	stats, err := s.prompts.Stats(r.Context(), runmode.LocalDefaultOrg, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, stats)
}
