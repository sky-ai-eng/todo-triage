package gh

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/todo-triage/internal/github"
)

// Size caps for GitHub Actions log archive downloads.
//
// maxArchiveBytes is the total compressed download cap — above this, refuse
// to download at all. 500 MB is deliberately generous; realistic log archives
// are a few MB at most, and a PR with 500 MB of CI logs has bigger problems
// than this command. Pathological size is the exception we're guarding, not
// the rule.
//
// maxPerFileBytes is a second layer against zip bombs — even if the total
// archive is under the cap, reject any single entry that decompresses to
// more than this. 100 MB per file is absurdly generous for log files but
// low enough to bound damage from a decompression attack.
const (
	maxArchiveBytes int64 = 500 * 1024 * 1024 // 500 MB
	maxPerFileBytes int64 = 100 * 1024 * 1024 // 100 MB
)

// handleActions dispatches gh actions subcommands.
func handleActions(client *github.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: todotriage exec gh actions <action> [flags]")
	}

	action := args[0]
	flags := args[1:]

	switch action {
	case "download-logs":
		actionsDownloadLogs(client, flags)
	default:
		exitErr(fmt.Sprintf("unknown actions action: %s", action))
	}
}

// downloadLogsResult is the JSON envelope emitted on success by
// `gh actions download-logs`. Structured output matches the `exec gh`
// contract that every command produces JSON on stdout, so downstream
// tooling (agents, jq, shell pipelines) can parse without scraping
// human-readable text.
type downloadLogsResult struct {
	RunID           int64    `json:"run_id"`
	Owner           string   `json:"owner"`
	Repo            string   `json:"repo"`
	DestDir         string   `json:"dest_dir"`
	BytesDownloaded int64    `json:"bytes_downloaded"`
	Entries         []string `json:"entries"`
}

// actionsDownloadLogs implements `gh actions download-logs <run_id>`.
//
// Fetches the full log archive for a workflow run, extracts it into
// <cwd>/_scratch/ci-logs/<run_id>/, and prints a structured JSON result
// on stdout so agents can parse it the same way they parse every other
// exec gh command. Errors go to stderr with a non-zero exit.
//
// The resource-owning work (temp file + destination directory) lives in
// downloadAndExtractLogs so defers actually fire on error — exitErr calls
// os.Exit, which skips defers, so inlining the logic here would leak the
// temp zip (and leave a half-extracted destDir) on every failure path.
func actionsDownloadLogs(client *github.Client, args []string) {
	owner, repo, err := resolveRepo(flagVal(args, "--repo"))
	if err != nil {
		exitErr(err.Error())
	}

	runIDStr := firstPositional(args)
	if runIDStr == "" {
		exitErr("usage: todotriage exec gh actions download-logs <run_id> [--repo owner/repo]")
	}
	runID, err := strconv.ParseInt(runIDStr, 10, 64)
	if err != nil || runID <= 0 {
		exitErr(fmt.Sprintf("invalid run_id %q: expected a positive integer", runIDStr))
	}

	// Destination: <cwd>/_scratch/ci-logs/<run_id>/. Resolving to absolute
	// so the success output gives the agent a path it can use directly
	// without needing to reason about cwd. safeDestDirForRun also walks
	// each path component to reject pre-existing symlinks — otherwise a
	// symlinked `_scratch` (accidental or malicious) would let our
	// RemoveAll / MkdirAll / zip extraction escape the working directory.
	cwd, err := os.Getwd()
	if err != nil {
		exitErr(fmt.Sprintf("resolve cwd: %v", err))
	}
	destDir, err := safeDestDirForRun(cwd, runID)
	if err != nil {
		exitErr(err.Error())
	}

	bytesDownloaded, err := downloadAndExtractLogs(client, owner, repo, runID, destDir)
	if err != nil {
		exitErr(err.Error())
	}

	// Top-level directory listing so the agent can see which jobs are
	// available without a separate tool call. Kept outside the transactional
	// inner function because a listing failure on a successfully-extracted
	// destDir shouldn't roll back the extraction — the bytes are good even
	// if we can't enumerate them.
	entries, err := topLevelEntries(destDir)
	if err != nil {
		exitErr(fmt.Sprintf("list extracted entries: %v", err))
	}
	if entries == nil {
		entries = []string{} // JSON null would be misleading; empty archive is an empty list
	}

	printJSON(downloadLogsResult{
		RunID:           runID,
		Owner:           owner,
		Repo:            repo,
		DestDir:         destDir,
		BytesDownloaded: bytesDownloaded,
		Entries:         entries,
	})
}

