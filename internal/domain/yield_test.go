package domain

import (
	"strings"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

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
