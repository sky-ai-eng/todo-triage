package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// handleDashboardStats returns aggregated PR statistics from entity snapshots.
func (s *Server) handleDashboardStats(w http.ResponseWriter, r *http.Request) {
	creds, err := auth.Load()
	if err != nil || creds.GitHubPAT == "" {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	username, _ := s.users.GetGitHubUsername(r.Context(), runmode.LocalDefaultUserID)
	if username == "" {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}

	stats, err := s.dashboard.Stats(r.Context(), runmode.LocalDefaultOrg, username, 30)
	if err != nil {
		internalError(w, "dashboard", err)
		return
	}

	writeJSON(w, http.StatusOK, stats)
}

// handleDashboardPRs returns open PRs from entity snapshots.
func (s *Server) handleDashboardPRs(w http.ResponseWriter, r *http.Request) {
	creds, err := auth.Load()
	if err != nil || creds.GitHubPAT == "" {
		writeJSON(w, http.StatusOK, []domain.PRSummaryRow{})
		return
	}
	username, _ := s.users.GetGitHubUsername(r.Context(), runmode.LocalDefaultUserID)
	if username == "" {
		writeJSON(w, http.StatusOK, []domain.PRSummaryRow{})
		return
	}

	prs, err := s.dashboard.PRs(r.Context(), runmode.LocalDefaultOrg, username)
	if err != nil {
		internalError(w, "dashboard", err)
		return
	}
	if prs == nil {
		prs = []domain.PRSummaryRow{}
	}
	writeJSON(w, http.StatusOK, prs)
}

// handleDashboardPRStatus fetches live CI/review status for a single PR.
// This stays as a live API call since it's on-demand detail, not aggregated data.
func (s *Server) handleDashboardPRStatus(w http.ResponseWriter, r *http.Request) {
	numberStr := r.PathValue("number")
	number, err := strconv.Atoi(numberStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid PR number"})
		return
	}

	repoParam := r.URL.Query().Get("repo")
	if repoParam == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo query parameter required (owner/repo)"})
		return
	}
	parts := strings.SplitN(repoParam, "/", 2)
	if len(parts) != 2 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo must be owner/repo format"})
		return
	}

	creds, err := auth.Load()
	if err != nil || creds.GitHubPAT == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub not configured"})
		return
	}
	cfg, _ := config.Load()
	baseURL := cfg.GitHub.BaseURL
	if baseURL == "" {
		baseURL = creds.GitHubURL
	}

	client := ghclient.NewClient(baseURL, creds.GitHubPAT)
	status, err := client.GetPRStatus(parts[0], parts[1], number)
	if err != nil {
		internalError(w, "dashboard", err)
		return
	}

	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleDashboardPRDraft(w http.ResponseWriter, r *http.Request) {
	numberStr := r.PathValue("number")
	number, err := strconv.Atoi(numberStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid PR number"})
		return
	}

	repoParam := r.URL.Query().Get("repo")
	parts := strings.SplitN(repoParam, "/", 2)
	if len(parts) != 2 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo must be owner/repo"})
		return
	}

	var body struct {
		Draft bool `json:"draft"`
	}
	if !decodeJSON(w, r, &body, "") {
		return
	}

	creds, _ := auth.Load()
	cfg, _ := config.Load()
	baseURL := cfg.GitHub.BaseURL
	if baseURL == "" {
		baseURL = creds.GitHubURL
	}
	client := ghclient.NewClient(baseURL, creds.GitHubPAT)

	if body.Draft {
		err = client.ConvertPRToDraft(parts[0], parts[1], number)
	} else {
		err = client.MarkPRReady(parts[0], parts[1], number)
	}
	if err != nil {
		internalError(w, "dashboard", err)
		return
	}

	// Patch the local entity snapshot to match the state we just pushed to
	// GitHub. Without this, the frontend's subsequent fetchAll() reads the
	// stale pre-mutation snapshot and the card snaps back to its old column
	// until the next poll cycle (up to several minutes later).
	//
	// TODO(SKY-193): we deliberately don't fire a synthetic pr:ready_for_review
	// / pr:converted_to_draft event here — the user's UI click is its own
	// signal and a second event would race the next poll's diff and confuse
	// the audit trail. Revisit if a user reports "my trigger didn't fire
	// when I dragged the card."
	sourceID := fmt.Sprintf("%s/%s#%d", parts[0], parts[1], number)
	if patchErr := patchPRSnapshotDraft(r.Context(), s.entities, sourceID, body.Draft); patchErr != nil {
		log.Printf("[dashboard] warning: failed to patch snapshot for %s after draft toggle: %v", sourceID, patchErr)
	}

	writeJSON(w, http.StatusOK, map[string]any{"draft": body.Draft})
}

// patchPRSnapshotDraft flips the is_draft field on an entity's PR snapshot
// in place after a successful external mutation. Best-effort: returns nil
// silently if the entity hasn't been discovered yet (e.g. user mutated
// before the first poll) — the poller will populate it eventually.
// Race window: a concurrent in-flight poll can overwrite our patch with
// its pre-mutation snapshot. Acceptable for beta — the next poll corrects
// it, and the window is small. PatchSnapshot intentionally does NOT bump
// last_polled_at so the next poll still refreshes the row.
func patchPRSnapshotDraft(ctx context.Context, entities db.EntityStore, sourceID string, draft bool) error {
	entity, err := entities.GetBySource(ctx, runmode.LocalDefaultOrgID, "github", sourceID)
	if err != nil {
		return err
	}
	if entity == nil {
		return nil
	}
	snapshotJSON := strings.TrimSpace(entity.SnapshotJSON)
	if snapshotJSON == "" || snapshotJSON == "{}" {
		return nil
	}
	var snap domain.PRSnapshot
	if err := json.Unmarshal([]byte(snapshotJSON), &snap); err != nil {
		return err
	}
	snap.IsDraft = draft
	patched, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return entities.PatchSnapshot(ctx, runmode.LocalDefaultOrgID, entity.ID, string(patched))
}
