package gh

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/agentmeta"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// getDiffShapes fetches the PR diff and returns both representations we
// persist on a pending review: a flat per-file set of commentable lines
// (used for legacy line-only validation) and a per-file list of hunk
// ranges (used to validate that multi-line comments don't straddle two
// hunks — GitHub rejects those with 422 at submit time). Tries the full
// diff first; falls back to per-file patches if GitHub rejects it as
// too large (HTTP 406).
func getDiffShapes(client *ghclient.Client, owner, repo string, number int) (map[string]map[int]bool, map[string][]ghclient.Hunk, error) {
	diff, err := client.GetPRDiff(owner, repo, number, "")
	if err != nil {
		if ghclient.IsHTTP406(err) {
			files, filesErr := client.GetPRFiles(owner, repo, number)
			if filesErr != nil {
				return nil, nil, filesErr
			}
			return ghclient.DiffLinesFromPatches(files), ghclient.DiffHunksFromPatches(files), nil
		}
		return nil, nil, err
	}
	return ghclient.DiffLines(diff), ghclient.DiffHunks(diff), nil
}

func handlePR(client *ghclient.Client, database *db.DB, args []string) {
	if len(args) < 1 {
		exitErr("usage: triagefactory exec gh pr <action> [flags]")
	}

	action := args[0]
	flags := args[1:]

	switch action {
	case "create":
		prCreate(client, database, flags)
	case "view":
		prView(client, flags)
	case "diff":
		prDiff(client, flags)
	case "files":
		prFiles(client, flags)
	case "thread-view":
		prThreadView(client, flags)
	case "review-view":
		prReviewView(client, flags)
	case "review-delete":
		prReviewDelete(database, flags)
	case "review-dismiss":
		prReviewDismiss(client, flags)
	case "start-review":
		prStartReview(client, database, flags)
	case "add-review-comment":
		prAddReviewComment(database, flags)
	case "submit-review":
		prSubmitReview(client, database, flags)
	case "comment-list-pending":
		prListPending(database, flags)
	case "add-comment":
		prAddComment(client, flags)
	case "comment-reply":
		prCommentReply(client, flags)
	case "comment-react":
		prCommentReact(client, flags)
	case "comment-update":
		prCommentUpdate(client, database, flags)
	case "comment-delete":
		prCommentDelete(client, database, flags)
	default:
		exitErr(fmt.Sprintf("unknown pr action: %s", action))
	}
}

func prView(client *ghclient.Client, args []string) {
	owner, repo, number := parseRepoAndNumber(args)
	verbose := hasFlag(args, "-v") || hasFlag(args, "--verbose")
	pr, err := client.GetPR(owner, repo, number, verbose)
	exitOnErr(err)
	printJSON(pr)
}

func prDiff(client *ghclient.Client, args []string) {
	owner, repo, number := parseRepoAndNumber(args)
	file := flagVal(args, "--file")
	diff, err := client.GetPRDiff(owner, repo, number, file)
	exitOnErr(err)
	fmt.Print(diff)
}

func prFiles(client *ghclient.Client, args []string) {
	owner, repo, number := parseRepoAndNumber(args)
	files, err := client.GetPRFiles(owner, repo, number)
	exitOnErr(err)
	printJSON(files)
}

func prThreadView(client *ghclient.Client, args []string) {
	if len(args) < 2 {
		exitErr("usage: gh pr thread-view <pr_number> <comment_id> [--page N]")
	}
	owner, repo, _ := parseRepoAndNumber(args[:1])
	commentID := mustInt(args[1], "comment_id")
	page := 1
	if v := flagVal(args, "--page"); v != "" {
		page = mustInt(v, "page")
	}
	thread, err := client.GetCommentThread(owner, repo, commentID, page)
	exitOnErr(err)
	printJSON(thread)
}

