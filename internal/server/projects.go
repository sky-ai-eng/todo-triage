package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SKY-215. Projects are the data layer underneath the Curator stack —
// the long-lived per-project context that the rest of the SKY-211
// family hangs work onto. This file is pure CRUD + on-disk knowledge
// dir cleanup; the Curator runtime, classifier, and UI all land in
// later tickets and can hit the same handlers without changes here.

// createProjectRequest is the POST body shape. Most fields are
// optional — a project starts as an empty shell named by the user
// and gets filled in over time (description, pinned repos, summary).
type createProjectRequest struct {
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	PinnedRepos       []string `json:"pinned_repos"`
	DesignerSessionID string   `json:"designer_session_id"` // optional; usually set by SKY-216 not the user
}

// patchProjectRequest is the PATCH body shape. Pointers distinguish
// "absent → leave unchanged" from "explicit → overwrite". PinnedRepos
// uses *[]string so a client can clear it with [] without colliding
// with the absent case.
type patchProjectRequest struct {
	Name              *string   `json:"name"`
	Description       *string   `json:"description"`
	PinnedRepos       *[]string `json:"pinned_repos"`
	SummaryMD         *string   `json:"summary_md"`
	SummaryStale      *bool     `json:"summary_stale"`
	DesignerSessionID *string   `json:"designer_session_id"`
}

func (s *Server) handleProjectCreate(w http.ResponseWriter, r *http.Request) {
	var req createProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	pinned, errMsg := validatePinnedRepos(req.PinnedRepos)
	if errMsg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
		return
	}

	id, err := db.CreateProject(s.db, domain.Project{
		Name:              name,
		Description:       req.Description,
		PinnedRepos:       pinned,
		DesignerSessionID: req.DesignerSessionID,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	created, err := db.GetProject(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "created but read-back failed: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleProjectList(w http.ResponseWriter, _ *http.Request) {
	projects, err := db.ListProjects(s.db)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, projects)
}

func (s *Server) handleProjectGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	project, err := db.GetProject(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if project == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) handleProjectUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req patchProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	existing, err := db.GetProject(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if existing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}

	updated := *existing
	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if trimmed == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name cannot be empty"})
			return
		}
		updated.Name = trimmed
	}
	if req.Description != nil {
		updated.Description = *req.Description
	}
	if req.PinnedRepos != nil {
		pinned, errMsg := validatePinnedRepos(*req.PinnedRepos)
		if errMsg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
			return
		}
		updated.PinnedRepos = pinned
	}
	if req.SummaryMD != nil {
		updated.SummaryMD = *req.SummaryMD
	}
	if req.SummaryStale != nil {
		updated.SummaryStale = *req.SummaryStale
	}
	if req.DesignerSessionID != nil {
		updated.DesignerSessionID = *req.DesignerSessionID
	}

	if err := db.UpdateProject(s.db, updated); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	fresh, err := db.GetProject(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "updated but read-back failed: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, fresh)
}

func (s *Server) handleProjectDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := db.DeleteProject(s.db, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Best-effort on-disk cleanup. The Curator runtime (SKY-216)
	// hasn't shipped, so today this dir likely doesn't exist for
	// any project — but the contract is "delete clears local
	// state" so we walk through it now to keep that contract
	// consistent once that runtime starts populating files. The
	// DB delete is the source of truth and a stale on-disk dir is
	// recoverable (next run that needs that path will recreate or
	// surface the issue), so a removal failure surfaces as a
	// non-fatal warning rather than a 5xx.
	//
	// The full error (with absolute path + OS-specific detail) is
	// logged server-side; the X-Cleanup-Warning header is a
	// generic message so we don't leak filesystem layout to the
	// client.
	if dir, err := projectKnowledgeDir(id); err == nil {
		if rmErr := os.RemoveAll(dir); rmErr != nil && !os.IsNotExist(rmErr) {
			log.Printf("[projects] cleanup of project %s knowledge dir %q failed: %v", id, dir, rmErr)
			w.Header().Set("X-Cleanup-Warning", "on-disk cleanup of project knowledge dir failed; check server logs")
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// projectKnowledgeDir returns ~/.triagefactory/projects/<id>/. The
// Curator runtime (SKY-216) is the producer; this resolver lives
// here because the delete handler is the only consumer until that
// ticket lands.
func projectKnowledgeDir(id string) (string, error) {
	if id == "" {
		return "", errors.New("project id is required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".triagefactory", "projects", id), nil
}

// validatePinnedRepos validates the "owner/repo" slug shape AND
// returns the normalized (trimmed) slice that callers should
// persist. Without the normalization step, " owner/repo " would
// pass validation (which trims) but get stored padded, making
// future lookups by slug miss the row. The shape is GitHub-specific
// today; flagged in the SKY-215 ticket as not blocking v1 (no
// second forge exists yet).
//
// Returns (normalized, "") on success and (nil, errMsg) on failure.
func validatePinnedRepos(repos []string) ([]string, string) {
	out := make([]string, len(repos))
	for i, r := range repos {
		trimmed := strings.TrimSpace(r)
		if trimmed == "" {
			return nil, "pinned_repos[" + strconv.Itoa(i) + "] is empty"
		}
		// Require exactly one '/' with non-empty owner and repo.
		// Anything else (no slash, leading/trailing slash, multiple
		// slashes) is rejected — the slug shape is "owner/repo".
		parts := strings.Split(trimmed, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, "pinned_repos[" + strconv.Itoa(i) + "] must be 'owner/repo'"
		}
		out[i] = trimmed
	}
	return out, ""
}
