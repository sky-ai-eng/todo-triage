package sqlite

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestSubstituteLocalGitHubIdentity covers the three branches the
// seed-time substitution cares about: empty allowlist becomes
// single-entry, non-empty allowlists are preserved, and missing
// fields stay missing.
func TestSubstituteLocalGitHubIdentity(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		localUser string
		want      map[string]any
		wantInput bool // true means expect output == input
	}{
		{
			name:      "empty author_in → single-entry allowlist",
			input:     `{"author_in":[]}`,
			localUser: "AidanAllchin",
			want:      map[string]any{"author_in": []any{"AidanAllchin"}},
		},
		{
			name:      "empty reviewer_in + author_in both substituted",
			input:     `{"author_in":[],"reviewer_in":[]}`,
			localUser: "AidanAllchin",
			want: map[string]any{
				"author_in":   []any{"AidanAllchin"},
				"reviewer_in": []any{"AidanAllchin"},
			},
		},
		{
			name:      "non-empty allowlist preserved verbatim",
			input:     `{"author_in":["someone-else"]}`,
			localUser: "AidanAllchin",
			wantInput: true,
		},
		{
			name:      "missing allowlist field left absent",
			input:     `{"is_draft":false}`,
			localUser: "AidanAllchin",
			wantInput: true,
		},
		{
			name:      "empty username is no-op (no GitHub connected yet)",
			input:     `{"author_in":[]}`,
			localUser: "",
			wantInput: true,
		},
		{
			name:      "malformed JSON passes through",
			input:     `not-json`,
			localUser: "AidanAllchin",
			wantInput: true,
		},
		{
			name:      "empty input passes through",
			input:     "",
			localUser: "AidanAllchin",
			wantInput: true,
		},
		{
			name:      "commenter_in substituted same as the others",
			input:     `{"commenter_in":[]}`,
			localUser: "AidanAllchin",
			want:      map[string]any{"commenter_in": []any{"AidanAllchin"}},
		},
		{
			name:      "jira assignee_in untouched by the GitHub helper",
			input:     `{"assignee_in":[]}`,
			localUser: "AidanAllchin",
			wantInput: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := substituteLocalGitHubIdentity(tc.input, tc.localUser)
			if tc.wantInput {
				if got != tc.input {
					t.Errorf("expected passthrough; got %q want %q", got, tc.input)
				}
				return
			}
			var actual map[string]any
			if err := json.Unmarshal([]byte(got), &actual); err != nil {
				t.Fatalf("substituted JSON failed to decode: %v\n%s", err, got)
			}
			if !reflect.DeepEqual(actual, tc.want) {
				t.Errorf("substituted JSON mismatch:\ngot:  %v\nwant: %v", actual, tc.want)
			}
		})
	}
}

// TestSubstituteLocalJiraIdentity is the SKY-270 mirror: empty
// assignee_in becomes single-entry, GitHub-namespaced keys are ignored
// so the two helpers compose, and the same edge cases (empty identity,
// malformed JSON, non-empty preservation) degrade cleanly.
func TestSubstituteLocalJiraIdentity(t *testing.T) {
	const accountID = "557058:abc-aidan"
	cases := []struct {
		name      string
		input     string
		localID   string
		want      map[string]any
		wantInput bool
	}{
		{
			name:    "empty assignee_in → single-entry account ID",
			input:   `{"assignee_in":[]}`,
			localID: accountID,
			want:    map[string]any{"assignee_in": []any{accountID}},
		},
		{
			name:      "non-empty assignee_in preserved verbatim",
			input:     `{"assignee_in":["someone-else"]}`,
			localID:   accountID,
			wantInput: true,
		},
		{
			name:      "github author_in ignored by the Jira helper",
			input:     `{"author_in":[]}`,
			localID:   accountID,
			wantInput: true,
		},
		{
			name:      "empty account ID is no-op (Jira not connected yet)",
			input:     `{"assignee_in":[]}`,
			localID:   "",
			wantInput: true,
		},
		{
			name:      "malformed JSON passes through",
			input:     `not-json`,
			localID:   accountID,
			wantInput: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := substituteLocalJiraIdentity(tc.input, tc.localID)
			if tc.wantInput {
				if got != tc.input {
					t.Errorf("expected passthrough; got %q want %q", got, tc.input)
				}
				return
			}
			var actual map[string]any
			if err := json.Unmarshal([]byte(got), &actual); err != nil {
				t.Fatalf("substituted JSON failed to decode: %v\n%s", err, got)
			}
			if !reflect.DeepEqual(actual, tc.want) {
				t.Errorf("substituted JSON mismatch:\ngot:  %v\nwant: %v", actual, tc.want)
			}
		})
	}
}
