package projectbundle

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// GitHubProbe provides the preflight clone URL lookup used by import.
type GitHubProbe interface {
	CloneURLForRepo(ctx context.Context, owner, repo string) (string, error)
}

// Import reads a .tfproject ZIP and materializes it into a new local project.
func Import(
	ctx context.Context,
	database *sql.DB,
	readerAt io.ReaderAt,
	size int64,
	probe GitHubProbe,
) (*domain.Project, []ImportWarning, error) {
	if size <= 0 {
		return nil, nil, errors.New("bundle is empty")
	}
	zr, err := zip.NewReader(readerAt, size)
	if err != nil {
		return nil, nil, fmt.Errorf("open bundle zip: %w", err)
	}
	entries, err := indexZipEntries(zr.File)
	if err != nil {
		return nil, nil, err
	}

	manifest, err := readManifest(entries)
	if err != nil {
		return nil, nil, err
	}
	if err := ensureUniqueProjectName(database, manifest.Project.Name); err != nil {
		return nil, nil, err
	}
	cloneURLs, err := preflightPinnedRepos(ctx, manifest.Project.PinnedRepos, probe)
	if err != nil {
		return nil, nil, err
	}

	sessionEntries, err := listEntriesWithPrefix(entries, sessionPrefix)
	if err != nil {
		return nil, nil, err
	}
	hasSession := len(sessionEntries) > 0
	if hasSession {
		if _, ok := entries[sessionTranscriptPath]; !ok {
			return nil, nil, fmt.Errorf("bundle session is missing %s", sessionTranscriptPath)
		}
		if manifest.Session == nil {
			return nil, nil, errors.New("bundle session exists but manifest.session is missing")
		}
		if strings.TrimSpace(manifest.Session.CuratorSessionID) == "" || strings.TrimSpace(manifest.Session.ResolvedCwd) == "" {
			return nil, nil, errors.New("manifest.session requires curator_session_id and resolved_cwd")
		}
	}

	newProjectID := uuid.New().String()
	newSessionID := ""
	if hasSession {
		newSessionID = uuid.New().String()
	}
	projectRoot, err := curator.KnowledgeDir(newProjectID)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve project root: %w", err)
	}
	kbRoot := filepath.Join(projectRoot, "knowledge-base")

	cleanup := &rollbackTracker{}
	committed := false
	defer func() {
		if !committed {
			cleanup.Cleanup()
		}
	}()

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin import transaction: %w", err)
	}
	defer tx.Rollback()

	if err := insertImportedProject(tx, newProjectID, newSessionID, manifest.Project); err != nil {
		return nil, nil, err
	}

	requestIDMap, err := importCuratorRequests(tx, newProjectID, entries[curatorRequestsPath])
	if err != nil {
		return nil, nil, err
	}
	if err := importCuratorMessages(tx, requestIDMap, entries[curatorMessagesPath]); err != nil {
		return nil, nil, err
	}
	if err := importPendingContext(tx, newProjectID, newSessionID, requestIDMap, entries[curatorPendingContextPath]); err != nil {
		return nil, nil, err
	}
	if err := ensureRepoProfiles(tx, manifest.Project.PinnedRepos, cloneURLs); err != nil {
		return nil, nil, err
	}

	if err := os.MkdirAll(kbRoot, 0o755); err != nil {
		return nil, nil, fmt.Errorf("mkdir knowledge root: %w", err)
	}
	cleanup.Add(projectRoot)
	if err := materializeKnowledge(entries, kbRoot); err != nil {
		return nil, nil, err
	}

	if hasSession {
		if err := materializeSession(entries, manifest.Session, projectRoot, newSessionID, cleanup); err != nil {
			return nil, nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit import transaction: %w", err)
	}
	committed = true

	warnings := clonePinnedRepos(ctx, manifest.Project.PinnedRepos, cloneURLs)
	project, err := db.GetProject(database, newProjectID)
	if err != nil {
		return nil, warnings, fmt.Errorf("load imported project: %w", err)
	}
	if project == nil {
		return nil, warnings, errors.New("import committed but project row is missing")
	}
	return project, warnings, nil
}

