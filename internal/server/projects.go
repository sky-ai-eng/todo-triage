package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
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
	JiraProjectKey   string   `json:"jira_project_key"`
	LinearProjectKey string   `json:"linear_project_key"`
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
	JiraProjectKey   *string   `json:"jira_project_key"`
	LinearProjectKey *string   `json:"linear_project_key"`
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
	pinned, errMsg := validatePinnedRepos(s.db, req.PinnedRepos)
	if errMsg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
		return
	}
	jiraKey := strings.TrimSpace(req.JiraProjectKey)
	linearKey := strings.TrimSpace(req.LinearProjectKey)
	// Mirror the PATCH path's lazy-load policy: only read config
	// when the Jira side actually needs validation. Linear validation
	// rejects non-empty regardless of cfg (integration not configured
	// yet), so loading config for a Linear-only POST would turn a
	// deterministic 400 into a 500 if config.yaml is broken — even
	// though that field's validation never reads it.
	cfg := config.Config{}
	if jiraKey != "" {
		loaded, err := config.Load()
		if err != nil {
			log.Printf("handleProjectCreate: failed to load config: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load config"})
			return
		}
		cfg = loaded
	}
	if jiraKey != "" || linearKey != "" {
		jiraKey, linearKey, errMsg = validateTrackerKeys(cfg, jiraKey, linearKey)
		if errMsg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
			return
		}
	}

	id, err := db.CreateProject(s.db, domain.Project{
		Name:             name,
		Description:      req.Description,
		PinnedRepos:      pinned,
		JiraProjectKey:   jiraKey,
		LinearProjectKey: linearKey,
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

	// Per-project lock around the read-merge-write window so two
	// concurrent autosaves don't lost-update each other. Two quick
	// edits from different widgets (pinned-repos chip + tracker
	// picker) would otherwise both read pre-edit state, merge their
	// own field, and serially overwrite the row — leaving whichever
	// landed second as the only contribution. Holding the mutex
	// across read+merge+write makes the whole sequence atomic from
	// the perspective of other PATCHes for the same project.
	mu := s.projectMutex(id)
	mu.Lock()
	defer mu.Unlock()

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
		pinned, errMsg := validatePinnedRepos(s.db, *req.PinnedRepos)
		if errMsg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
			return
		}
		updated.PinnedRepos = pinned
	}
	// Validate only the fields the client sent. Re-validating the
	// untouched side against the current config would surface a
	// confusing error if the config drifted (e.g. a Jira project
	// got renamed in Settings) on an unrelated PATCH that's only
	// touching, say, the Linear key. The handler's contract is
	// "validate what the client asked to change," not "re-validate
	// the whole row on every PATCH."
	//
	// Config is loaded lazily — and only when the Jira side is being
	// set to a non-empty value. Clearing either tracker, or setting
	// Linear at all, never reads config: validateTrackerKeys with an
	// empty Jira input doesn't consult cfg, and Linear validation
	// rejects non-empty regardless of cfg. This keeps a malformed
	// config.yaml from 500-ing requests like `{"linear_project_key":""}`
	// that have nothing to do with Jira.
	if req.JiraProjectKey != nil {
		jiraInput := strings.TrimSpace(*req.JiraProjectKey)
		if jiraInput == "" {
			updated.JiraProjectKey = ""
		} else {
			cfg, err := config.Load()
			if err != nil {
				log.Printf("[projects] patch: config load: %v", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load config"})
				return
			}
			jiraKey, _, errMsg := validateTrackerKeys(cfg, *req.JiraProjectKey, "")
			if errMsg != "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
				return
			}
			updated.JiraProjectKey = jiraKey
		}
	}
	if req.LinearProjectKey != nil {
		// Linear is rejected if non-empty regardless of config; the
		// empty-cfg argument below makes the no-cfg-needed contract
		// explicit. When the Linear integration ships, this branch
		// will need a config.Load() of its own — but only when the
		// input is non-empty, mirroring the Jira pattern above.
		_, linearKey, errMsg := validateTrackerKeys(config.Config{}, "", *req.LinearProjectKey)
		if errMsg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
			return
		}
		updated.LinearProjectKey = linearKey
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

	// Take the same per-project lock that PATCH uses. Without this,
	// an in-flight autosave (holding the mutex, mid read-merge-write)
	// races a DELETE: DELETE drops the row out from under PATCH, and
	// PATCH's UPDATE returns sql.ErrNoRows which the handler maps to
	// a 500. With the lock, DELETE waits for PATCH to finish
	// committing, then deletes — and a PATCH that arrives after the
	// DELETE finds the row gone and 404s cleanly.
	mu := s.projectMutex(id)
	mu.Lock()
	defer mu.Unlock()

	// Snapshot pinned_repos BEFORE the cascade fires so we can prune
	// each affected bare clone's worktree registration list after the
	// project's repos/ subtree gets RemoveAll'd. Without the prune,
	// stale entries accumulate in <bare>/worktrees/ — recoverable but
	// noisy, and they block re-creating the same name in a future
	// project's worktree. A read failure here is non-fatal: skip the
	// prune step, the on-disk cleanup still happens.
	var pinned []string
	if existing, err := db.GetProject(s.db, id); err == nil && existing != nil {
		pinned = existing.PinnedRepos
	}

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

	// Prune the bare clone of each pinned repo — the per-project
	// worktrees we just RemoveAll'd would otherwise leave behind
	// dangling entries in <bare>/worktrees/ that block re-creating
	// the same name in a future project. Best-effort, post-RemoveAll
	// because prune is what reads the now-missing dirs.
	for _, slug := range pinned {
		owner, repo, ok := splitOwnerRepo(slug)
		if !ok {
			continue
		}
		worktree.PruneCuratorBare(owner, repo)
	}

	w.WriteHeader(http.StatusNoContent)
}

