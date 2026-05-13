package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

const maxChainSteps = 50

// handleChainStepsGet returns the ordered step list for a chain prompt.
// Always returns an array (never null) so frontend code can iterate
// without a nil check.
func (s *Server) handleChainStepsGet(w http.ResponseWriter, r *http.Request) {
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

	steps, err := s.chains.ListSteps(r.Context(), runmode.LocalDefaultOrg, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if steps == nil {
		steps = []domain.ChainStep{}
	}
	writeJSON(w, http.StatusOK, steps)
}

type chainStepInput struct {
	StepPromptID string `json:"step_prompt_id"`
	Brief        string `json:"brief"`
}

type chainStepsPutRequest struct {
	Steps []chainStepInput `json:"steps"`
}

// handleChainStepsPut replaces the chain prompt's step list. Validates
// that the chain prompt exists and is kind='chain', and that no step
// references another chain prompt (recursion guard at the API layer).
func (s *Server) handleChainStepsPut(w http.ResponseWriter, r *http.Request) {
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
	if prompt.Kind != domain.PromptKindChain {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "this prompt is not a chain (kind != 'chain')",
		})
		return
	}

	var req chainStepsPutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if len(req.Steps) > maxChainSteps {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "chain may not exceed " + strconv.Itoa(maxChainSteps) + " steps",
		})
		return
	}

	stepIDs := make([]string, 0, len(req.Steps))
	briefs := make([]string, 0, len(req.Steps))
	for i, step := range req.Steps {
		if step.StepPromptID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "step_prompt_id is required for every step",
			})
			return
		}
		stepPrompt, err := s.prompts.Get(r.Context(), runmode.LocalDefaultOrg, step.StepPromptID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if stepPrompt == nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "step " + strconv.Itoa(i) + " references a non-existent prompt",
			})
			return
		}
		// Recursion guard: a chain step must point at a leaf prompt.
		// Nested chains aren't supported in v1 and would also create
		// cycles if a chain referenced itself transitively.
		if stepPrompt.Kind == domain.PromptKindChain {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "step " + strconv.Itoa(i) + " references another chain prompt; nested chains aren't supported",
			})
			return
		}
		if step.StepPromptID == id {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "a chain cannot reference itself as a step",
			})
			return
		}
		stepIDs = append(stepIDs, step.StepPromptID)
		briefs = append(briefs, step.Brief)
	}

	if err := s.chains.ReplaceSteps(r.Context(), runmode.LocalDefaultOrg, id, stepIDs, briefs); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// chainRunResponse bundles the chain run row with its per-step runs
// and verdicts so the run-detail UI can render the timeline in one
// fetch instead of N+1.
type chainRunResponse struct {
	ChainRun *domain.ChainRun       `json:"chain_run"`
	Steps    []chainRunStepView     `json:"steps"`
}

type chainRunStepView struct {
	Step    domain.ChainStep      `json:"step"`
	Run     *domain.AgentRun      `json:"run,omitempty"`
	Verdict *domain.ChainVerdict  `json:"verdict,omitempty"`
}

func (s *Server) handleChainRunGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	cr, err := s.chains.GetRun(r.Context(), runmode.LocalDefaultOrg, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if cr == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "chain run not found"})
		return
	}

	steps, err := s.chains.ListSteps(r.Context(), runmode.LocalDefaultOrg, cr.ChainPromptID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	stepRuns, err := s.chains.RunsForChain(r.Context(), runmode.LocalDefaultOrg, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	runByStep := map[int]*domain.AgentRun{}
	runIDs := make([]string, 0, len(stepRuns))
	for i := range stepRuns {
		if stepRuns[i].ChainStepIndex != nil {
			runByStep[*stepRuns[i].ChainStepIndex] = &stepRuns[i]
			runIDs = append(runIDs, stepRuns[i].ID)
		}
	}

	verdictsByRun, err := s.chains.LatestVerdictsForRuns(r.Context(), runmode.LocalDefaultOrg, runIDs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	views := make([]chainRunStepView, 0, len(steps))
	for _, step := range steps {
		view := chainRunStepView{Step: step}
		if run, ok := runByStep[step.StepIndex]; ok {
			view.Run = run
			view.Verdict = verdictsByRun[run.ID]
		}
		views = append(views, view)
	}

	writeJSON(w, http.StatusOK, chainRunResponse{ChainRun: cr, Steps: views})
}

func (s *Server) handleChainRunCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	cr, err := s.chains.GetRun(r.Context(), runmode.LocalDefaultOrg, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if cr == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "chain run not found"})
		return
	}

	switch cr.Status {
	case domain.ChainRunStatusCompleted, domain.ChainRunStatusFailed,
		domain.ChainRunStatusAborted, domain.ChainRunStatusCancelled:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "chain run already terminal"})
		return
	}

	if err := s.spawner.CancelChain(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
}