func prReviewView(client *ghclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr review-view <review_id> --pr <pr_number> [-v]")
	}
	reviewID := mustInt(args[0], "review_id")
	// Explicit --pr-missing message: the most common mistake here is
	// the agent extrapolating the `pr view <pr_number>` shape and
	// writing `pr review-view <pr_number>` (omitting --pr). The
	// generic "pr_number is required" mustInt error doesn't point at
	// the asymmetry; spelling it out gets the agent to the corrected
	// shape on the next attempt.
	prFlag := flagVal(args, "--pr")
	if prFlag == "" {
		exitErr(fmt.Sprintf(
			"review-view requires --pr <pr_number>. The positional argument %d is the review_id, not the PR number — they are different ids (review ids come from `gh pr view <pr_number> -v` -> reviews[].id). Canonical shape: gh pr review-view %d --pr <pr_number> [-v]",
			reviewID, reviewID,
		))
	}
	owner, repo := ownerRepo(args)
	prNumber := mustInt(prFlag, "pr_number")
	verbose := hasFlag(args, "-v") || hasFlag(args, "--verbose")
	detail, err := client.GetReviewDetail(owner, repo, prNumber, reviewID, verbose)
	exitOnErr(err)
	printJSON(detail)
}

func prReviewDelete(database *db.DB, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr review-delete <review_id>")
	}
	reviewID := args[0]
	err := db.DeletePendingReview(database.Conn, reviewID)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "review_id": reviewID})
}

func prReviewDismiss(client *ghclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr review-dismiss <review_id> --pr <number> --body <reason>")
	}
	reviewID := mustInt(args[0], "review_id")
	owner, repo := ownerRepo(args)
	prNumber := mustInt(flagVal(args, "--pr"), "pr_number")
	body := flagVal(args, "--body")
	if body == "" {
		body = "Dismissed"
	}
	err := client.DismissReview(owner, repo, prNumber, reviewID, body)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "review_id": reviewID, "status": "dismissed"})
}

// --- Review lifecycle (local state) ---

func prStartReview(client *ghclient.Client, database *db.DB, args []string) {
	owner, repo, number := parseRepoAndNumber(args)
	// Fetch head SHA for the review
	pr, err := client.GetPR(owner, repo, number, false)
	exitOnErr(err)

	// Fetch commentable lines + hunk ranges; falls back to per-file patches on HTTP 406.
	diffLinesMap, diffHunksMap, err := getDiffShapes(client, owner, repo, number)
	exitOnErr(err)

	// Lines as JSON: {"file": [1,2,3,...], ...}
	compactMap := make(map[string][]int)
	for file, lines := range diffLinesMap {
		for line := range lines {
			compactMap[file] = append(compactMap[file], line)
		}
	}
	diffLinesJSON, _ := json.Marshal(compactMap)

	// Hunks as JSON: {"file": [[start,end], ...], ...}
	hunksMap := make(map[string][][2]int, len(diffHunksMap))
	for file, hunks := range diffHunksMap {
		pairs := make([][2]int, len(hunks))
		for i, h := range hunks {
			pairs[i] = [2]int{h.NewStart, h.NewEnd}
		}
		hunksMap[file] = pairs
	}
	diffHunksJSON, _ := json.Marshal(hunksMap)

	reviewID := uuid.New().String()
	err = db.CreatePendingReview(database.Conn, domain.PendingReview{
		ID:        reviewID,
		PRNumber:  number,
		Owner:     owner,
		Repo:      repo,
		CommitSHA: pr.HeadSHA,
		DiffLines: string(diffLinesJSON),
		DiffHunks: string(diffHunksJSON),
		RunID:     os.Getenv("TRIAGE_FACTORY_RUN_ID"),
	})
	exitOnErr(err)

	printJSON(map[string]any{
		"review_id":  reviewID,
		"pr_number":  number,
		"commit_sha": pr.HeadSHA,
		"status":     "pending_local",
		"files":      len(diffLinesMap),
	})
}