// splitOwnerRepo splits "owner/repo" once. Mirrors the helper in
// internal/curator; duplicated here rather than imported to avoid
// pulling the curator package's surface into the projects handler.
func splitOwnerRepo(slug string) (owner, repo string, ok bool) {
	for i := 0; i < len(slug); i++ {
		if slug[i] == '/' {
			if i == 0 || i == len(slug)-1 {
				return "", "", false
			}
			return slug[:i], slug[i+1:], true
		}
	}
	return "", "", false
}

// validatePinnedRepoShape checks the "owner/repo" slug format and
// returns the normalized (trimmed) slice. Pure — does not touch the
// DB — so it stays cheap to test in isolation and stays usable in
// any future code path that just needs to canonicalize slug input.
//
// The trim-then-persist step matters: without it, " owner/repo "
// would pass validation (the validator trims internally) but get
// stored padded, breaking subsequent lookups by slug.
//
// Returns (normalized, "") on success and (nil, errMsg) on failure.
func validatePinnedRepoShape(repos []string) ([]string, string) {
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

// validatePinnedRepos composes shape validation with the must-be-
// configured existence check: every slug must correspond to a row in
// repo_profiles. This pins the UX contract — the frontend (SKY-217)
// presents pinned_repos as a multi-select over the configured-repos
// list, so an unconfigured slug arriving here is either a stale
// client (the user removed the repo from config after pinning) or
// someone hand-crafting a curl. Rejecting it up front keeps the
// Curator from later trying to materialize a worktree for a repo
// the user can't authenticate against.
//
// Returns (normalized, "") on success and (nil, errMsg) on failure.
func validatePinnedRepos(database *sql.DB, repos []string) ([]string, string) {
	out, errMsg := validatePinnedRepoShape(repos)
	if errMsg != "" {
		return nil, errMsg
	}
	if len(out) == 0 {
		return out, ""
	}

	configured, err := db.GetConfiguredRepoNames(database)
	if err != nil {
		return nil, "failed to load configured repos: " + err.Error()
	}
	known := make(map[string]struct{}, len(configured))
	for _, name := range configured {
		known[name] = struct{}{}
	}
	for _, slug := range out {
		if _, ok := known[slug]; !ok {
			return nil, "pinned_repos: " + slug + " is not a configured repo (add it on the GitHub config page first)"
		}
	}
	return out, ""
}

// validateTrackerKeys validates jira_project_key and linear_project_key
// independently. Each is optional; when non-empty, jira_project_key
// must be present in cfg.Jira.Projects (the user-curated list set up
// in Settings) and linear_project_key is rejected outright until the
// Linear integration ships. Both fields are normalized via
// strings.TrimSpace before the existence check so a value padded with
// stray whitespace doesn't pass validation but get stored unmatched.
//
// Takes cfg as a parameter rather than calling config.Load() directly
// so the function is testable in isolation and so a single PATCH/POST
// only reads the config file once even when both fields need
// validating.
//
// Returns the normalized values and an empty error string on success,
// or two empty strings and an error message on failure.
func validateTrackerKeys(cfg config.Config, jiraKey, linearKey string) (string, string, string) {
	jiraNorm := strings.TrimSpace(jiraKey)
	linearNorm := strings.TrimSpace(linearKey)

	if linearNorm != "" {
		// Linear integration is future work. Surfacing this as an
		// explicit "not configured" error matches the UX contract:
		// the frontend renders a disabled Linear picker, so a
		// non-empty value arriving here is a bypass attempt or a
		// stale client.
		return "", "", "linear_project_key: Linear integration is not configured"
	}

	if jiraNorm == "" {
		return "", "", ""
	}
	for _, p := range cfg.Jira.Projects {
		if p == jiraNorm {
			return jiraNorm, "", ""
		}
	}
	return "", "", "jira_project_key: " + jiraNorm + " is not in the configured Jira projects list (add it on the Settings page first)"
}

// knowledgeFile is the per-file shape returned by the knowledge
// endpoint. We surface the relative path (under
// <KnowledgeDir>/knowledge-base/) rather than the absolute path so
// the API doesn't leak the user's home directory layout.
//
// Content is inlined ONLY for text-shaped files under
// knowledgeInlineMaxBytes — markdown, plain text, json/yaml/etc. that
// the panel renders inline. Non-text files (images, PDFs, archives)
// and large text files leave Content empty; the frontend fetches
// them on-demand from the per-file raw endpoint when a preview is
// actually needed. Two reasons:
//   - Image bytes don't survive a JSON round-trip without base64
//     encoding, which inflates the listing payload an order of
//     magnitude even when the user never expands the file.
//   - The list endpoint runs on every panel mount; inlining a
//     50MB upload would block that response on disk reads the user
//     hasn't asked for yet.
//
// MimeType drives the frontend's render switch (markdown / image /
// text / no-preview). Detected from the file extension first
// (mime.TypeByExtension) and falls back to "application/octet-stream"
// for unknown types.
type knowledgeFile struct {
	Path      string `json:"path"`
	MimeType  string `json:"mime_type"`
	Content   string `json:"content"`
	UpdatedAt string `json:"updated_at"`
	SizeBytes int64  `json:"size_bytes"`
}

// knowledgeInlineMaxBytes caps the size of content we inline in the
// list response. Above this, even text files get fetched lazily from
// the raw endpoint when expanded. 256KB covers ~25k lines of code
// or ~50 pages of prose — well past anything a knowledge file
// reasonably needs to be, and small enough that loading the listing
// stays snappy.
const knowledgeInlineMaxBytes = 256 * 1024

// knowledgeMaxUploadBytes caps individual file uploads at 5MB. The
// Curator's agent has to read these into context at some point, so
// extremely large files are the wrong shape for a knowledge base.
// Multi-file uploads are bounded by knowledgeMaxRequestBytes below.
const knowledgeMaxUploadBytes = 5 * 1024 * 1024

// knowledgeMaxRequestBytes caps a single multipart request at 25MB
// total. Big enough for a handful of images or PDFs at the per-file
// limit, small enough that a malicious or accidental "drag in 500
// files" doesn't lock up the process on disk + memory.
const knowledgeMaxRequestBytes = 25 * 1024 * 1024

// handleProjectKnowledge serves the files under the project's
// knowledge-base directory. SKY-217.
//
// Returns an empty list (not 404) when the project exists but the
// knowledge subdir doesn't, so the frontend can render an empty state
// instead of a noisy error. A real I/O failure (permission denied,
// etc.) is a 500 — but the response body is a generic message: the
// underlying os.* errors include absolute paths under the user's
// home dir, which would otherwise leak filesystem layout to the
// browser. Detail goes to the server log where the operator can find
// it; the client gets a stable string.
func (s *Server) handleProjectKnowledge(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	project, err := db.GetProject(s.db, id)
	if err != nil {
		log.Printf("[projects] knowledge list: db get %s: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load project"})
		return
	}
	if project == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	root, err := curator.KnowledgeDir(id)
	if err != nil {
		log.Printf("[projects] knowledge list: resolve dir for %s: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read knowledge base"})
		return
	}
	kbDir := filepath.Join(root, "knowledge-base")
	files, err := readKnowledgeFiles(kbDir)
	if err != nil {
		log.Printf("[projects] knowledge list: read %s: %v", kbDir, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read knowledge base"})
		return
	}
	writeJSON(w, http.StatusOK, files)
}