// downloadAndExtractLogs owns every on-disk resource the command needs: the
// destination directory and the temp zip file. It's split out from
// actionsDownloadLogs specifically so that defers actually run on failure —
// the outer function uses exitErr (→ os.Exit), which skips defers, so any
// cleanup logic has to live under a function that can return an error.
//
// Cleanup semantics:
//   - Temp zip: always removed on return, success or failure.
//   - destDir:  kept on success, removed on any failure, so a partial
//     extraction never leaves stale files around that could look like a
//     valid state to the next retry.
//
// Returns the number of bytes downloaded on success.
func downloadAndExtractLogs(client *github.Client, owner, repo string, runID int64, destDir string) (int64, error) {
	// Clobber any previous extraction for the same run_id. The command owns
	// this directory completely (<cwd>/_scratch/ci-logs/<run_id>), so a
	// re-run should produce a clean state — otherwise stale entries from an
	// older extraction (jobs that no longer exist, renamed matrix legs)
	// would sit alongside the current run's files and mislead the agent
	// reading them back.
	if err := os.RemoveAll(destDir); err != nil {
		return 0, fmt.Errorf("clear stale destination directory: %w", err)
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return 0, fmt.Errorf("create destination directory: %w", err)
	}
	// Track success so we can roll back destDir on any failure path. The
	// closure captures this by reference — setting success=true at the end
	// of the happy path is what disarms the cleanup.
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(destDir)
		}
	}()

	// Stream the archive to a temp file. We can't extract from a stream
	// because archive/zip needs ReaderAt for the central directory at the
	// end of the file.
	tmpFile, err := os.CreateTemp("", "todotriage-ci-logs-*.zip")
	if err != nil {
		return 0, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close() // idempotent-safe; second close just returns ErrClosed
		_ = os.Remove(tmpPath)
	}()

	// No explicit ctx deadline: DownloadArtifact enforces its own
	// per-request timeout via the shallow-copied http.Client it runs
	// against. Setting another WithTimeout here would just duplicate
	// the same magic number (and let the two drift in a future edit).
	// Using context.Background keeps the door open for signal-driven
	// cancellation without committing to a specific wall-clock ceiling.
	ctx := context.Background()

	apiPath := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/logs", owner, repo, runID)
	bytesDownloaded, err := client.DownloadArtifact(ctx, apiPath, tmpFile, maxArchiveBytes)
	if err != nil {
		return 0, fmt.Errorf("download logs: %w", err)
	}
	// Close before reopening for read — archive/zip needs exclusive read
	// access and goes through the filesystem path, not our handle. The
	// deferred Close above will no-op on this already-closed file.
	if err := tmpFile.Close(); err != nil {
		return 0, fmt.Errorf("close temp file: %w", err)
	}

	if err := extractZip(tmpPath, destDir, maxPerFileBytes); err != nil {
		return 0, fmt.Errorf("extract archive: %w", err)
	}

	success = true
	return bytesDownloaded, nil
}

// safeDestDirForRun resolves the <cwd>/_scratch/ci-logs/<run_id> destination
// path for a given workflow run, with a symlink safety check at every
// component we walk through. If any component under cwd exists as a
// symlink, the function refuses — otherwise our RemoveAll, MkdirAll, and
// zip extraction would all follow the link and could touch paths outside
// the working directory.
//
// Components that don't exist yet are fine (we'll create them ourselves).
// Components that exist as regular directories are also fine. Only
// pre-existing symlinks are rejected.
//
// There's an inherent TOCTOU race between this check and the subsequent
// filesystem operations, but for our threat model (accidental pre-existing
// symlinks in a worktree, not a live attacker with filesystem primitives)
// this is sufficient. Defending against a live race would require *at
// syscalls with O_NOFOLLOW on every path component, which is overkill.
func safeDestDirForRun(cwd string, runID int64) (string, error) {
	components := []string{"_scratch", "ci-logs", strconv.FormatInt(runID, 10)}
	current := cwd
	for _, c := range components {
		current = filepath.Join(current, c)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				// Nothing past this point exists yet — the rest of the
				// path will be created by MkdirAll, so there's nothing
				// to symlink-check.
				break
			}
			return "", fmt.Errorf("stat path component %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing to use symlinked path component %s (resolves outside the worktree)", current)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("path component %s exists but is not a directory", current)
		}
	}
	return filepath.Join(cwd, "_scratch", "ci-logs", strconv.FormatInt(runID, 10)), nil
}