func prAddReviewComment(database *db.DB, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr add-review-comment <review_id> --file <path> --line <N> --body <text> [--start-line <N>]")
	}
	reviewID := args[0]
	file := flagVal(args, "--file")
	line := mustInt(flagVal(args, "--line"), "line")
	body := flagVal(args, "--body")

	if file == "" || body == "" {
		exitErr("--file and --body are required")
	}

	var startLine *int
	if sl := flagVal(args, "--start-line"); sl != "" {
		v := mustInt(sl, "start-line")
		startLine = &v
	}

	// Verify review exists
	review, err := db.GetPendingReview(database.Conn, reviewID)
	exitOnErr(err)
	if review == nil {
		exitErr(fmt.Sprintf("pending review %s not found", reviewID))
	}

	// Validate the comment's range against the captured diff. Prefer the
	// hunk-aware check (start_line and line must be in the same hunk —
	// GitHub rejects cross-hunk ranges with 422 at submit time); fall back
	// to the legacy line-only check when diff_hunks is empty (pre-migration
	// rows) or its JSON fails to parse (internal corruption — better to
	// run the weaker check than silently accept the comment, since
	// diff_hunks is something *we* wrote).
	validated := false
	if review.DiffHunks != "" {
		var hunksJSON map[string][][2]int
		if err := json.Unmarshal([]byte(review.DiffHunks), &hunksJSON); err == nil {
			hunks := make(map[string][]ghclient.Hunk, len(hunksJSON))
			for f, pairs := range hunksJSON {
				hs := make([]ghclient.Hunk, len(pairs))
				for i, p := range pairs {
					hs[i] = ghclient.Hunk{NewStart: p[0], NewEnd: p[1]}
				}
				hunks[f] = hs
			}
			if msg := ghclient.ValidateCommentRange(hunks, file, line, startLine); msg != "" {
				exitErr(msg)
			}
			validated = true
		}
	}
	if !validated && review.DiffLines != "" {
		var validLines map[string][]int
		if json.Unmarshal([]byte(review.DiffLines), &validLines) == nil {
			fileLines, fileExists := validLines[file]
			if !fileExists {
				exitErr(fmt.Sprintf("file '%s' is not in the diff. Valid files: %v", file, keys(validLines)))
			}
			lineSet := make(map[int]bool, len(fileLines))
			for _, l := range fileLines {
				lineSet[l] = true
			}
			if !lineSet[line] {
				exitErr(fmt.Sprintf("line %d in '%s' is not part of the diff. Comment on lines that appear in the diff output.", line, file))
			}
			// Without hunk metadata we can't enforce same-hunk membership
			// (the residual 422 case the SubmitReview backstop covers), but
			// these two cheap checks always apply when --start-line is set
			// and don't need hunk info — without them the legacy/corrupted
			// path would skip start_line validation entirely.
			if startLine != nil {
				if *startLine > line {
					exitErr(fmt.Sprintf("start_line %d must be ≤ line %d", *startLine, line))
				}
				if !lineSet[*startLine] {
					exitErr(fmt.Sprintf("start_line %d in '%s' is not part of the diff. Comment on lines that appear in the diff output.", *startLine, file))
				}
			}
		}
	}

	commentID := uuid.New().String()
	comment := domain.PendingReviewComment{
		ID:        commentID,
		ReviewID:  reviewID,
		Path:      file,
		Line:      line,
		StartLine: startLine,
		Body:      body,
	}

	err = db.AddPendingReviewComment(database.Conn, comment)
	exitOnErr(err)

	printJSON(map[string]any{
		"comment_id": commentID,
		"review_id":  reviewID,
		"status":     "pending_local",
	})
}

