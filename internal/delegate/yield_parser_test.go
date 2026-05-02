package delegate

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func TestParseAgentResult_AcceptsYieldEnvelope(t *testing.T) {
	cases := []struct {
		name string
		text string
		typ  string
	}{
		{
			name: "confirmation",
			text: `{"status":"yield","yield":{"type":"confirmation","message":"go?","accept_label":"Yes","reject_label":"No"}}`,
			typ:  domain.YieldTypeConfirmation,
		},
		{
			name: "choice",
			text: `{"status":"yield","yield":{"type":"choice","message":"which?","options":[{"id":"a","label":"A"}],"multi":false}}`,
			typ:  domain.YieldTypeChoice,
		},
		{
			name: "prompt",
			text: `{"status":"yield","yield":{"type":"prompt","message":"name?","placeholder":"x"}}`,
			typ:  domain.YieldTypePrompt,
		},
		{
			name: "with surrounding markdown fences",
			text: "```json\n{\"status\":\"yield\",\"yield\":{\"type\":\"confirmation\",\"message\":\"ok?\"}}\n```",
			typ:  domain.YieldTypeConfirmation,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAgentResult(tc.text)
			if got == nil {
				t.Fatalf("nil parse for %q", tc.text)
			}
			if got.Status != "yield" {
				t.Errorf("status = %q, want yield", got.Status)
			}
			if got.Yield == nil || got.Yield.Type != tc.typ {
				t.Errorf("yield.type = %v, want %q", got.Yield, tc.typ)
			}
		})
	}
}

func TestParseAgentResult_RejectsBadYield(t *testing.T) {
	bad := []string{
		// status:yield but no yield payload
		`{"status":"yield"}`,
		// yield with unknown type
		`{"status":"yield","yield":{"type":"plan_steps","message":"…"}}`,
		// yield with empty type
		`{"status":"yield","yield":{"message":"…"}}`,
	}
	for _, text := range bad {
		if got := parseAgentResult(text); got != nil {
			t.Errorf("expected nil for %q, got %+v", text, got)
		}
	}
}

func TestParseAgentResult_StillAcceptsLegacyCompletion(t *testing.T) {
	got := parseAgentResult(`{"status":"completed","summary":"done"}`)
	if got == nil {
		t.Fatal("legacy completion rejected")
	}
	if got.Summary != "done" {
		t.Errorf("summary = %q, want done", got.Summary)
	}
}