// readKnowledgeFiles walks one level of the knowledge-base directory
// and returns every regular file in stable order, with mime type
// detected per file and content inlined for text-shaped files under
// knowledgeInlineMaxBytes. Single level by design — the agent is
// supposed to keep a flat layout under knowledge-base/, and recursing
// here would surface scratch state from any nested dirs.
//
// "Doesn't exist" is not an error: a fresh project hasn't had any
// knowledge written yet, so an empty list is the truthful response.
//
// Symlinks and non-regular files are skipped — the agent shouldn't be
// pointing this directory at anything outside it, and reading through
// a symlink could exfiltrate file contents from elsewhere on the
// user's machine via a malicious upload (a `notes.md` symlink to
// ~/.ssh/id_rsa, etc.). Better to skip silently than to follow.
func readKnowledgeFiles(dir string) ([]knowledgeFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []knowledgeFile{}, nil
		}
		return nil, err
	}
	out := make([]knowledgeFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Skip filenames that the per-file endpoints would reject.
		// Otherwise the listing would surface entries the user can't
		// open or delete from the UI — for instance, a leading-dot
		// name written directly by the agent (`.cache.json`) shows
		// up in the list but the DELETE endpoint refuses the path,
		// leaving the user with phantom files. Filtering at list
		// time keeps the visible set actionable.
		if _, errMsg := sanitizeKnowledgeFilename(name); errMsg != "" {
			continue
		}
		full := filepath.Join(dir, name)
		info, err := os.Lstat(full)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		mimeType := detectMimeType(name)
		entry := knowledgeFile{
			Path:      name,
			MimeType:  mimeType,
			UpdatedAt: info.ModTime().UTC().Format("2006-01-02T15:04:05Z07:00"),
			SizeBytes: info.Size(),
		}
		if isTextMime(mimeType) && info.Size() <= knowledgeInlineMaxBytes {
			// openNoFollow closes the TOCTOU window between the
			// Lstat above and reading the file: an attacker who can
			// swap the path for a symlink in the intervening
			// microseconds would otherwise have its target's bytes
			// inlined into the listing response. Mirrors the same
			// defense the raw-file endpoint uses; on non-unix
			// builds this falls back to plain os.Open (Windows
			// symlinks are admin-only and out of threat model).
			f, err := openNoFollow(full)
			if err != nil {
				// If the open is a symlink rejection, skip the
				// file rather than aborting the whole listing —
				// the listing has to keep working for unrelated
				// files even if a malicious one slips in.
				if isSymlinkRejection(err) {
					continue
				}
				return nil, err
			}
			body, err := io.ReadAll(io.LimitReader(f, knowledgeInlineMaxBytes+1))
			f.Close()
			if err != nil {
				return nil, err
			}
			// Bound the inline content to the same cap the listing
			// promises — defense in depth in case the file grew
			// between Lstat and read.
			if int64(len(body)) > knowledgeInlineMaxBytes {
				body = body[:knowledgeInlineMaxBytes]
			}
			entry.Content = string(body)
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// detectMimeType maps a filename to a content-type using the
// extension table Go's `mime` package ships. Falls back to
// "application/octet-stream" for unknown extensions rather than
// sniffing — sniffing requires reading the file, which is wasted
// work for the listing path where we already skip non-text.
//
// We special-case .md → text/markdown. Go's stdlib mime table
// includes it on most platforms but not all (depends on the system
// mime.types file), so we pin the value to keep the frontend's
// markdown-vs-other branch stable across machines.
func detectMimeType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

// isTextMime decides whether a content-type's bytes can be rendered
// as a string in the frontend's <pre> or markdown renderer. Anything
// in the text/ family is text by definition; a few application/
// types (JSON, YAML, XML, JS) are also actually-text and worth
// inlining so the panel can show them without an extra fetch.
func isTextMime(mimeType string) bool {
	if i := strings.Index(mimeType, ";"); i >= 0 {
		mimeType = mimeType[:i]
	}
	mimeType = strings.TrimSpace(mimeType)
	if strings.HasPrefix(mimeType, "text/") {
		return true
	}
	switch mimeType {
	case "application/json",
		"application/yaml",
		"application/x-yaml",
		"application/xml",
		"application/javascript",
		"application/typescript",
		"application/toml":
		return true
	}
	return false
}

// sanitizeKnowledgeFilename hardens an uploaded filename before it
// touches the filesystem. The agent is the primary writer here, but
// uploads come from the browser and a malicious or careless filename
// shouldn't be able to escape the knowledge-base/ subdir or shadow
// system files.
//
// Rules:
//   - Strip any path component via filepath.Base (platform-aware:
//     uses '/' on Unix, '/' or '\\' on Windows).
//   - Reject empty results, "..", "." segments.
//   - Reject leading dot (no hidden files — the agent's own scratch
//     state lives at the project root with leading dots, and we don't
//     want uploads colliding with it).
//   - Reject the OS path separator post-Base (defense in depth).
//
// We deliberately don't normalize backslash to slash before Base: on
// Unix '\\' is a legitimate filename character (e.g. `a\b.md` is one
// file, not two path components), and the manual replace would have
// turned that filename into `b.md` for upload but left it alone in
// the listing — leaving the user with a listing entry that the raw
// and delete endpoints can't resolve. Browsers strip path components
// from multipart filenames before sending them, so the cross-platform
// case the manual replace was guarding against doesn't actually arise
// in practice.
//
// Returns the sanitized base name and an error message string for
// the handler to surface as a 400.
func sanitizeKnowledgeFilename(raw string) (string, string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "filename is required"
	}
	base := filepath.Base(trimmed)
	if base == "." || base == ".." || base == "/" || base == "" {
		return "", "filename is invalid"
	}
	if strings.HasPrefix(base, ".") {
		return "", "filename cannot start with a dot"
	}
	// Reject only the OS's actual path separator — '\\' is a literal
	// character on Unix, not a separator, and stripping it from a
	// legitimate filename would desync the listing from the per-file
	// endpoints.
	if strings.ContainsRune(base, filepath.Separator) {
		return "", "filename cannot contain path separators"
	}
	return base, ""
}

// resolveKnowledgePath maps a URL path parameter to an absolute file
// path inside the project's knowledge-base directory and validates
// that the resolved path stays within. Two layers of defense:
//  1. URL-decode + sanitize via sanitizeKnowledgeFilename, which
//     rejects "..", path separators, and leading dots.
//  2. Resolve to absolute via filepath.Join(kbDir, name) and re-check
//     the result has kbDir as its prefix. Belt-and-suspenders against
//     anything the sanitizer might miss on a future filesystem.
//
// Errors that include filesystem detail (KnowledgeDir's UserHomeDir
// failure, etc.) are logged server-side; the returned message is
// generic so the response body doesn't leak path layout to the
// browser.
//
// Returns (kbDir, fullPath, status, errMsg). status is 0 on success,
// http.StatusBadRequest for client-side mistakes (bad input, path
// traversal), and http.StatusInternalServerError for server-side
// failures (UserHomeDir, etc.). Earlier versions collapsed both
// cases to a 400, which misclassified internal failures as bad
// input and made operator triage harder.
func resolveKnowledgePath(projectID, rawPath string) (string, string, int, string) {
	root, err := curator.KnowledgeDir(projectID)
	if err != nil {
		log.Printf("[projects] resolve knowledge dir for %s: %v", projectID, err)
		return "", "", http.StatusInternalServerError, "failed to resolve knowledge dir"
	}
	kbDir := filepath.Join(root, "knowledge-base")
	decoded, err := url.PathUnescape(rawPath)
	if err != nil {
		return "", "", http.StatusBadRequest, "invalid path encoding"
	}
	name, errMsg := sanitizeKnowledgeFilename(decoded)
	if errMsg != "" {
		return "", "", http.StatusBadRequest, errMsg
	}
	full := filepath.Join(kbDir, name)
	rel, err := filepath.Rel(kbDir, full)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return "", "", http.StatusBadRequest, "path escapes knowledge-base directory"
	}
	return kbDir, full, 0, ""
}