func prSubmitReview(client *ghclient.Client, database *db.DB, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr submit-review <review_id> --event <approve|comment|request_changes> --body <text>")
	}
	reviewID := args[0]
	event := flagVal(args, "--event")
	body := flagVal(args, "--body")

	if event == "" {
		exitErr("--event is required (approve, comment, request_changes)")
	}

	eventMap := map[string]string{
		"approve":         "APPROVE",
		"comment":         "COMMENT",
		"request_changes": "REQUEST_CHANGES",
	}
	ghEvent, ok := eventMap[event]
	if !ok {
		ghEvent = event
	}

	// Load pending review
	review, err := db.GetPendingReview(database.Conn, reviewID)
	exitOnErr(err)
	if review == nil {
		exitErr(fmt.Sprintf("pending review %s not found", reviewID))
	}

	// Load pending comments
	pendingComments, err := db.ListPendingReviewComments(database.Conn, reviewID)
	exitOnErr(err)

	// Convert to GitHub format
	ghComments := make([]ghclient.SubmitReviewComment, len(pendingComments))
	for i, c := range pendingComments {
		ghComments[i] = ghclient.SubmitReviewComment{
			Path:      c.Path,
			Line:      c.Line,
			StartLine: c.StartLine,
			Body:      c.Body,
		}
	}

	// In preview mode (TRIAGE_FACTORY_REVIEW_PREVIEW=1, the default
	// for delegated runs), the agent's submit-review queues the
	// review for human approval rather than posting to GitHub. The
	// server injects header/footer with actual cost data at submit
	// time. SKY-212: lock the row on first agent submit so a second
	// submit-review call in the same run gets a hard error instead
	// of silently overwriting — agents were looping after seeing
	// the pending_approval response, mistaking it for "still pending,
	// keep going."
	if os.Getenv("TRIAGE_FACTORY_REVIEW_PREVIEW") == "1" {
		err = db.LockPendingReviewSubmission(database.Conn, reviewID, body, ghEvent)
		if errors.Is(err, db.ErrPendingReviewAlreadySubmitted) {
			exitErr(fmt.Sprintf(
				"review %s has already been queued for human approval. Do not call submit-review again — your work on this review is complete. Finish the run by writing $TRIAGE_FACTORY_RUN_ROOT/_scratch/entity-memory/<run_id>.md and returning your completion JSON.",
				reviewID,
			))
		}
		exitOnErr(err)

		printJSON(map[string]any{
			// queued_for_human_approval makes the contract explicit:
			// the review has been handed off to the human approval
			// queue and the agent's work on this PR is done. The old
			// "pending_approval" wording mirrored the run-status
			// vocabulary and was easy to misread as "still pending,
			// more to do." next_step spells out the wrap-up so even
			// agents that aren't reading the prompt closely get the
			// signal directly from the tool result.
			"status":          "queued_for_human_approval",
			"review_id":       reviewID,
			"event":           ghEvent,
			"comments_queued": len(ghComments),
			"next_step":       "Review is queued for human approval. Do not call submit-review again. Finish the run by writing $TRIAGE_FACTORY_RUN_ROOT/_scratch/entity-memory/<run_id>.md and returning your completion JSON.",
		})
		return
	}

	// Inject footer with run metadata
	body = body + agentmeta.Build(database.Conn, os.Getenv("TRIAGE_FACTORY_RUN_ID"), "review")

	// Submit atomically to GitHub
	ghReviewID, actualEvent, err := client.SubmitReview(
		review.Owner, review.Repo, review.PRNumber,
		review.CommitSHA, ghEvent, body, ghComments,
	)
	exitOnErr(err)

	// Clean up local state
	if err := db.DeletePendingReview(database.Conn, reviewID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to clean up local review state: %v\n", err)
	}

	printJSON(map[string]any{
		"github_review_id": ghReviewID,
		"event":            actualEvent,
		"comments_posted":  len(ghComments),
	})
}

