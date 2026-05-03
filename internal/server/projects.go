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
	if jiraKey != "" || linearKey != "" {
		cfg, err := config.Load()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load config: " + err.Error()})
			return
		}
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
			body, err := os.ReadFile(full)
			if err != nil {
				return nil, err
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
//   - Strip any path component (clients sometimes send full paths).
//   - Reject empty results, "..", "." segments.
//   - Reject leading dot (no hidden files — the agent's own scratch
//     state lives at the project root with leading dots, and we don't
//     want uploads colliding with it).
//   - Reject path separators in the final name (defense in depth
//     after the strip — Windows-style backslashes can survive
//     filepath.Base on a Unix host).
//
// Returns the sanitized base name and an error message string for
// the handler to surface as a 400.
func sanitizeKnowledgeFilename(raw string) (string, string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "filename is required"
	}
	// filepath.Base on Unix doesn't strip Windows separators, so do
	// it manually first to handle clients that send "folder\\file.md".
	trimmed = strings.ReplaceAll(trimmed, "\\", "/")
	base := filepath.Base(trimmed)
	if base == "." || base == ".." || base == "/" || base == "" {
		return "", "filename is invalid"
	}
	if strings.HasPrefix(base, ".") {
		return "", "filename cannot start with a dot"
	}
	if strings.ContainsAny(base, "/\\") {
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
// Returns (kbDir, fullPath, "") on success and ("", "", errMsg) on
// failure.
func resolveKnowledgePath(projectID, rawPath string) (string, string, string) {
	root, err := curator.KnowledgeDir(projectID)
	if err != nil {
		log.Printf("[projects] resolve knowledge dir for %s: %v", projectID, err)
		return "", "", "failed to resolve knowledge dir"
	}
	kbDir := filepath.Join(root, "knowledge-base")
	decoded, err := url.PathUnescape(rawPath)
	if err != nil {
		return "", "", "invalid path encoding"
	}
	name, errMsg := sanitizeKnowledgeFilename(decoded)
	if errMsg != "" {
		return "", "", errMsg
	}
	full := filepath.Join(kbDir, name)
	rel, err := filepath.Rel(kbDir, full)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return "", "", "path escapes knowledge-base directory"
	}
	return kbDir, full, ""
}

// handleProjectKnowledgeFile streams the raw bytes of a single
// knowledge file. Used by the frontend for image <img src=> tags,
// preview-not-available "Open" links, and lazy-fetch of large text
// files. Sets Content-Disposition: inline so the browser previews
// rather than downloads — this is a sidebar viewer, not a download
// hub. Users who want to save a file can use Save As from there.
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
	_, full, errMsg := resolveKnowledgePath(id, r.PathValue("path"))
	if errMsg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
		return
	}
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found"})
			return
		}
		log.Printf("[projects] knowledge fetch: stat %s: %v", full, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read file"})
		return
	}
	if !info.Mode().IsRegular() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a regular file"})
		return
	}
	w.Header().Set("Content-Type", detectMimeType(filepath.Base(full)))
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Content-Disposition", "inline; filename="+strconv.Quote(filepath.Base(full)))
	http.ServeFile(w, r, full)
}

// handleProjectKnowledgeUpload accepts one or more files via
// multipart/form-data and writes each to the project's knowledge-base
// directory. The dir is created lazily on first upload so a project
// that's never been uploaded to (and never been chatted with) doesn't
// accumulate empty directories.
//
// Conflict policy: REJECT. If a file with the same sanitized name
// already exists, the upload returns 409 with a message naming the
// conflict — the user explicitly picked "delete the old one first"
// over auto-rename or overwrite. Implementation uses O_CREATE|O_EXCL
// so the check + write is atomic against a concurrent upload of the
// same name.
//
// Per-file size limit and total request limit are enforced via
// http.MaxBytesReader at the wrapper level; a multipart file that
// blows past knowledgeMaxUploadBytes during streaming surfaces as a
// "request body too large" error and the partially-written file is
// removed.
//
// Multi-file requests are partial-success: each file is processed
// independently, and the response includes a per-file outcome. A
// duplicate filename failing doesn't block siblings from succeeding.
func (s *Server) handleProjectKnowledgeUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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
	defer out.Close()

	// Cap the per-file copy at knowledgeMaxUploadBytes + 1 so we
	// detect oversize uploads even if the multipart header lied
	// about Size. If we hit the cap, remove the partial file.
	limited := io.LimitReader(src, knowledgeMaxUploadBytes+1)
	written, err := io.Copy(out, limited)
	if err != nil {
		_ = os.Remove(dst)
		return "write file: " + err.Error()
	}
	if written > knowledgeMaxUploadBytes {
		_ = os.Remove(dst)
		return fmt.Sprintf("file exceeds %d-byte limit", knowledgeMaxUploadBytes)
	}
	return ""
}

// handleProjectKnowledgeDelete removes a single file from the
// knowledge-base directory. The Curator's RemoveAll on project
// delete handles whole-dir cleanup; this is the per-file pair to
// the upload endpoint, mirrored on the frontend by the per-file
// trash button.
func (s *Server) handleProjectKnowledgeDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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
	_, full, errMsg := resolveKnowledgePath(id, r.PathValue("path"))
	if errMsg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
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
	w.WriteHeader(http.StatusNoContent)
}