// handleProjectKnowledgeFile streams the raw bytes of a single
// knowledge file. Used by the frontend for image <img src=> tags,
// preview-not-available "Open" links, and lazy-fetch of large text
// files. Sets Content-Disposition: inline so the browser previews
// rather than downloads — this is a sidebar viewer, not a download
// hub. Users who want to save a file can use Save As from there.
//
// Symlink defense: Lstat (not Stat) gates regular-file admission.
// Stat follows symlinks and would happily green-light a symlink
// whose target is a regular file outside the knowledge-base dir,
// leaking arbitrary file contents through this endpoint. Also
// switches from http.ServeFile (which re-opens the path and could
// be raced) to http.ServeContent against an already-open file
// handle so the file we serve is the one we just verified.
func (s *Server) handleProjectKnowledgeFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	project, err := db.GetProject(s.db, id)
	if err != nil {
		log.Printf("[projects] knowledge fetch: db get %s: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load project"})
		return
	}
	if project == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	_, full, status, errMsg := resolveKnowledgePath(id, r.PathValue("path"))
	if errMsg != "" {
		writeJSON(w, status, map[string]string{"error": errMsg})
		return
	}
	linfo, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found"})
			return
		}
		log.Printf("[projects] knowledge fetch: lstat %s: %v", full, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read file"})
		return
	}
	if linfo.Mode()&os.ModeSymlink != 0 || !linfo.Mode().IsRegular() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a regular file"})
		return
	}
	// O_NOFOLLOW closes the TOCTOU window between Lstat and Open:
	// even if the path was swapped for a symlink in the microseconds
	// between, the kernel refuses to traverse it. Without this, the
	// Lstat check above would be advisory rather than authoritative.
	// See open_nofollow_unix.go / open_nofollow_other.go for the
	// platform split.
	f, err := openNoFollow(full)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found"})
			return
		}
		// Distinguish ELOOP (symlink rejected by O_NOFOLLOW) from
		// other openNoFollow failures. Permission denied, EIO, and
		// similar are server-side problems that should surface as
		// 500 so the operator can spot them — collapsing all
		// failures to "not a regular file" 400 would mask real
		// production issues behind a misleading client error.
		log.Printf("[projects] knowledge fetch: open %s: %v", full, err)
		if isSymlinkRejection(err) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a regular file"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read file"})
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", detectMimeType(filepath.Base(full)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "inline; filename="+strconv.Quote(filepath.Base(full)))
	http.ServeContent(w, r, filepath.Base(full), linfo.ModTime(), f)
}

