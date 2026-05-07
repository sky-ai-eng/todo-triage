package server

import (
	"log"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// projectEntity is the per-row payload returned by
// GET /api/projects/{id}/entities. Trimmed down from domain.Entity —
// snapshot_json + description aren't useful in a list view, and
// elision keeps the response small for projects with many entities.
type projectEntity struct {
	ID                      string     `json:"id"`
	Source                  string     `json:"source"`
	SourceID                string     `json:"source_id"`
	Kind                    string     `json:"kind"`
	Title                   string     `json:"title"`
	URL                     string     `json:"url"`
	State                   string     `json:"state"`
	ClassificationRationale string     `json:"classification_rationale,omitempty"`
	LastPolledAt            *time.Time `json:"last_polled_at"`
	CreatedAt               time.Time  `json:"created_at"`
}

// handleProjectEntities returns the list of active entities assigned
// to this project, ordered most-recently-polled first. SKY-238.
//
// Active-only: terminal-state entities (closed PRs, completed Jiras)
// are filtered out at the DB layer. The panel surfaces work that's
// still in flight; historical context lives elsewhere (entity detail
// pages, future audit views) so the panel stays scannable.
func (s *Server) handleProjectEntities(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	project, err := db.GetProject(s.db, projectID)
	if err != nil {
		log.Printf("[entities] get project %s: %v", projectID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load project"})
		return
	}
	if project == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}

	entities, err := db.ListActiveEntitiesByProject(s.db, projectID)
	if err != nil {
		log.Printf("[entities] list for project %s: %v", projectID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load entities"})
		return
	}

	out := make([]projectEntity, 0, len(entities))
	for _, e := range entities {
		out = append(out, projectEntity{
			ID:                      e.ID,
			Source:                  e.Source,
			SourceID:                e.SourceID,
			Kind:                    e.Kind,
			Title:                   e.Title,
			URL:                     e.URL,
			State:                   e.State,
			ClassificationRationale: e.ClassificationRationale,
			LastPolledAt:            e.LastPolledAt,
			CreatedAt:               e.CreatedAt,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"entities": out})
}