// prCreate queues (preview mode) or directly opens (standalone mode)
// a pull request. Caller is responsible for having pushed the head
// branch upstream first; this is the contract documented in the
// agent prompts and enforced by GitHub's API at submit time anyway.
//
// Preview mode (TRIAGE_FACTORY_REVIEW_PREVIEW=1, the delegated-run
// default): create a pending_prs row + lock it. Output
// status=queued_for_human_approval. The server's submit handler
// reads the row at user-approval time, opens the PR for real, and
// applies the agentmeta footer.
//
// Standalone mode (env unset, e.g. a human running this directly
// from a checkout): pre-apply the footer and POST to GitHub
// immediately. Same shape as prSubmitReview's standalone branch.
func prCreate(client *ghclient.Client, database *db.DB, args []string) {
	title := flagVal(args, "--title")
	body := flagVal(args, "--body")
	bodyFile := flagVal(args, "--body-file")
	base := flagVal(args, "--base")
	head := flagVal(args, "--head")
	draft := hasFlag(args, "--draft")

	if title == "" {
		exitErr("usage: gh pr create --title <T> (--body <B> | --body-file <path>) --base <branch> [--head <branch>] [--draft] [--repo owner/repo]\n--title is required")
	}
	if base == "" {
		exitErr("--base is required (the branch to merge into, e.g. main)")
	}

	// --body and --body-file are mutually exclusive: with both set,
	// the agent's intent is ambiguous and silently picking one risks
	// dropping the longer/more-recent draft. Force a clean choice.
	if body != "" && bodyFile != "" {
		exitErr("--body and --body-file are mutually exclusive; pass one or the other")
	}
	if bodyFile != "" {
		// "-" means stdin, matching gh's convention. The file path is
		// read directly (no glob expansion, no relative-path
		// surprises — just os.ReadFile from cwd).
		var data []byte
		var err error
		if bodyFile == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(bodyFile)
		}
		if err != nil {
			exitErr("read --body-file: " + err.Error())
		}
		body = string(data)
	}

	// Strip Claude Code's auto-appended citation line. Claude Code
	// (the agent harness) routinely tacks "🤖 Generated with [Claude
	// Code](https://claude.com/claude-code)" onto every PR body it
	// produces. Triage Factory's footer (added by the server at
	// submit time) already attributes the work — letting the
	// upstream Claude Code citation through would visually crowd
	// out the TF citation and double-bill the PR. Strip before
	// queuing so the user never sees it in the preview, even if the
	// agent forgot to remove it.
	body = stripClaudeCodeCitation(body)

	owner, repo := ownerRepo(args)

	// If --head wasn't supplied, derive from the current branch. The
	// agent's cwd inside a materialized worktree is `feature/<KEY>`
	// after `cd "$(triagefactory exec workspace add ...)"` so this
	// resolves cleanly. exitErr if we can't determine — would otherwise
	// silently submit the wrong branch to GitHub.
	if head == "" {
		out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
		if err != nil {
			exitErr("could not determine current branch via git rev-parse; pass --head <branch> explicitly. err: " + err.Error())
		}
		head = strings.TrimSpace(string(out))
		if head == "" || head == "HEAD" {
			exitErr("current branch is detached or empty; pass --head <branch> explicitly")
		}
	}

	// Pre-flight: verify the head branch actually exists on origin.
	// pr create's contract requires `git push` first, but agents
	// occasionally skip that step (e.g. a Jira agent that decided
	// the changes already lived on the remote and went straight to
	// `pr create`). Without this check, the row queues fine, the
	// human approval overlay can't fetch the diff, the user sees a
	// 502, and the run isn't recoverable without manual DB surgery.
	// `ls-remote --exit-code` returns 2 specifically when nothing
	// matched, so we hard-fail with a clear "did you forget to
	// push?" message that teaches the agent how to retry — cheaper
	// than letting the agent finish the run with a broken row.
	//
	// However, this preflight relies on the current working directory's
	// git configuration (`origin`). When the caller targets a repository
	// explicitly (for example via `--repo` and `--head`) from outside a
	// local checkout, there is no worktree to query and we should let the
	// request proceed to the GitHub API instead of failing early here.
	inWorkTree := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	if err := inWorkTree.Run(); err == nil {
		lsRemote := exec.Command("git", "ls-remote", "--exit-code", "--heads", "origin", head)
		var lsStderr strings.Builder
		lsRemote.Stderr = &lsStderr
		if err := lsRemote.Run(); err != nil {
			exitErr(fmt.Sprintf(
				"head branch '%s' is not on origin. Run `git push origin %s` first, then retry `pr create`. `pr create` requires the head branch to exist on the upstream — without it the diff cannot be rendered for human approval. (git ls-remote stderr: %s)",
				head, head, strings.TrimSpace(lsStderr.String()),
			))
		}
	}

	if os.Getenv("TRIAGE_FACTORY_REVIEW_PREVIEW") == "1" {
		runID := os.Getenv("TRIAGE_FACTORY_RUN_ID")
		if runID == "" {
			exitErr("TRIAGE_FACTORY_REVIEW_PREVIEW=1 but TRIAGE_FACTORY_RUN_ID is empty; this command was meant to be invoked by the delegated agent")
		}

		// Capture the head sha at queue time so the UI can flag drift if
		// the agent pushes a fixup mid-approval. Best-effort: if
		// rev-parse fails (cwd doesn't have HEAD, weird state), we
		// surface the error rather than queueing a row with no sha
		// because the head_sha column is NOT NULL.
		headSHAOut, err := exec.Command("git", "rev-parse", "HEAD").Output()
		if err != nil {
			exitErr("could not capture head sha via git rev-parse HEAD; the worktree appears to be in a bad state. err: " + err.Error())
		}
		headSHA := strings.TrimSpace(string(headSHAOut))
		if headSHA == "" {
			exitErr("git rev-parse HEAD returned empty; refusing to queue a pending PR with no head sha")
		}

		// SKY-212-style anti-retry. The schema's UNIQUE(run_id) on
		// pending_prs would already block a second insert, but it
		// surfaces as a generic SQL constraint error that doesn't
		// teach the agent to stop calling. Check up front so the
		// agent gets a clear "already queued" message on retry,
		// matching what submit-review does for reviews.
		if existing, err := db.PendingPRByRunID(database.Conn, runID); err != nil {
			exitErr("lookup existing pending PR for run: " + err.Error())
		} else if existing != nil {
			exitErr(fmt.Sprintf(
				"a PR for run %s has already been queued for human approval. Do not call pr create again — your work is complete. Finish the run by writing $TRIAGE_FACTORY_RUN_ROOT/_scratch/entity-memory/<run_id>.md and returning your completion JSON.",
				runID,
			))
		}

		id := uuid.NewString()
		if err := db.CreatePendingPR(database.Conn, domain.PendingPR{
			ID: id, RunID: runID,
			Owner: owner, Repo: repo,
			HeadBranch: head, HeadSHA: headSHA, BaseBranch: base,
			Title: title, Body: body,
			Draft: draft,
		}); err != nil {
			// The pre-check above is racy: two concurrent `pr create`
			// invocations on different DB connections can both pass
			// it before either has inserted, then one wins the
			// UNIQUE(run_id) insert and the other lands here with a
			// generic SQL constraint error. Re-check by run_id; if a
			// row exists, treat this as the same SKY-212 retry case
			// the pre-check normally catches and surface the clean
			// "already queued" message instead of a confusing SQL
			// error the agent doesn't know how to interpret.
			if existing, lookupErr := db.PendingPRByRunID(database.Conn, runID); lookupErr == nil && existing != nil {
				exitErr(fmt.Sprintf(
					"a PR for run %s has already been queued for human approval. Do not call pr create again — your work is complete. Finish the run by writing $TRIAGE_FACTORY_RUN_ROOT/_scratch/entity-memory/<run_id>.md and returning your completion JSON.",
					runID,
				))
			}
			exitErr("failed to insert pending PR: " + err.Error())
		}
		// LockPendingPR is the second layer — it shouldn't trip in
		// practice now that we pre-check above, but it's still load-
		// bearing for the race where two `pr create` invocations
		// arrive at this point simultaneously (the pre-check happens
		// in two separate connections and both see "no existing
		// row"). The lock's WHERE locked = 0 gate serializes them at
		// the DB layer; the loser sees ErrPendingPRAlreadyQueued.
		if err := db.LockPendingPR(database.Conn, id, title, body); err != nil {
			if errors.Is(err, db.ErrPendingPRAlreadyQueued) {
				exitErr(fmt.Sprintf(
					"a PR for run %s has already been queued for human approval. Do not call pr create again — your work is complete. Finish the run by writing $TRIAGE_FACTORY_RUN_ROOT/_scratch/entity-memory/<run_id>.md and returning your completion JSON.",
					runID,
				))
			}
			exitErr("failed to lock pending PR: " + err.Error())
		}

		printJSON(map[string]any{
			"status":     "queued_for_human_approval",
			"id":         id,
			"owner":      owner,
			"repo":       repo,
			"head":       head,
			"base":       base,
			"head_sha":   headSHA,
			"draft_hint": draft, // not stored — passed through at user-approval time
			"next_step":  "PR is queued for human approval. Do not call pr create again. Finish the run by writing $TRIAGE_FACTORY_RUN_ROOT/_scratch/entity-memory/<run_id>.md and returning your completion JSON.",
		})
		return
	}

	// Standalone mode: open the PR immediately, with footer pre-applied.
	body = body + agentmeta.Build(database.Conn, os.Getenv("TRIAGE_FACTORY_RUN_ID"), "PR")
	number, htmlURL, err := client.CreatePR(owner, repo, head, base, title, body, draft)
	exitOnErr(err)

	printJSON(map[string]any{
		"status":   "submitted",
		"number":   number,
		"html_url": htmlURL,
	})
}

