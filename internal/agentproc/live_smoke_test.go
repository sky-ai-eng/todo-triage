package agentproc

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestSDK_LiveSmoke is a live end-to-end smoke test against the real
// Agent SDK runtime. Skipped by default; opt in with
//
//	TF_TEST_SDK_LIVE=1 go test ./internal/agentproc -run TestSDK_LiveSmoke -v
//
// First run installs the SDK + Node deps under ~/.triagefactory/sdk
// (a few hundred MB, ~30s). Subsequent runs are fast.
//
// Bills against whatever auth the user's environment resolves —
// typically the local Claude Code OAuth login (Pro/Max subscription).
//
// Exists as a manual verification gate while we migrate the runtime
// from the `claude` CLI to the SDK; remove or fold into a higher-level
// integration suite once the migration ships.
func TestSDK_LiveSmoke(t *testing.T) {
	if os.Getenv("TF_TEST_SDK_LIVE") != "1" {
		t.Skip("set TF_TEST_SDK_LIVE=1 to run the live SDK smoke test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	sink := &captureSink{}
	outcome, err := Run(ctx, RunOptions{
		Cwd:      t.TempDir(),
		Message:  "Reply with exactly the word PONG and nothing else.",
		MaxTurns: 1,
		TraceID:  "live-smoke",
	}, sink)
	if err != nil {
		t.Fatalf("Run failed: %v\nstderr: %s", err, outcomeStderr(outcome))
	}
	if outcome == nil || outcome.Result == nil {
		t.Fatalf("expected terminal Result, got %+v\nstderr: %s", outcome, outcomeStderr(outcome))
	}
	if outcome.SessionID == "" {
		t.Errorf("expected non-empty session id (system/init missing or stream.go regressed)")
	}
	if !strings.Contains(outcome.Result.Result, "PONG") {
		t.Errorf("expected PONG in result, got %q", outcome.Result.Result)
	}
	if assistantMsg := findFirstAssistant(sink.messages); assistantMsg == nil {
		t.Errorf("expected at least one assistant message in the sink")
	}
	t.Logf("live SDK smoke ok: session=%s cost_usd=%.6f turns=%d", outcome.SessionID, outcome.Result.CostUSD, outcome.Result.NumTurns)
}

func outcomeStderr(o *Outcome) string {
	if o == nil {
		return ""
	}
	return o.Stderr
}

func findFirstAssistant(msgs []*domain.AgentMessage) *domain.AgentMessage {
	for _, m := range msgs {
		if m.Role == "assistant" {
			return m
		}
	}
	return nil
}