// handleProjectKnowledgeUpload accepts one or more files via
// multipart/form-data and writes each to the project's knowledge-base
// directory. The dir is created lazily on first upload so a project
// that's never been uploaded to (and never been chatted with) doesn't
// accumulate empty directories.
//
// Conflict policy: REJECT. If a file with the same sanitized name
// already exists, that file is reported as a conflict in the per-file
// results — the user explicitly picked "delete the old one first"
// over auto-rename or overwrite. Implementation uses O_CREATE|O_EXCL
// so the check + write is atomic against a concurrent upload of the
// same name.
//
// Per-file size limit and total request limit are enforced during
// upload handling. If a file exceeds knowledgeMaxUploadBytes while
// streaming, that file is reported as a failed per-file result and any
// partially-written file is removed.
//
// Multi-file requests are partial-success: each file is processed
// independently, and the response is HTTP 200 with per-file results.
// A duplicate filename or oversize-file failure doesn't block
// siblings from succeeding.
func (s *Server) handleProjectKnowledgeUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Take the same per-project lock that PATCH and DELETE use.
	// Without it, an upload that has just verified the project
	// exists could race a DELETE: DELETE drops the row and
	// RemoveAll's the project dir, then this handler re-creates
	// knowledge-base/ and writes files into a project that no
	// longer exists in the DB — leaving orphaned on-disk state
	// after a successful delete. Holding the mutex across the
	// existence check + write makes the upload visibly atomic
	// to a concurrent delete.
	mu := s.projectMutex(id)
	mu.Lock()
	defer mu.Unlock()

	project, err := db.GetProject(s.db, id)
	if err != nil {
		log.Printf("[projects] knowledge upload: db get %s: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load project"})
		return
	}
	if project == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	root, err := curator.KnowledgeDir(id)
	if err != nil {
		log.Printf("[projects] knowledge upload: resolve dir for %s: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to resolve knowledge dir"})
		return
	}
	kbDir := filepath.Join(root, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		log.Printf("[projects] knowledge upload: mkdir %s: %v", kbDir, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create knowledge dir"})
		return
	}

	// MaxBytesReader caps the WHOLE request, including form fields
	// and per-file streams. Per-file caps are enforced separately
	// during the copy below so a single oversize file doesn't poison
	// siblings that were within budget.
	r.Body = http.MaxBytesReader(w, r.Body, knowledgeMaxRequestBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "multipart parse: " + err.Error()})
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			if err := r.MultipartForm.RemoveAll(); err != nil {
				log.Printf("[projects] knowledge upload: cleanup multipart form for %s: %v", id, err)
			}
		}
	}()
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no files in upload (use form field 'file')"})
		return
	}

	type uploadResult struct {
		Path     string `json:"path,omitempty"`
		Original string `json:"original"`
		Error    string `json:"error,omitempty"`
	}
	results := make([]uploadResult, 0, len(files))
	for _, fh := range files {
		original := fh.Filename
		name, errMsg := sanitizeKnowledgeFilename(original)
		if errMsg != "" {
			results = append(results, uploadResult{Original: original, Error: errMsg})
			continue
		}
		if fh.Size > knowledgeMaxUploadBytes {
			results = append(results, uploadResult{
				Original: original,
				Error:    fmt.Sprintf("file exceeds %d-byte limit", knowledgeMaxUploadBytes),
			})
			continue
		}
		full := filepath.Join(kbDir, name)
		if errMsg := writeUploadedFile(fh, full); errMsg != "" {
			results = append(results, uploadResult{Original: original, Error: errMsg})
			continue
		}
		results = append(results, uploadResult{Path: name, Original: original})
	}

	// Flip summary_stale if anything actually landed. Per the
	// schema comment on summary_stale, knowledge-base changes have
	// to mark the project so SKY-220's regenerator picks them up.
	// A pure-failures request (every file rejected for conflict /
	// size / sanitize) leaves the on-disk state unchanged, so we
	// don't need to bump summary_stale in that case — keeping the
	// flip conditional on at-least-one-success avoids a regen
	// trigger for a no-op upload.
	wroteAny := false
	for _, r := range results {
		if r.Error == "" {
			wroteAny = true
			break
		}
	}
	if wroteAny {
		if _, err := s.db.Exec(`UPDATE projects SET summary_stale = TRUE, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, id); err != nil {
			// Log but don't fail the upload — the files are on disk
			// and the user expects the response to reflect that.
			// The stale marker and activity timestamp are hints for
			// follow-on processing / display, not part of the
			// upload's correctness contract.
			log.Printf("[projects] knowledge upload: mark summary_stale/update timestamp for %s: %v", id, err)
		}
	}

	// 207-ish semantics in a 200: each entry carries its own status.
	// We pick 200 here rather than 207 (Multi-Status) because the
	// rest of the API speaks plain JSON, not WebDAV; a single shape
	// keeps client handling consistent across endpoints.
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// writeUploadedFile copies the uploaded file's bytes to the target
// path with O_EXCL semantics — fails fast if a file with the same
// name already exists, instead of overwriting. The exclusive open is
// the actual race-safe enforcement of the "reject conflict" policy:
// a quick os.Stat check beforehand is racy because two uploads can
// see "doesn't exist" and both proceed to write.
//
// On every cleanup path the destination is closed BEFORE os.Remove.
// On Windows os.Remove fails on a still-open file, leaving the
// partial upload on disk — which would also poison the next upload
// of the same name (O_EXCL would still see the orphan and reject).
// Unix tolerates remove-while-open but the explicit-close pattern
// keeps the two platforms behaving the same.
func writeUploadedFile(fh *multipart.FileHeader, dst string) string {
	src, err := fh.Open()
	if err != nil {
		return "open upload: " + err.Error()
	}
	defer src.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return "file already exists — delete it first"
		}
		return "create file: " + err.Error()
	}
	closed := false
	closeOnce := func() {
		if !closed {
			out.Close()
			closed = true
		}
	}
	defer closeOnce()

	// Cap the per-file copy at knowledgeMaxUploadBytes + 1 so we
	// detect oversize uploads even if the multipart header lied
	// about Size. If we hit the cap, remove the partial file —
	// which means we have to close the handle first.
	limited := io.LimitReader(src, knowledgeMaxUploadBytes+1)
	written, err := io.Copy(out, limited)
	if err != nil {
		closeOnce()
		_ = os.Remove(dst)
		return "write file: " + err.Error()
	}
	if written > knowledgeMaxUploadBytes {
		closeOnce()
		_ = os.Remove(dst)
		return fmt.Sprintf("file exceeds %d-byte limit", knowledgeMaxUploadBytes)
	}
	if err := out.Close(); err != nil {
		closed = true
		_ = os.Remove(dst)
		return "close file: " + err.Error()
	}
	closed = true
	return ""
}

// handleProjectKnowledgeDelete removes a single file from the
// knowledge-base directory. The Curator's RemoveAll on project
// delete handles whole-dir cleanup; this is the per-file pair to
// the upload endpoint, mirrored on the frontend by the per-file
// trash button.
func (s *Server) handleProjectKnowledgeDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Same per-project lock as upload + project PATCH/DELETE.
	// Without it, the direct UPDATE summary_stale = TRUE below
	// races a concurrent project PATCH: PATCH reads
	// summary_stale=false, this handler removes the file and
	// flips the flag, then PATCH's UpdateProject writes its
	// pre-edit summary_stale=false back over our flip — the
	// regenerator never picks the change up. Holding the lock
	// makes the file remove + flag flip atomic from the
	// perspective of other writers.
	mu := s.projectMutex(id)
	mu.Lock()
	defer mu.Unlock()

	project, err := db.GetProject(s.db, id)
	if err != nil {
		log.Printf("[projects] knowledge delete: db get %s: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load project"})
		return
	}
	if project == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	_, full, status, errMsg := resolveKnowledgePath(id, r.PathValue("path"))
	if errMsg != "" {
		writeJSON(w, status, map[string]string{"error": errMsg})
		return
	}
	if err := os.Remove(full); err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found"})
			return
		}
		log.Printf("[projects] knowledge delete: remove %s: %v", full, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to remove file"})
		return
	}
	if _, err := s.db.Exec(`UPDATE projects SET summary_stale = TRUE, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, id); err != nil {
		log.Printf("[projects] knowledge delete: mark summary_stale for %s: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update project state"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