func prListPending(database *db.DB, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr comment-list-pending <review_id>")
	}
	reviewID := args[0]
	comments, err := db.ListPendingReviewComments(database.Conn, reviewID)
	exitOnErr(err)
	if comments == nil {
		comments = []domain.PendingReviewComment{}
	}
	printJSON(comments)
}

// --- Direct comments (hit GitHub API) ---

func prAddComment(client *ghclient.Client, args []string) {
	owner, repo, number := parseRepoAndNumber(args)
	body := flagVal(args, "--body")
	if body == "" {
		exitErr("--body is required")
	}
	commentID, err := client.AddComment(owner, repo, number, body)
	exitOnErr(err)
	printJSON(map[string]any{"comment_id": commentID})
}

func prCommentReply(client *ghclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr comment-reply <comment_id> --body <text> --pr <number>")
	}
	commentID := mustInt(args[0], "comment_id")
	owner, repo := ownerRepo(args)
	prNumber := mustInt(flagVal(args, "--pr"), "pr_number")
	body := flagVal(args, "--body")
	if body == "" {
		exitErr("--body is required")
	}
	replyID, err := client.ReplyToComment(owner, repo, prNumber, commentID, body)
	exitOnErr(err)
	printJSON(map[string]any{"reply_id": replyID})
}

