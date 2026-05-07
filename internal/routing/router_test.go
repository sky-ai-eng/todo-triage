package routing

import (
	"encoding/json"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// --- matchPredicate unit tests ----------------------------------------------
// These validate the predicate-matching logic the router uses to evaluate
// task_rules and prompt_triggers against events. The type-erased matcher
// is already tested in the events package; these tests exercise the router's
// specific calling convention (empty predJSON = match-all, unknown event
// type = no match, etc.).

func TestMatchPredicate_EmptyPredicate_MatchesAll(t *testing.T) {
	meta := events.GitHubPRCICheckFailedMetadata{
		Author:       "aidan",
		AuthorIsSelf: true,
		CheckName:    "build",
		Repo:         "owner/repo",
	}
	metaJSON, _ := json.Marshal(meta)

	matched, err := matchPredicate(domain.EventGitHubPRCICheckFailed, "", string(metaJSON))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !matched {
		t.Error("empty predicate should match all events")
	}
}

func TestMatchPredicate_AuthorIsSelf_True(t *testing.T) {
	meta := events.GitHubPRCICheckFailedMetadata{
		Author:       "aidan",
		AuthorIsSelf: true,
		CheckName:    "build",
	}
	metaJSON, _ := json.Marshal(meta)

	matched, err := matchPredicate(domain.EventGitHubPRCICheckFailed, `{"author_is_self":true}`, string(metaJSON))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !matched {
		t.Error("expected match when AuthorIsSelf=true and predicate requires it")
	}
}

func TestMatchPredicate_AuthorIsSelf_False_Rejects(t *testing.T) {
	meta := events.GitHubPRCICheckFailedMetadata{
		Author:       "someone-else",
		AuthorIsSelf: false,
		CheckName:    "build",
	}
	metaJSON, _ := json.Marshal(meta)

	matched, err := matchPredicate(domain.EventGitHubPRCICheckFailed, `{"author_is_self":true}`, string(metaJSON))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if matched {
		t.Error("expected no match when AuthorIsSelf=false but predicate requires true")
	}
}

func TestMatchPredicate_MultiField_AND(t *testing.T) {
	meta := events.GitHubPRCICheckFailedMetadata{
		Author:       "aidan",
		AuthorIsSelf: true,
		CheckName:    "test",
		Repo:         "owner/repo",
	}
	metaJSON, _ := json.Marshal(meta)

	// Both fields match.
	matched, _ := matchPredicate(domain.EventGitHubPRCICheckFailed,
		`{"author_is_self":true,"check_name":"test"}`, string(metaJSON))
	if !matched {
		t.Error("expected match when both fields pass")
	}

	// One field fails.
	matched, _ = matchPredicate(domain.EventGitHubPRCICheckFailed,
		`{"author_is_self":true,"check_name":"build"}`, string(metaJSON))
	if matched {
		t.Error("expected no match when one field fails (AND semantics)")
	}
}

func TestMatchPredicate_HasLabel(t *testing.T) {
	meta := events.GitHubPRNewCommitsMetadata{
		Author:       "aidan",
		AuthorIsSelf: true,
		Labels:       []string{"wip", "self-review"},
		Repo:         "owner/repo",
	}
	metaJSON, _ := json.Marshal(meta)

	// Label present.
	matched, _ := matchPredicate(domain.EventGitHubPRNewCommits,
		`{"has_label":"self-review"}`, string(metaJSON))
	if !matched {
		t.Error("expected match when HasLabel=self-review and label is present")
	}

	// Label absent.
	matched, _ = matchPredicate(domain.EventGitHubPRNewCommits,
		`{"has_label":"urgent"}`, string(metaJSON))
	if matched {
		t.Error("expected no match when HasLabel=urgent but label is absent")
	}
}

func TestMatchPredicate_LabelName_OnLabelEvent(t *testing.T) {
	meta := events.GitHubPRLabelAddedMetadata{
		Author:       "aidan",
		AuthorIsSelf: true,
		LabelName:    "self-review",
		Labels:       []string{"self-review"},
		Repo:         "owner/repo",
	}
	metaJSON, _ := json.Marshal(meta)

	matched, _ := matchPredicate(domain.EventGitHubPRLabelAdded,
		`{"label_name":"self-review","author_is_self":true}`, string(metaJSON))
	if !matched {
		t.Error("expected match on label_name + author_is_self")
	}

	matched, _ = matchPredicate(domain.EventGitHubPRLabelAdded,
		`{"label_name":"urgent"}`, string(metaJSON))
	if matched {
		t.Error("expected no match when label_name doesn't match")
	}
}

func TestMatchPredicate_UnknownEventType_NoMatch(t *testing.T) {
	matched, err := matchPredicate("github:pr:does_not_exist", `{"author_is_self":true}`, `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if matched {
		t.Error("unknown event type should not match")
	}
}

func TestMatchPredicate_JiraAssigneeIsSelf(t *testing.T) {
	meta := events.JiraIssueAssignedMetadata{
		Assignee:       "Aidan Allchin",
		AssigneeIsSelf: true,
		IssueKey:       "SKY-123",
		Project:        "SKY",
	}
	metaJSON, _ := json.Marshal(meta)

	matched, _ := matchPredicate(domain.EventJiraIssueAssigned,
		`{"assignee_is_self":true}`, string(metaJSON))
	if !matched {
		t.Error("expected match when assignee_is_self=true")
	}

	// Reassigned to someone else.
	meta.AssigneeIsSelf = false
	meta.Assignee = "Bob"
	metaJSON, _ = json.Marshal(meta)
	matched, _ = matchPredicate(domain.EventJiraIssueAssigned,
		`{"assignee_is_self":true}`, string(metaJSON))
	if matched {
		t.Error("expected no match when assignee_is_self=false")
	}
}

func TestMatchPredicate_ReviewerIsSelf_SelfReview(t *testing.T) {
	meta := events.GitHubPRReviewCommentedMetadata{
		Author:         "aidan",
		AuthorIsSelf:   true,
		Reviewer:       "aidan",
		ReviewerIsSelf: true,
		Labels:         []string{"self-review"},
	}
	metaJSON, _ := json.Marshal(meta)

	// Self-review scenario: reviewer_is_self + author_is_self + has_label
	matched, _ := matchPredicate(domain.EventGitHubPRReviewCommented,
		`{"reviewer_is_self":true,"author_is_self":true,"has_label":"self-review"}`,
		string(metaJSON))
	if !matched {
		t.Error("expected match for self-review scenario predicate")
	}

	// External review: reviewer_is_self=false
	meta.Reviewer = "alice"
	meta.ReviewerIsSelf = false
	metaJSON, _ = json.Marshal(meta)
	matched, _ = matchPredicate(domain.EventGitHubPRReviewCommented,
		`{"reviewer_is_self":true,"author_is_self":true,"has_label":"self-review"}`,
		string(metaJSON))
	if matched {
		t.Error("expected no match when reviewer_is_self=false")
	}
}

// --- EntityTerminatingEvents ------------------------------------------------

func TestEntityTerminatingEvents(t *testing.T) {
	terminators := []string{
		domain.EventGitHubPRMerged,
		domain.EventGitHubPRClosed,
		domain.EventJiraIssueCompleted,
	}
	for _, et := range terminators {
		if !EntityTerminatingEvents[et] {
			t.Errorf("expected %q to be entity-terminating", et)
		}
	}

	nonTerminators := []string{
		domain.EventGitHubPRCICheckFailed,
		domain.EventGitHubPRNewCommits,
		domain.EventGitHubPRReviewRequested,
		domain.EventGitHubPRReviewRequestRemoved,
		domain.EventJiraIssueAssigned,
	}
	for _, et := range nonTerminators {
		if EntityTerminatingEvents[et] {
			t.Errorf("expected %q to NOT be entity-terminating", et)
		}
	}
}
