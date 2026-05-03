package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/curator"
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
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	PinnedRepos      []string `json:"pinned_repos"`
	CuratorSessionID string   `json:"curator_session_id"` // optional; usually set by the runtime, not the user
}

// patchProjectRequest is the PATCH body shape. Pointers distinguish
// "absent → leave unchanged" from "explicit → overwrite". PinnedRepos
// uses *[]string so a client can clear it with [] without colliding
// with the absent case.
type patchProjectRequest struct {
	Name             *string   `json:"name"`
	Description      *string   `json:"description"`
	PinnedRepos      *[]string `json:"pinned_repos"`
	SummaryMD        *string   `json:"summary_md"`
	SummaryStale     *bool     `json:"summary_stale"`
	CuratorSessionID *string   `json:"curator_session_id"`
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
		Name:             name,
		Description:      req.Description,
		PinnedRepos:      pinned,
		CuratorSessionID: req.CuratorSessionID,
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
	if req.CuratorSessionID != nil {
		updated.CuratorSessionID = *req.CuratorSessionID
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

	// Stop any in-flight Curator chat for this project BEFORE the DB
	// delete: the goroutine writes terminal cancelled status into
	// curator_requests rows, which the FK cascade is about to drop.
	// Doing it in the right order means a user who deletes a project
	// mid-chat sees a deterministic terminal state on every row
	// rather than relying on cascade behavior to handle in-flight
	// rows. No-op when the curator runtime hasn't been wired (test
	// harnesses, fresh-install before first message).
	if s.curator != nil {
		s.curator.CancelProject(id)
	}

	if err := db.DeleteProject(s.db, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Best-effort on-disk cleanup. The DB delete is the source of
	// truth and a stale on-disk dir is recoverable (next run that
	// needs that path will recreate or surface the issue), so a
	// cleanup failure surfaces as a non-fatal warning rather than
	// a 5xx.
	//
	// Two failure modes both produce the same X-Cleanup-Warning so
	// the client always learns that on-disk state may be stale:
	//   - resolving the path failed (UserHomeDir error etc.) —
	//     cleanup couldn't even be attempted
	//   - resolving worked but RemoveAll failed
	//
	// The full error (with absolute path + OS-specific detail) is
	// logged server-side; the header is a generic message so we
	// don't leak filesystem layout to the client.
	const cleanupWarning = "on-disk cleanup of project knowledge dir failed; check server logs"
	dir, dirErr := curator.KnowledgeDir(id)
	switch {
	case dirErr != nil:
		log.Printf("[projects] cannot resolve knowledge dir for project %s; on-disk cleanup skipped: %v", id, dirErr)
		w.Header().Set("X-Cleanup-Warning", cleanupWarning)
	default:
		if rmErr := os.RemoveAll(dir); rmErr != nil && !os.IsNotExist(rmErr) {
			log.Printf("[projects] cleanup of project %s knowledge dir %q failed: %v", id, dir, rmErr)
			w.Header().Set("X-Cleanup-Warning", cleanupWarning)
		}
	}

	w.WriteHeader(http.StatusNoContent)
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
