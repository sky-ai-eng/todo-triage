package domain

import (
	"strings"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// TestYieldRequest_Validate covers the malformed-payload guard that
// keeps a run from parking in awaiting_input with a yield the modal
// can't render meaningfully.
func TestYieldRequest_Validate(t *testing.T) {
	cases := []struct {
		name    string
		req     *YieldRequest
		wantErr bool
	}{
		{
			name:    "nil",
			req:     nil,
			wantErr: true,
		},
		{
			name:    "unknown type",
			req:     &YieldRequest{Type: "plan_steps", Message: "hi"},
			wantErr: true,
		},
		{
			name:    "confirmation missing message",
			req:     &YieldRequest{Type: YieldTypeConfirmation, Message: ""},
			wantErr: true,
		},
		{
			name:    "confirmation whitespace message",
			req:     &YieldRequest{Type: YieldTypeConfirmation, Message: "  \t\n  "},
			wantErr: true,
		},
		{
			name:    "prompt missing message",
			req:     &YieldRequest{Type: YieldTypePrompt, Message: ""},
			wantErr: true,
		},
		{
			name:    "choice no options",
			req:     &YieldRequest{Type: YieldTypeChoice, Message: "pick", Options: nil},
			wantErr: true,
		},
		{
			name: "choice option empty id",
			req: &YieldRequest{
				Type: YieldTypeChoice, Message: "pick",
				Options: []YieldChoiceOption{{ID: "", Label: "A"}},
			},
			wantErr: true,
		},
		{
			name: "choice option empty label",
			req: &YieldRequest{
				Type: YieldTypeChoice, Message: "pick",
				Options: []YieldChoiceOption{{ID: "a", Label: " "}},
			},
			wantErr: true,
		},
		{
			name: "choice duplicate ids",
			req: &YieldRequest{
				Type: YieldTypeChoice, Message: "pick",
				Options: []YieldChoiceOption{{ID: "a", Label: "A"}, {ID: "a", Label: "Also A"}},
			},
			wantErr: true,
		},
		{
			name:    "confirmation valid (labels optional)",
			req:     &YieldRequest{Type: YieldTypeConfirmation, Message: "go?"},
			wantErr: false,
		},
		{
			name:    "prompt valid (placeholder optional)",
			req:     &YieldRequest{Type: YieldTypePrompt, Message: "name?"},
			wantErr: false,
		},
		{
			name: "choice valid",
			req: &YieldRequest{
				Type: YieldTypeChoice, Message: "pick",
				Options: []YieldChoiceOption{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}},
			},
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("expected error for %s, got nil", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected no error for %s, got %v", tc.name, err)
			}
		})
	}
}

func TestRenderYieldResponseForAgent_Confirmation(t *testing.T) {
	req := &YieldRequest{Type: YieldTypeConfirmation, Message: "force push?"}
	got := RenderYieldResponseForAgent(req, &YieldResponse{Type: YieldTypeConfirmation, Accepted: boolPtr(true)})
	if !strings.Contains(got, "accepted") {
		t.Errorf("missing 'accepted' in %q", got)
	}
	got = RenderYieldResponseForAgent(req, &YieldResponse{Type: YieldTypeConfirmation, Accepted: boolPtr(false)})
	if !strings.Contains(got, "declined") {
		t.Errorf("missing 'declined' in %q", got)
	}
	// Nil Accepted defaults to "declined" in the renderer (validation
	// rejects nil before reaching this layer; this is a defensive
	// fallback rather than an expected input).
	got = RenderYieldResponseForAgent(req, &YieldResponse{Type: YieldTypeConfirmation, Accepted: nil})
	if !strings.Contains(got, "declined") {
		t.Errorf("nil Accepted should render as declined, got %q", got)
	}
}

func TestRenderYieldResponseForAgent_ChoiceUsesLabels(t *testing.T) {
	req := &YieldRequest{
		Type:    YieldTypeChoice,
		Message: "which?",
		Options: []YieldChoiceOption{{ID: "a", Label: "Approach A"}, {ID: "b", Label: "Approach B"}},
	}
	got := RenderYieldResponseForAgent(req, &YieldResponse{Type: YieldTypeChoice, Selected: []string{"a", "b"}})
	if !strings.Contains(got, "Approach A") || !strings.Contains(got, "Approach B") {
		t.Errorf("labels missing: %q", got)
	}
	if !strings.Contains(got, "a, b") {
		t.Errorf("expected option ids in %q", got)
	}
}

func TestRenderYieldResponseForAgent_PromptIncludesValue(t *testing.T) {
	req := &YieldRequest{Type: YieldTypePrompt, Message: "name?"}
	got := RenderYieldResponseForAgent(req, &YieldResponse{Type: YieldTypePrompt, Value: "Aidan"})
	if !strings.Contains(got, "Aidan") {
		t.Errorf("value missing: %q", got)
	}

	got = RenderYieldResponseForAgent(req, &YieldResponse{Type: YieldTypePrompt, Value: ""})
	if !strings.Contains(got, "empty") {
		t.Errorf("expected 'empty' wording for blank prompt: %q", got)
	}
}

func TestRenderYieldResponseForDisplay_PrefersUserLabels(t *testing.T) {
	req := &YieldRequest{Type: YieldTypeConfirmation, AcceptLabel: "Force push", RejectLabel: "Stop"}
	got := RenderYieldResponseForDisplay(req, &YieldResponse{Type: YieldTypeConfirmation, Accepted: boolPtr(true)})
	if got != "Force push" {
		t.Errorf("display = %q, want %q", got, "Force push")
	}
	got = RenderYieldResponseForDisplay(req, &YieldResponse{Type: YieldTypeConfirmation, Accepted: boolPtr(false)})
	if got != "Stop" {
		t.Errorf("display = %q, want %q", got, "Stop")
	}
}

func TestRenderYieldResponseForDisplay_FallsBackWhenLabelsAbsent(t *testing.T) {
	req := &YieldRequest{Type: YieldTypeConfirmation}
	got := RenderYieldResponseForDisplay(req, &YieldResponse{Type: YieldTypeConfirmation, Accepted: boolPtr(true)})
	if got != "Approved" {
		t.Errorf("display = %q, want Approved", got)
	}
}
