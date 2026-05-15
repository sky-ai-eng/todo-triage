package db

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func TestMarkAgentRunAwaitingInput_FlipsRunningRow(t *testing.T) {
	database := newTestDB(t)
	entity, _, err := FindOrCreateEntity(database, "github", "owner/repo#1", "pr", "Test", "https://example.com/1")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	eventID, err := RecordEvent(database, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed,
		EntityID:  &entity.ID,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task := seedTaskForTest(t, database, entity.ID, domain.EventGitHubPRCICheckFailed, "build", eventID)
	createPromptForTest(t, database, domain.Prompt{ID: "p", Name: "T", Body: "x", Source: "user"})
	if err := CreateAgentRun(database, domain.AgentRun{ID: "r1", TaskID: task.ID, PromptID: "p", Status: "running", Model: "m"}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := database.Exec(`UPDATE runs SET status = 'running' WHERE id = ?`, "r1"); err != nil {
		t.Fatalf("set running: %v", err)
	}

	ok, err := MarkAgentRunAwaitingInput(database, "r1")
	if err != nil {
		t.Fatalf("MarkAgentRunAwaitingInput: %v", err)
	}
	if !ok {
		t.Fatal("expected flip to succeed on running row")
	}

	// Idempotent against repeat calls — already awaiting_input now.
	ok, err = MarkAgentRunAwaitingInput(database, "r1")
	if err != nil {
		t.Fatalf("repeat call err: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false on repeat call (already awaiting_input)")
	}
}

func TestMarkAgentRunAwaitingInput_RefusesTerminal(t *testing.T) {
	database := newTestDB(t)
	entity, _, err := FindOrCreateEntity(database, "github", "owner/repo#2", "pr", "Test", "https://example.com/2")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	eventID, err := RecordEvent(database, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed,
		EntityID:  &entity.ID,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task := seedTaskForTest(t, database, entity.ID, domain.EventGitHubPRCICheckFailed, "k", eventID)
	createPromptForTest(t, database, domain.Prompt{ID: "p", Name: "T", Body: "x", Source: "user"})

	for _, term := range []string{"completed", "failed", "cancelled", "task_unsolvable", "pending_approval", "taken_over"} {
		runID := "r-" + term
		if err := CreateAgentRun(database, domain.AgentRun{ID: runID, TaskID: task.ID, PromptID: "p", Status: term, Model: "m"}); err != nil {
			t.Fatalf("create run: %v", err)
		}
		if _, err := database.Exec(`UPDATE runs SET status = ? WHERE id = ?`, term, runID); err != nil {
			t.Fatalf("set %s: %v", term, err)
		}
		ok, err := MarkAgentRunAwaitingInput(database, runID)
		if err != nil {
			t.Fatalf("MarkAgentRunAwaitingInput on %s: %v", term, err)
		}
		if ok {
			t.Errorf("expected refusal for terminal status %s, got ok=true", term)
		}
	}
}

func TestMarkAgentRunResuming_OnlyFromAwaitingInput(t *testing.T) {
	database := newTestDB(t)
	entity, _, _ := FindOrCreateEntity(database, "github", "owner/repo#3", "pr", "T", "https://example.com/3")
	eventID, _ := RecordEvent(database, domain.Event{EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entity.ID})
	task := seedTaskForTest(t, database, entity.ID, domain.EventGitHubPRCICheckFailed, "x", eventID)
	createPromptForTest(t, database, domain.Prompt{ID: "p", Name: "T", Body: "x", Source: "user"})

	_ = CreateAgentRun(database, domain.AgentRun{ID: "r-running", TaskID: task.ID, PromptID: "p", Status: "running", Model: "m"})
	_ = CreateAgentRun(database, domain.AgentRun{ID: "r-awaiting", TaskID: task.ID, PromptID: "p", Status: "awaiting_input", Model: "m"})
	_, _ = database.Exec(`UPDATE runs SET status = 'awaiting_input' WHERE id = 'r-awaiting'`)

	// Running row → refuse (not in awaiting_input)
	ok, err := MarkAgentRunResuming(database, "r-running")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Fatal("expected refusal on running row")
	}

	// Awaiting row → success
	ok, err = MarkAgentRunResuming(database, "r-awaiting")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ok {
		t.Fatal("expected success on awaiting_input row")
	}

	// Second resume → refuse
	ok, _ = MarkAgentRunResuming(database, "r-awaiting")
	if ok {
		t.Fatal("expected refusal on second resume")
	}
}

func TestInsertAndLatestYieldRequest_Roundtrip(t *testing.T) {
	database := newTestDB(t)
	entity, _, _ := FindOrCreateEntity(database, "github", "owner/repo#4", "pr", "T", "https://example.com/4")
	eventID, _ := RecordEvent(database, domain.Event{EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entity.ID})
	task := seedTaskForTest(t, database, entity.ID, domain.EventGitHubPRCICheckFailed, "x", eventID)
	createPromptForTest(t, database, domain.Prompt{ID: "p", Name: "T", Body: "x", Source: "user"})
	_ = CreateAgentRun(database, domain.AgentRun{ID: "r1", TaskID: task.ID, PromptID: "p", Status: "running", Model: "m"})

	first := &domain.YieldRequest{Type: domain.YieldTypeConfirmation, Message: "force push?", AcceptLabel: "Yes", RejectLabel: "No"}
	if _, err := InsertYieldRequest(database, "r1", first); err != nil {
		t.Fatalf("insert first: %v", err)
	}
	got, err := LatestYieldRequest(database, "r1")
	if err != nil {
		t.Fatalf("read first: %v", err)
	}
	if got == nil || got.Type != first.Type || got.Message != first.Message {
		t.Fatalf("first roundtrip mismatch: %+v", got)
	}

	// A second yield supersedes the first as "current open."
	second := &domain.YieldRequest{Type: domain.YieldTypeChoice, Message: "approach?", Options: []domain.YieldChoiceOption{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}, Multi: false}
	if _, err := InsertYieldRequest(database, "r1", second); err != nil {
		t.Fatalf("insert second: %v", err)
	}
	got, err = LatestYieldRequest(database, "r1")
	if err != nil {
		t.Fatalf("read second: %v", err)
	}
	if got == nil || got.Type != domain.YieldTypeChoice {
		t.Fatalf("second roundtrip mismatch: %+v", got)
	}
	if len(got.Options) != 2 || got.Options[0].ID != "a" {
		t.Fatalf("options not preserved: %+v", got.Options)
	}

	// No yield → nil, no error.
	got, err = LatestYieldRequest(database, "no-such-run")
	if err != nil {
		t.Fatalf("missing run err: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for missing run, got %+v", got)
	}
}

func TestInsertYieldResponse_StoresDisplayAndPayload(t *testing.T) {
	database := newTestDB(t)
	entity, _, _ := FindOrCreateEntity(database, "github", "owner/repo#5", "pr", "T", "https://example.com/5")
	eventID, _ := RecordEvent(database, domain.Event{EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entity.ID})
	task := seedTaskForTest(t, database, entity.ID, domain.EventGitHubPRCICheckFailed, "x", eventID)
	createPromptForTest(t, database, domain.Prompt{ID: "p", Name: "T", Body: "x", Source: "user"})
	_ = CreateAgentRun(database, domain.AgentRun{ID: "r1", TaskID: task.ID, PromptID: "p", Status: "running", Model: "m"})

	yes := true
	resp := &domain.YieldResponse{Type: domain.YieldTypeConfirmation, Accepted: &yes}
	msg, err := InsertYieldResponse(database, "r1", resp, "Force push")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if msg.Subtype != YieldResponseSubtype {
		t.Errorf("subtype = %q, want %q", msg.Subtype, YieldResponseSubtype)
	}
	if msg.Content != "Force push" {
		t.Errorf("display content = %q, want %q", msg.Content, "Force push")
	}
	if _, ok := msg.Metadata["yield_response"]; !ok {
		t.Error("yield_response missing from metadata")
	}

	// Read back via MessagesForRun and verify metadata persisted.
	all, err := MessagesForRun(database, "r1")
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 message, got %d", len(all))
	}
	if !strings.Contains(string(mustJSON(t, all[0].Metadata)), `"accepted":true`) {
		t.Errorf("metadata round-trip lost accepted: %v", all[0].Metadata)
	}
}

func TestEntitiesWithAwaitingInputRuns_FiltersAndScopes(t *testing.T) {
	database := newTestDB(t)
	e1, _, _ := FindOrCreateEntity(database, "github", "owner/repo#10", "pr", "T", "https://example.com/10")
	e2, _, _ := FindOrCreateEntity(database, "github", "owner/repo#11", "pr", "T", "https://example.com/11")
	e3, _, _ := FindOrCreateEntity(database, "github", "owner/repo#12", "pr", "T", "https://example.com/12")

	mkRun := func(entity, runID, status string) {
		eventID, _ := RecordEvent(database, domain.Event{EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entity})
		task := seedTaskForTest(t, database, entity, domain.EventGitHubPRCICheckFailed, runID, eventID)
		createPromptForTest(t, database, domain.Prompt{ID: "p-" + runID, Name: "T", Body: "x", Source: "user"})
		_ = CreateAgentRun(database, domain.AgentRun{ID: runID, TaskID: task.ID, PromptID: "p-" + runID, Status: status, Model: "m"})
		_, _ = database.Exec(`UPDATE runs SET status = ? WHERE id = ?`, status, runID)
	}

	mkRun(e1.ID, "r1-await", "awaiting_input")
	mkRun(e1.ID, "r1-done", "completed")
	mkRun(e2.ID, "r2-running", "running")
	mkRun(e3.ID, "r3-await", "awaiting_input")

	// Scope to e1, e2 — e3 is also awaiting but not in the filter.
	got, err := EntitiesWithAwaitingInputRuns(database, []string{e1.ID, e2.ID})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if _, ok := got[e1.ID]; !ok {
		t.Errorf("expected e1 in result")
	}
	if _, ok := got[e2.ID]; ok {
		t.Errorf("expected e2 NOT in result (only running)")
	}
	if _, ok := got[e3.ID]; ok {
		t.Errorf("expected e3 NOT in result (out of scope)")
	}

	// Empty input → empty result, no SQL error.
	got, err = EntitiesWithAwaitingInputRuns(database, nil)
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty result, got %d", len(got))
	}
}

