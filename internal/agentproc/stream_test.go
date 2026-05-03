package agentproc

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestParseLine_CaptureSessionAndAccumulate exercises the typical
// stream-json sequence: system/init carrying session_id, then an
// assistant message with text content, then a result event. Pinned
// behavior: session_id is set before any messages are emitted so a
// caller can persist it eagerly; messages emit only after stop_reason
// or a new msg id (no premature flushes).
func TestParseLine_CaptureSessionAndAccumulate(t *testing.T) {
	s := NewStreamState()

	if msgs, res := s.ParseLine([]byte(`{"type":"system","subtype":"init","session_id":"sess-abc"}`), "trace-1"); msgs != nil || res != nil {
		t.Fatalf("system/init should not emit messages or result; got msgs=%v res=%v", msgs, res)
	}
	if got := s.SessionID(); got != "sess-abc" {
		t.Errorf("SessionID = %q, want sess-abc", got)
	}

	// Assistant turn with text but no stop_reason — accumulated, no flush.
	if msgs, _ := s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","model":"sonnet","content":[{"type":"text","text":"hello"}]}}`), "trace-1"); len(msgs) != 0 {
		t.Errorf("expected no flush before stop_reason; got %d msgs", len(msgs))
	}

	// Same msg id, now with stop_reason — flushes one assistant msg.
	msgs, _ := s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","stop_reason":"end_turn","content":[]}}`), "trace-1")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 flushed msg on stop_reason; got %d", len(msgs))
	}
	if msgs[0].Role != "assistant" || msgs[0].Content != "hello" || msgs[0].RunID != "trace-1" {
		t.Errorf("flushed message wrong shape: %+v", msgs[0])
	}

	// Terminal result event.
	_, res := s.ParseLine([]byte(`{"type":"result","is_error":false,"duration_ms":120,"num_turns":2,"total_cost_usd":0.01,"stop_reason":"end_turn","result":"{\"status\":\"completed\",\"summary\":\"done\"}"}`), "trace-1")
	if res == nil {
		t.Fatal("expected Result on result event")
	}
	if res.DurationMs != 120 || res.NumTurns != 2 || res.CostUSD != 0.01 || res.StopReason != "end_turn" {
		t.Errorf("result accounting mismatch: %+v", res)
	}
}

func TestParseLine_ToolUseAndToolResult(t *testing.T) {
	s := NewStreamState()
	// Tool use inside assistant turn.
	s.ParseLine([]byte(`{"type":"system","subtype":"init","session_id":"s"}`), "t")
	s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"call-1","name":"Read","input":{"file_path":"/x"}}]}}`), "t")
	flushed, _ := s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","stop_reason":"tool_use","content":[]}}`), "t")
	if len(flushed) != 1 || flushed[0].Subtype != "tool_use" || len(flushed[0].ToolCalls) != 1 {
		t.Fatalf("expected flushed assistant tool_use; got %+v", flushed)
	}
	if flushed[0].ToolCalls[0].Name != "Read" {
		t.Errorf("tool name = %q, want Read", flushed[0].ToolCalls[0].Name)
	}

	// Tool result emitted as a "user" line.
	out, _ := s.ParseLine([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","content":"contents"}]}}`), "t")
	var toolMsg *domain.AgentMessage
	for _, m := range out {
		if m.Role == "tool" {
			toolMsg = m
			break
		}
	}
	if toolMsg == nil {
		t.Fatalf("expected tool message in %+v", out)
	}
	if toolMsg.ToolCallID != "call-1" || toolMsg.Content != "contents" {
		t.Errorf("tool message wrong: %+v", toolMsg)
	}
}

func TestParseLine_IgnoresMalformedJSON(t *testing.T) {
	s := NewStreamState()
	if msgs, res := s.ParseLine([]byte(`not json`), "t"); msgs != nil || res != nil {
		t.Errorf("malformed line should be silently dropped; got msgs=%v res=%v", msgs, res)
	}
}