func prCommentReact(client *ghclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr comment-react <comment_id> --emoji <emoji>")
	}
	commentID := mustInt(args[0], "comment_id")
	owner, repo := ownerRepo(args)
	emoji := flagVal(args, "--emoji")
	if emoji == "" {
		exitErr("--emoji is required (+1, -1, laugh, confused, heart, hooray, rocket, eyes)")
	}
	err := client.ReactToComment(owner, repo, commentID, emoji)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true})
}

func prCommentUpdate(client *ghclient.Client, database *db.DB, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr comment-update <comment_id> --body <text>")
	}
	commentID := args[0]
	body := flagVal(args, "--body")
	if body == "" {
		exitErr("--body is required")
	}

	// Check if it's a local pending comment (UUID) vs remote (integer)
	if isLocalID(commentID) {
		err := db.UpdatePendingReviewComment(database.Conn, commentID, body)
		exitOnErr(err)
		printJSON(map[string]any{"ok": true, "scope": "local"})
	} else {
		owner, repo := ownerRepo(args)
		id := mustInt(commentID, "comment_id")
		err := client.UpdateComment(owner, repo, id, body)
		exitOnErr(err)
		printJSON(map[string]any{"ok": true, "scope": "remote"})
	}
}

func prCommentDelete(client *ghclient.Client, database *db.DB, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr comment-delete <comment_id>")
	}
	commentID := args[0]

	if isLocalID(commentID) {
		err := db.DeletePendingReviewComment(database.Conn, commentID)
		exitOnErr(err)
		printJSON(map[string]any{"ok": true, "scope": "local"})
	} else {
		owner, repo := ownerRepo(args)
		id := mustInt(commentID, "comment_id")
		err := client.DeleteComment(owner, repo, id)
		exitOnErr(err)
		printJSON(map[string]any{"ok": true, "scope": "remote"})
	}
}