func TestAddAgentRunPartialTotals_AccumulatesAcrossYields(t *testing.T) {
	database := newTestDB(t)
	entity, _, _ := FindOrCreateEntity(database, "github", "owner/repo#20", "pr", "T", "https://example.com/20")
	eventID, _ := RecordEvent(database, domain.Event{EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entity.ID})
	task := seedTaskForTest(t, database, entity.ID, domain.EventGitHubPRCICheckFailed, "x", eventID)
	createPromptForTest(t, database, domain.Prompt{ID: "p", Name: "T", Body: "x", Source: "user"})
	_ = CreateAgentRun(database, domain.AgentRun{ID: "r1", TaskID: task.ID, PromptID: "p", Status: "running", Model: "m"})

	if err := AddAgentRunPartialTotals(database, "r1", 0.25, 1500, 5); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := AddAgentRunPartialTotals(database, "r1", 0.5, 2500, 7); err != nil {
		t.Fatalf("second add: %v", err)
	}
	// CompleteAgentRun adds its own totals on top of the partial sums.
	if err := CompleteAgentRun(database, "r1", "completed", 0.1, 500, 2, "stop", "ok"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	run, err := GetAgentRun(database, "r1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if run.TotalCostUSD == nil || *run.TotalCostUSD < 0.849 || *run.TotalCostUSD > 0.851 {
		t.Errorf("cost = %v, want ~0.85", run.TotalCostUSD)
	}
	if run.DurationMs == nil || *run.DurationMs != 4500 {
		t.Errorf("duration_ms = %v, want 4500", run.DurationMs)
	}
	if run.NumTurns == nil || *run.NumTurns != 14 {
		t.Errorf("num_turns = %v, want 14", run.NumTurns)
	}
}

func mustJSON(t *testing.T, v map[string]any) []byte {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}