type rollbackTracker struct {
	paths map[string]struct{}
}

func (r *rollbackTracker) Add(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	if r.paths == nil {
		r.paths = make(map[string]struct{})
	}
	r.paths[path] = struct{}{}
}

func (r *rollbackTracker) Cleanup() {
	if len(r.paths) == 0 {
		return
	}
	ordered := make([]string, 0, len(r.paths))
	for p := range r.paths {
		ordered = append(ordered, p)
	}
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, p := range ordered {
		_ = os.RemoveAll(p)
	}
}

func readManifest(entries map[string]*zip.File) (*Manifest, error) {
	zf, ok := entries[manifestPath]
	if !ok {
		return nil, ErrManifestMissing
	}
	body, err := readZipFileLimited(zf, maxManifestBytes)
	if err != nil {
		return nil, err
	}
	return decodeManifest(body)
}

func readZipFileLimited(zf *zip.File, maxBytes int64) ([]byte, error) {
	rc, err := zf.Open()
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", zf.Name, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", zf.Name, err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("%s exceeds %d-byte limit", zf.Name, maxBytes)
	}
	return body, nil
}

func ensureUniqueProjectName(database *sql.DB, incoming string) error {
	incoming = strings.TrimSpace(incoming)
	projects, err := db.ListProjects(database)
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	for _, p := range projects {
		if strings.EqualFold(strings.TrimSpace(p.Name), incoming) {
			return &DuplicateNameError{Name: incoming}
		}
	}
	return nil
}

func preflightPinnedRepos(ctx context.Context, pinned []string, probe GitHubProbe) (map[string]string, error) {
	cloneURLs := make(map[string]string, len(pinned))
	missing := make([]MissingRepoError, 0)
	for _, slug := range pinned {
		owner, repo, ok := splitOwnerRepo(slug)
		if !ok {
			return nil, fmt.Errorf("invalid pinned repo slug %q", slug)
		}
		if probe == nil {
			missing = append(missing, MissingRepoError{
				Repo:  slug,
				Error: "GitHub is not configured",
			})
			continue
		}
		cloneURL, err := probe.CloneURLForRepo(ctx, owner, repo)
		if err != nil {
			missing = append(missing, MissingRepoError{
				Repo:  slug,
				Error: err.Error(),
			})
			continue
		}
		if strings.TrimSpace(cloneURL) == "" {
			missing = append(missing, MissingRepoError{
				Repo:  slug,
				Error: "missing clone_url",
			})
			continue
		}
		cloneURLs[slug] = strings.TrimSpace(cloneURL)
	}
	if len(missing) > 0 {
		return nil, &MissingReposError{Missing: missing}
	}
	return cloneURLs, nil
}

func insertImportedProject(tx *sql.Tx, projectID, curatorSessionID string, manifestProject ManifestProject) error {
	pinned := cloneStrings(manifestProject.PinnedRepos)
	if pinned == nil {
		pinned = []string{}
	}
	pinnedJSON, err := json.Marshal(pinned)
	if err != nil {
		return fmt.Errorf("marshal pinned repos: %w", err)
	}
	now := time.Now().UTC()
	_, err = tx.Exec(`
		INSERT INTO projects (
			id, name, description, summary_md, summary_stale,
			curator_session_id, pinned_repos, jira_project_key,
			linear_project_key, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		projectID,
		strings.TrimSpace(manifestProject.Name),
		manifestProject.Description,
		nullIfEmptyString(manifestProject.SummaryMD),
		true,
		nullIfEmptyString(curatorSessionID),
		string(pinnedJSON),
		nullIfEmptyString(manifestProject.JiraProjectKey),
		nullIfEmptyString(manifestProject.LinearProjectKey),
		now,
		now,
	)
	if err != nil {
		return fmt.Errorf("insert imported project: %w", err)
	}
	return nil
}

func importCuratorRequests(tx *sql.Tx, projectID string, zf *zip.File) (map[string]string, error) {
	rows, err := decodeZipJSONLines[domain.CuratorRequest](zf)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", curatorRequestsPath, err)
	}
	idMap := make(map[string]string, len(rows))
	for _, row := range rows {
		oldID := strings.TrimSpace(row.ID)
		if oldID == "" {
			continue
		}
		idMap[oldID] = uuid.New().String()
	}
	for _, row := range rows {
		oldID := strings.TrimSpace(row.ID)
		if oldID == "" {
			continue
		}
		newID := idMap[oldID]
		_, err := tx.Exec(`
			INSERT INTO curator_requests (
				id, project_id, status, user_input, error_msg,
				cost_usd, duration_ms, num_turns, started_at,
				finished_at, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			newID,
			projectID,
			row.Status,
			row.UserInput,
			nullIfEmptyString(row.ErrorMsg),
			row.CostUSD,
			row.DurationMs,
			row.NumTurns,
			nullIfNilTime(row.StartedAt),
			nullIfNilTime(row.FinishedAt),
			row.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("insert curator_request %s: %w", oldID, err)
		}
	}
	return idMap, nil
}