// isLocalID returns true if the ID looks like a UUID (local pending comment)
// vs an integer (GitHub remote comment).
func keys[K comparable, V any](m map[K]V) []K {
	result := make([]K, 0, len(m))
	for k := range m {
		result = append(result, k)
	}
	return result
}

func isLocalID(id string) bool {
	if len(id) == 0 {
		return false
	}
	// GitHub IDs are purely numeric, our local IDs are UUIDs (contain hyphens).
	_, err := strconv.Atoi(id)
	return err != nil
}

// --- argument parsing helpers ---

func parseRepoAndNumber(args []string) (string, string, int) {
	owner, repo := ownerRepo(args)
	// Find first positional arg (not a flag or flag value)
	num := firstPositional(args)
	if num == "" {
		exitErr("PR number is required")
	}
	number := mustInt(num, "pr_number")
	return owner, repo, number
}

// firstPositional returns the first argument that isn't a flag or a flag's value.
func firstPositional(args []string) string {
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if a == "--repo" || a == "--file" || a == "--pr" || a == "--body" || a == "--body-file" || a == "--line" || a == "--start-line" || a == "--event" || a == "--status" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a
	}
	return ""
}

// ownerRepo resolves the target repo for a PR subcommand. Delegates to the
// shared resolveRepo so --repo flag, TRIAGE_FACTORY_REPO env, and .git/config
// fallback all behave consistently across every gh command. Passes the
// full args slice (not just the flag value) so resolveRepo can detect
// "--repo present but empty" and fail loudly instead of silently falling
// back to env/git resolution.
func ownerRepo(args []string) (string, string) {
	owner, repo, err := resolveRepo(args)
	if err != nil {
		exitErr(err.Error())
	}
	return owner, repo
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func flagVal(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func mustInt(s, name string) int {
	if s == "" {
		exitErr(name + " is required")
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		exitErr(fmt.Sprintf("invalid %s: %s", name, s))
	}
	return v
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func exitOnErr(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func exitErr(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

// claudeCodeCitationFragment matches the citation Claude Code
// auto-appends to every PR body it produces. We match on the
// markdown-link substring rather than the full "🤖 Generated with
// ..." prefix so the strip survives the agent reformatting the
// emoji or surrounding text. The link target is stable.
const claudeCodeCitationFragment = "Generated with [Claude Code](https://claude.com/claude-code)"

// stripClaudeCodeCitation drops the trailing line containing the
// Claude Code citation, plus any whitespace separating it from the
// preceding content. Returns body unchanged when the last
// non-whitespace line doesn't contain the citation — including the
// case where the citation appears mid-body, since that's content
// the user wrote intentionally.
func stripClaudeCodeCitation(body string) string {
	trimmed := strings.TrimRight(body, " \t\n\r")
	if trimmed == "" {
		return body
	}
	lastNL := strings.LastIndex(trimmed, "\n")
	var lastLine string
	if lastNL == -1 {
		lastLine = trimmed
	} else {
		lastLine = trimmed[lastNL+1:]
	}
	if !strings.Contains(lastLine, claudeCodeCitationFragment) {
		return body
	}
	if lastNL == -1 {
		return ""
	}
	return strings.TrimRight(trimmed[:lastNL], " \t\n\r")
}
