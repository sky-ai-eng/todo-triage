package server

import (
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// HumanFeedbackInput is the formatter's input. Pure data — the
// handler does the DB work to assemble it, the formatter is a
// no-side-effects function so it's fully covered by golden tests.
type HumanFeedbackInput struct {
	// OriginalBody is the agent's drafted review body (write-once
	// snapshot from pending_reviews.original_review_body). nil means
	// no snapshot exists — typically a legacy review row from before
	// SKY-204 added the column. A non-nil pointer to "" is a real
	// snapshot of an empty drafted body (common — agents that
	// produce inline comments alone leave the top-level body
	// empty). The formatter degrades only on nil; an empty real
	// snapshot still drives a body diff if the human added text.
	OriginalBody *string

	FinalBody string

	// OriginalEvent: same nil-vs-empty distinction as OriginalBody.
	// In practice events shouldn't ever be a legitimately empty
	// string (handleReviewSubmit rejects review_event=""), but the
	// pointer encoding keeps the snapshot semantics symmetric and
	// future-proof.
	OriginalEvent *string
	FinalEvent    string

	Comments []ReviewCommentDiffEntry
}

// ReviewCommentDiffEntry is the per-comment classification driving
// the bullet list. The handler builds these by joining the agent's
// drafted comments (read off pending_review_comments at the moment
// of submit, before DeletePendingReview clears them) with the user's
// final comment set, keyed by comment ID. Status is one of:
//
//   - CommentDiffUnchanged: same id, body matches the original
//     snapshot (or no snapshot exists — the legacy case is folded
//     into unchanged so we don't emit a Was: "" / Now: "..." entry
//     against a fabricated original).
//   - CommentDiffEdited:    same id, body differs from the snapshot.
//   - CommentDiffRemoved:   id was in the agent's draft, not in the
//     final set (deferred — see review_diff.go's note in
//     buildHumanFeedbackInput).
//   - CommentDiffAdded:     id is in the final set with no original_body
//     captured (deferred — same note).
//
// Original/Final are plain strings rather than pointers because the
// handler classifies the legacy case as Unchanged and never reads
// Original on the unchanged path; non-unchanged statuses always
// have a real snapshot to render.
type ReviewCommentDiffEntry struct {
	Path     string
	Line     int
	Status   string
	Original string
	Final    string
}

const (
	CommentDiffUnchanged = "unchanged"
	CommentDiffEdited    = "edited"
	CommentDiffRemoved   = "removed"
	CommentDiffAdded     = "added"
)

// humanFeedbackHeading is the literal first line of the produced
// block. Matches db.humanFeedbackHeader byte-for-byte (modulo the
// trailing newlines the constant carries) so the per-entity memory
// materialization can render either path identically. If you change
// one side, change the other — there's a doc comment on
// db.humanFeedbackHeader pointing here.
const humanFeedbackHeading = "## Human feedback (post-run)"

// FormatHumanFeedback produces the markdown block written to
// run_memory.human_content when a delegated review is submitted. The
// shape is fixed — the agent briefing teaches future agents to scan
// for "## Human feedback (post-run)" headings in materialized memory
// and consume the structure below as authoritative. Don't reorder or
// rename the bold labels without updating the briefing in lockstep.
//
// No-edits collapse: when the body, verdict, and every comment match
// the agent's draft, the block is just the heading + outcome +
// verdict line. The presence of a row with that minimal block is
// itself the high-signal datum: "the human accepted everything the
// agent drafted, here's the verdict they ran with."
func FormatHumanFeedback(in HumanFeedbackInput) string {
	bodyEdited := bodyChanged(in)
	verdictEdited := verdictChanged(in)
	commentEdited := anyCommentEdits(in.Comments)

	var sb strings.Builder
	sb.WriteString(humanFeedbackHeading)
	sb.WriteString("\n\n")

	if bodyEdited || verdictEdited || commentEdited {
		sb.WriteString("**Outcome:** Human submitted the review with edits.\n")
	} else {
		sb.WriteString("**Outcome:** Human submitted the review as drafted, no edits.\n")
	}

	writeVerdictLine(&sb, in)

	if bodyEdited {
		sb.WriteString("\n**Body:** Edited.\n")
		writeBlockquote(&sb, in.FinalBody)
		sb.WriteString(">\n")
		sb.WriteString("> **Originally drafted as:**\n")
		// bodyEdited is only true when OriginalBody != nil (see
		// bodyChanged), so the deref is safe here.
		writeBlockquote(&sb, *in.OriginalBody)
	}

	if commentEdited {
		sb.WriteString("\n**Comment edits:**\n\n")
		for _, c := range in.Comments {
			if c.Status == CommentDiffUnchanged {
				continue
			}
			writeCommentEntry(&sb, c)
		}
	}

	return sb.String()
}

func bodyChanged(in HumanFeedbackInput) bool {
	// Without an original snapshot we can't *prove* the body
	// changed. Returning false here keeps the block honest —
	// we're not claiming "as drafted" either, just declining to
	// emit a body section we can't substantiate. nil-vs-empty
	// matters: a non-nil pointer to "" means the agent really
	// drafted an empty body, and a human-added body is a real
	// edit we want to surface.
	if in.OriginalBody == nil {
		return false
	}
	return strings.TrimSpace(*in.OriginalBody) != strings.TrimSpace(in.FinalBody)
}

func verdictChanged(in HumanFeedbackInput) bool {
	if in.OriginalEvent == nil {
		return false
	}
	return *in.OriginalEvent != in.FinalEvent
}

func anyCommentEdits(comments []ReviewCommentDiffEntry) bool {
	for _, c := range comments {
		if c.Status != CommentDiffUnchanged {
			return true
		}
	}
	return false
}

func writeVerdictLine(sb *strings.Builder, in HumanFeedbackInput) {
	switch {
	case in.OriginalEvent == nil:
		// Legacy / no snapshot: emit the bare final verdict so
		// the next agent at least knows what was submitted, but
		// don't attach an unchanged/changed claim we can't back.
		fmt.Fprintf(sb, "**Verdict:** %s.\n", in.FinalEvent)
	case *in.OriginalEvent == in.FinalEvent:
		fmt.Fprintf(sb, "**Verdict:** %s (unchanged from agent's draft).\n", in.FinalEvent)
	default:
		fmt.Fprintf(sb, "**Verdict changed:** agent drafted %s, human submitted %s.\n",
			*in.OriginalEvent, in.FinalEvent)
	}
}

// writeBlockquote emits the body verbatim with each line prefixed by
// "> " — markdown blockquote. Used for review/comment bodies so the
// next agent parsing the block sees an unambiguous boundary instead
// of a quoted string that could collide with quotes inside the
// content. Empty input still emits a single "> " marker so the
// downstream `>` separator line still composes correctly.
func writeBlockquote(sb *strings.Builder, content string) {
	content = strings.TrimRight(content, "\n")
	if content == "" {
		sb.WriteString("> \n")
		return
	}
	for _, line := range strings.Split(content, "\n") {
		sb.WriteString("> ")
		sb.WriteString(line)
		sb.WriteString("\n")
	}
}

func writeCommentEntry(sb *strings.Builder, c ReviewCommentDiffEntry) {
	switch c.Status {
	case CommentDiffEdited:
		fmt.Fprintf(sb, "- `%s:%d` — edited\n", c.Path, c.Line)
		fmt.Fprintf(sb, "  - Was: %s\n", inlineBody(c.Original))
		fmt.Fprintf(sb, "  - Now: %s\n", inlineBody(c.Final))
	case CommentDiffRemoved:
		fmt.Fprintf(sb, "- `%s:%d` — removed by human\n", c.Path, c.Line)
		fmt.Fprintf(sb, "  - Was: %s\n", inlineBody(c.Original))
	case CommentDiffAdded:
		fmt.Fprintf(sb, "- `%s:%d` — added by human (was not in agent's draft)\n", c.Path, c.Line)
		fmt.Fprintf(sb, "  - Body: %s\n", inlineBody(c.Final))
	}
}

// buildHumanFeedbackInput translates the handler's DB types into the
// formatter's pure-data input. Called inline from handleReviewSubmit
// while the originals are still loaded (DeletePendingReview below
// removes them). The classification is intentionally limited to
// `unchanged` and `edited`: detecting `removed` (human deleted an
// agent-drafted comment) requires soft-delete on
// pending_review_comments, which is a schema change beyond SKY-205's
// scope; `added` (human inserted a fresh comment) requires a UI
// flow that doesn't exist yet. Both statuses are still emitted by
// the formatter — they just won't appear in v1's output.
//
// Comment classification: nil OriginalBody means a legacy row from
// before AddPendingReviewComment captured the snapshot — fold to
// `unchanged` so we don't render "Was: <empty>" against a
// fabricated original. A non-nil pointer to "" is a real snapshot of
// an empty agent-drafted comment, which still drives the
// edited/unchanged comparison normally.
//
// finalEvent (the value GitHub actually accepted) overrides
// review.ReviewEvent for the "what was submitted" half of the
// verdict diff. They're usually the same, but ghClient.SubmitReview
// returns the canonical event in case GitHub coerced the input
// (e.g. legacy alias mapping).
func buildHumanFeedbackInput(
	review *domain.PendingReview,
	comments []domain.PendingReviewComment,
	finalEvent string,
) HumanFeedbackInput {
	entries := make([]ReviewCommentDiffEntry, 0, len(comments))
	for _, c := range comments {
		status := CommentDiffUnchanged
		original := ""
		if c.OriginalBody != nil {
			original = *c.OriginalBody
			if original != c.Body {
				status = CommentDiffEdited
			}
		}
		entries = append(entries, ReviewCommentDiffEntry{
			Path:     c.Path,
			Line:     c.Line,
			Status:   status,
			Original: original,
			Final:    c.Body,
		})
	}
	return HumanFeedbackInput{
		OriginalBody:  review.OriginalReviewBody,
		FinalBody:     review.ReviewBody,
		OriginalEvent: review.OriginalReviewEvent,
		FinalEvent:    finalEvent,
		Comments:      entries,
	}
}

// inlineBody collapses a comment body into a single line for use
// inside a markdown list item's "- Was:/Now:/Body:" prose. Newlines
// fold to spaces because review comments are typically a sentence or
// two and rendering them inline keeps the diff scannable; the
// trade-off is that paragraph structure in long comments is lost.
// For the (rare) review *body* — which is often multi-paragraph —
// the formatter uses writeBlockquote instead, preserving line
// structure where it actually matters.
func inlineBody(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