func importCuratorMessages(tx *sql.Tx, requestIDMap map[string]string, zf *zip.File) error {
	rows, err := decodeZipJSONLines[domain.CuratorMessage](zf)
	if err != nil {
		return fmt.Errorf("decode %s: %w", curatorMessagesPath, err)
	}
	for _, row := range rows {
		requestID := requestIDMap[row.RequestID]
		if requestID == "" {
			return fmt.Errorf("curator message references unknown request_id %q", row.RequestID)
		}
		toolCallsJSON, err := marshalNullableJSON(row.ToolCalls)
		if err != nil {
			return fmt.Errorf("marshal tool_calls for request %s: %w", row.RequestID, err)
		}
		metadataJSON, err := marshalNullableJSON(row.Metadata)
		if err != nil {
			return fmt.Errorf("marshal metadata for request %s: %w", row.RequestID, err)
		}
		_, err = tx.Exec(`
			INSERT INTO curator_messages (
				request_id, role, subtype, content, tool_calls, tool_call_id,
				is_error, metadata, model, input_tokens, output_tokens,
				cache_read_tokens, cache_creation_tokens, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			requestID,
			row.Role,
			row.Subtype,
			row.Content,
			toolCallsJSON,
			nullIfEmptyString(row.ToolCallID),
			row.IsError,
			metadataJSON,
			nullIfEmptyString(row.Model),
			nullIfNilInt(row.InputTokens),
			nullIfNilInt(row.OutputTokens),
			nullIfNilInt(row.CacheReadTokens),
			nullIfNilInt(row.CacheCreationTokens),
			row.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("insert curator_message for request %s: %w", row.RequestID, err)
		}
	}
	return nil
}

func importPendingContext(
	tx *sql.Tx,
	projectID string,
	newSessionID string,
	requestIDMap map[string]string,
	zf *zip.File,
) error {
	rows, err := decodeZipJSONLines[domain.CuratorPendingContext](zf)
	if err != nil {
		return fmt.Errorf("decode %s: %w", curatorPendingContextPath, err)
	}
	if len(rows) == 0 {
		return nil
	}
	if strings.TrimSpace(newSessionID) == "" {
		return errors.New("bundle has pending context rows but no session payload")
	}
	for _, row := range rows {
		var consumedBy any
		if row.ConsumedByRequestID != "" {
			if mapped := requestIDMap[row.ConsumedByRequestID]; mapped != "" {
				consumedBy = mapped
			}
		}
		_, err := tx.Exec(`
			INSERT INTO curator_pending_context (
				project_id, curator_session_id, change_type, baseline_value,
				consumed_at, consumed_by_request_id, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?)
		`,
			projectID,
			newSessionID,
			row.ChangeType,
			row.BaselineValue,
			nullIfNilTime(row.ConsumedAt),
			consumedBy,
			row.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("insert pending context row: %w", err)
		}
	}
	return nil
}

func ensureRepoProfiles(tx *sql.Tx, pinned []string, cloneURLs map[string]string) error {
	for _, slug := range pinned {
		owner, repo, ok := splitOwnerRepo(slug)
		if !ok {
			return fmt.Errorf("invalid pinned repo slug %q", slug)
		}
		cloneURL := cloneURLs[slug]
		if cloneURL == "" {
			continue
		}
		_, err := tx.Exec(`
			INSERT INTO repo_profiles (id, owner, repo, clone_url, updated_at)
			VALUES (?, ?, ?, ?, datetime('now'))
			ON CONFLICT(id) DO UPDATE SET
				clone_url = CASE
					WHEN repo_profiles.clone_url IS NULL OR repo_profiles.clone_url = ''
					THEN excluded.clone_url
					ELSE repo_profiles.clone_url
				END,
				updated_at = datetime('now')
		`, slug, owner, repo, cloneURL)
		if err != nil {
			return fmt.Errorf("upsert repo profile %s: %w", slug, err)
		}
	}
	return nil
}

func materializeKnowledge(entries map[string]*zip.File, kbRoot string) error {
	kbEntries, err := listEntriesWithPrefix(entries, knowledgePrefix)
	if err != nil {
		return err
	}
	for _, e := range kbEntries {
		rel, err := safeBundleRel(e.Name, knowledgePrefix)
		if err != nil {
			return err
		}
		dest := filepath.Join(kbRoot, filepath.FromSlash(rel))
		if err := ensureUnderRoot(kbRoot, dest); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("mkdir knowledge parent for %s: %w", dest, err)
		}
		if err := copyZipEntryRaw(e.File, dest, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func materializeSession(
	entries map[string]*zip.File,
	manifestSession *ManifestSession,
	projectRoot string,
	newSessionID string,
	cleanup *rollbackTracker,
) error {
	newResolvedCwd := worktree.ResolveClaudeProjectCwd(projectRoot)
	newEncoded := worktree.EncodeClaudeProjectDir(newResolvedCwd)
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir for session import: %w", err)
	}
	encodedRoot := filepath.Join(home, ".claude", "projects", newEncoded)
	if err := os.MkdirAll(encodedRoot, 0o700); err != nil {
		return fmt.Errorf("mkdir claude project root: %w", err)
	}

	sessionTreeRoot := filepath.Join(encodedRoot, newSessionID)
	transcriptDest := filepath.Join(encodedRoot, newSessionID+".jsonl")
	cleanup.Add(sessionTreeRoot)
	cleanup.Add(transcriptDest)

	reps := buildSessionReplacements(
		manifestSession.CuratorSessionID,
		newSessionID,
		manifestSession.ResolvedCwd,
		newResolvedCwd,
	)

	transcript, ok := entries[sessionTranscriptPath]
	if !ok {
		return fmt.Errorf("session is missing %s", sessionTranscriptPath)
	}
	if err := copyZipEntryRewritten(transcript, transcriptDest, reps, 0o600); err != nil {
		return err
	}

	subagentEntries, err := listEntriesWithPrefix(entries, sessionSubagentsPrefix)
	if err != nil {
		return err
	}
	for _, e := range subagentEntries {
		rel, err := safeBundleRel(e.Name, sessionSubagentsPrefix)
		if err != nil {
			return err
		}
		dest := filepath.Join(sessionTreeRoot, "subagents", filepath.FromSlash(rel))
		if err := ensureUnderRoot(filepath.Join(sessionTreeRoot, "subagents"), dest); err != nil {
			return err
		}
		if err := copyZipEntryRewritten(e.File, dest, reps, 0o600); err != nil {
			return err
		}
	}

	toolEntries, err := listEntriesWithPrefix(entries, sessionToolResultsPrefix)
	if err != nil {
		return err
	}
	for _, e := range toolEntries {
		rel, err := safeBundleRel(e.Name, sessionToolResultsPrefix)
		if err != nil {
			return err
		}
		dest := filepath.Join(sessionTreeRoot, "tool-results", filepath.FromSlash(rel))
		if err := ensureUnderRoot(filepath.Join(sessionTreeRoot, "tool-results"), dest); err != nil {
			return err
		}
		if err := copyZipEntryRewritten(e.File, dest, reps, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func clonePinnedRepos(ctx context.Context, pinned []string, cloneURLs map[string]string) []ImportWarning {
	warnings := make([]ImportWarning, 0)
	for _, slug := range pinned {
		owner, repo, ok := splitOwnerRepo(slug)
		if !ok {
			warnings = append(warnings, ImportWarning{
				Code:    "invalid_repo_slug",
				Repo:    slug,
				Message: "invalid owner/repo slug",
			})
			continue
		}
		cloneURL := cloneURLs[slug]
		if cloneURL == "" {
			continue
		}
		if _, err := worktree.EnsureBareClone(ctx, owner, repo, cloneURL); err != nil {
			warnings = append(warnings, ImportWarning{
				Code:    "clone_failed",
				Repo:    slug,
				Message: err.Error(),
			})
		}
	}
	return warnings
}

type namedZipFile struct {
	Name string
	File *zip.File
}

func indexZipEntries(files []*zip.File) (map[string]*zip.File, error) {
	out := make(map[string]*zip.File, len(files))
	for _, zf := range files {
		if zf.FileInfo().IsDir() {
			continue
		}
		name := strings.TrimPrefix(zf.Name, "/")
		if strings.Contains(name, "\\") {
			return nil, fmt.Errorf("invalid bundle path %q", zf.Name)
		}
		clean := path.Clean(name)
		if clean == "." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("invalid bundle path %q", zf.Name)
		}
		out[clean] = zf
	}
	return out, nil
}

func listEntriesWithPrefix(entries map[string]*zip.File, prefix string) ([]namedZipFile, error) {
	out := make([]namedZipFile, 0)
	for name, zf := range entries {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if _, err := safeBundleRel(name, prefix); err != nil {
			return nil, err
		}
		out = append(out, namedZipFile{Name: name, File: zf})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func safeBundleRel(name, prefix string) (string, error) {
	if !strings.HasPrefix(name, prefix) {
		return "", fmt.Errorf("bundle path %q does not start with %q", name, prefix)
	}
	rel := strings.TrimPrefix(name, prefix)
	rel = path.Clean(rel)
	if rel == "." || rel == "" {
		return "", fmt.Errorf("bundle path %q has no relative component", name)
	}
	if strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("bundle path %q escapes its prefix", name)
	}
	return rel, nil
}

func ensureUnderRoot(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("resolved path %q escapes root %q", target, root)
	}
	return nil
}

func copyZipEntryRaw(zf *zip.File, dest string, mode os.FileMode) error {
	rc, err := zf.Open()
	if err != nil {
		return fmt.Errorf("open bundle entry %s: %w", zf.Name, err)
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("mkdir parent for %s: %w", dest, err)
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	defer out.Close()
	if _, err := io.Copy(out, rc); err != nil {
		return fmt.Errorf("copy %s to %s: %w", zf.Name, dest, err)
	}
	return nil
}

func copyZipEntryRewritten(zf *zip.File, dest string, reps []byteReplacement, mode os.FileMode) error {
	rc, err := zf.Open()
	if err != nil {
		return fmt.Errorf("open bundle entry %s: %w", zf.Name, err)
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return fmt.Errorf("mkdir parent for %s: %w", dest, err)
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	defer out.Close()
	if err := rewriteToFile(out, rc, reps); err != nil {
		return fmt.Errorf("rewrite %s to %s: %w", zf.Name, dest, err)
	}
	return nil
}

func decodeZipJSONLines[T any](zf *zip.File) ([]T, error) {
	if zf == nil {
		return []T{}, nil
	}
	rc, err := zf.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return readJSONLines[T](rc)
}

func splitOwnerRepo(slug string) (owner, repo string, ok bool) {
	parts := strings.Split(slug, "/")
	if len(parts) != 2 {
		return "", "", false
	}
	owner = strings.TrimSpace(parts[0])
	repo = strings.TrimSpace(parts[1])
	if owner == "" || repo == "" {
		return "", "", false
	}
	return owner, repo, true
}

func marshalNullableJSON(v any) (any, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case []domain.ToolCall:
		if len(t) == 0 {
			return nil, nil
		}
	case map[string]any:
		if len(t) == 0 {
			return nil, nil
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func nullIfEmptyString(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

func nullIfNilInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullIfNilTime(v *time.Time) any {
	if v == nil {
		return nil
	}
	return *v
}