// extractZip safely extracts zipPath into destDir. Rejects any entry whose
// resolved destination escapes destDir (path-traversal guard) and any entry
// whose uncompressed size exceeds maxFileBytes (zip-bomb guard). The cap is
// parameterized so tests can exercise the guard without writing 100 MB of
// real content; production callers pass maxPerFileBytes.
//
// maxFileBytes must be positive. There is no "unlimited" mode — callers
// that want effectively no cap should pass math.MaxInt64 explicitly rather
// than relying on a sentinel. Non-positive values are a programmer error
// and return immediately, because silently treating them as "no cap" once
// led to a subtle bug where the runtime io.LimitReader path turned into
// "reject everything" for negative inputs.
//
// destDir must already exist. Returns the first error encountered, which
// aborts the rest of the extraction — partial extraction left on disk is
// expected and fine, the caller (downloadLogs) fails the whole command.
func extractZip(zipPath, destDir string, maxFileBytes int64) error {
	if maxFileBytes <= 0 {
		return fmt.Errorf("extractZip: maxFileBytes must be positive, got %d", maxFileBytes)
	}

	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer reader.Close()

	// Canonicalize destDir once so prefix checks are against a stable value.
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("resolve destination abs path: %w", err)
	}
	// Trailing separator so "/tmp/foo" doesn't prefix-match "/tmp/foobar".
	absDestWithSep := absDest + string(os.PathSeparator)

	for _, entry := range reader.File {
		if err := extractZipEntry(entry, absDest, absDestWithSep, maxFileBytes); err != nil {
			return err
		}
	}
	return nil
}

// extractZipEntry writes a single zip entry to disk with all safety checks.
// Split out from extractZip so the defers on the entry reader and the
// output file fire per-entry rather than accumulating across the whole
// archive.
func extractZipEntry(entry *zip.File, absDest, absDestWithSep string, maxFileBytes int64) error {
	// Path-traversal rejection. filepath.Join + Clean collapses any
	// "../../" segments in the entry name; we then verify the result still
	// lives under destDir. This is the guard that makes arbitrary zip
	// files safe to extract.
	targetPath := filepath.Join(absDest, entry.Name)
	if targetPath != absDest && !strings.HasPrefix(targetPath, absDestWithSep) {
		return fmt.Errorf("unsafe archive entry %q: resolves outside destination", entry.Name)
	}

	if entry.FileInfo().IsDir() {
		return os.MkdirAll(targetPath, 0755)
	}

	// Header size pre-check. This fires on hand-crafted zips with a lying
	// UncompressedSize64 (adversarial case) — standard zip writers overwrite
	// this field with the real size, so for a well-formed zip the header
	// and content agree and this path is effectively a fast rejection.
	// extractZip guarantees maxFileBytes > 0, so the comparison is always
	// meaningful here.
	if int64(entry.UncompressedSize64) > maxFileBytes {
		return fmt.Errorf("archive entry %q exceeds per-file size cap: %d bytes", entry.Name, entry.UncompressedSize64)
	}

	// Ensure parent directory exists — zip archives don't always include
	// explicit directory entries.
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return fmt.Errorf("mkdir parent for %q: %w", entry.Name, err)
	}

	src, err := entry.Open()
	if err != nil {
		return fmt.Errorf("open entry %q: %w", entry.Name, err)
	}
	defer src.Close()

	dst, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("create output file %q: %w", entry.Name, err)
	}
	defer dst.Close()

	// Runtime cap via io.LimitReader — this is the guard that actually
	// fires for real oversized content. +1 so we can detect overflow past
	// the cap rather than silently truncating.
	limited := io.LimitReader(src, maxFileBytes+1)
	n, err := io.Copy(dst, limited)
	if err != nil {
		return fmt.Errorf("write entry %q: %w", entry.Name, err)
	}
	if n > maxFileBytes {
		return fmt.Errorf("archive entry %q exceeded per-file size cap during extraction (content larger than %d bytes)", entry.Name, maxFileBytes)
	}
	return nil
}

// topLevelEntries returns the names of entries directly inside dir, sorted
// alphabetically, with a trailing "/" appended to directories so the output
// distinguishes them from files. Used to print a quick orientation listing
// after extraction.
func topLevelEntries(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}
