package tracker

import (
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// shouldSkipRefresh reports whether a tracked open PR can keep its
// stored snapshot for this poll cycle (no follow-up RefreshPRs call
// needed). Skipping means we trust the stored snapshot through this
// cycle and emit no events for the entity.
//
// Why this is safe in the common case:
//
//   - Anything that changes the diff scope at the PR level (commits,
//     reviews, labels, comments, ready_for_review, mergeable, title,
//     base ref) bumps GitHub's PullRequest.updatedAt. If updatedAt
//     matches stored, none of those happened.
//   - Head SHA changes always bump updatedAt (a push is a PR-level
//     mutation), but we belt-and-brace it because a regression there
//     would silently drop new_commits events.
//
// Why the in-flight CI guard is necessary: check runs live on commits,
// not on the PR row. A check run completing does NOT bump
// PullRequest.updatedAt. So gating on updatedAt alone would lose
// ci_passed / ci_failed transitions. The guard forces a refresh on any
// stored snapshot where CI is still running.
//
// Why the CheckRuns!=nil guard is necessary: a freshly discovered
// entity has its snapshot seeded from the discovery fragment, which
// doesn't include check_runs (CheckRuns == nil signals "unknown prior
// state"). On its first poll-cycle pass through this gate, fresh and
// stored will have the same updatedAt + SHA, but the stored snapshot
// has no CI data. Skipping would leave it that way forever. Forcing
// the first refresh fills it in and the gate engages on subsequent
// cycles.
//
// What it deliberately doesn't catch: a workflow re-run that adds a
// brand-new check_run on an unchanged head SHA without bumping PR
// updatedAt and without leaving anything in-flight from a prior run
// in the stored snapshot. That's a real but narrow case; we'll catch
// it on the next legitimate updatedAt change, at most one cycle late.
// If it ever becomes a real complaint, the fix is a max-staleness
// floor that forces a refresh every N minutes regardless.
func shouldSkipRefresh(stored, fresh domain.PRSnapshot) bool {
	if stored.UpdatedAt == "" || fresh.UpdatedAt == "" {
		return false
	}
	if stored.UpdatedAt != fresh.UpdatedAt {
		return false
	}
	if stored.HeadSHA == "" || fresh.HeadSHA == "" {
		return false
	}
	if stored.HeadSHA != fresh.HeadSHA {
		return false
	}
	if stored.CheckRuns == nil {
		return false
	}
	if hasInFlightCI(stored) {
		return false
	}
	return true
}

// hasInFlightCI reports whether the snapshot contains any check_run
// in a non-terminal state. Only "completed" is terminal; any other
// status should be treated as still in flight until GitHub reports
// completion and populates conclusion.
func hasInFlightCI(snap domain.PRSnapshot) bool {
	for _, cr := range snap.CheckRuns {
		if cr.Status != "completed" {
			return true
		}
	}
	return false
}
