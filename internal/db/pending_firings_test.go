package db

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func TestEnqueuePendingFiring_Insert(t *testing.T) {
	database := newTestDB(t)

	entity, _, err := FindOrCreateEntity(database, "github", "owner/repo#1", "pr", "Test", "https://example.com/1")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	if err := CreatePrompt(database, domain.Prompt{ID: "p1", Name: "P", Body: "x", Source: "user"}); err != nil {
		t.Fatalf("create prompt: %v", err)
	}
	eventID, err := RecordEvent(database, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entity.ID, MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := FindOrCreateTask(database, entity.ID, domain.EventGitHubPRCICheckFailed, "build", eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := SavePromptTrigger(database, domain.PromptTrigger{
		ID: "t1", PromptID: "p1", TriggerType: domain.TriggerTypeEvent,
		EventType: domain.EventGitHubPRCICheckFailed, BreakerThreshold: 4, Enabled: true,
	}); err != nil {
		t.Fatalf("save trigger: %v", err)
	}

	inserted, err := EnqueuePendingFiring(database, entity.ID, task.ID, "t1", eventID)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if !inserted {
		t.Fatal("first enqueue should report inserted=true")
	}

	// Second enqueue with same (task_id, trigger_id) → collapse to no-op.
	inserted, err = EnqueuePendingFiring(database, entity.ID, task.ID, "t1", eventID)
	if err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if inserted {
		t.Error("duplicate enqueue should report inserted=false (collapse)")
	}

	all, err := ListPendingFiringsForEntity(database, entity.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("expected 1 row after collapse, got %d", len(all))
	}
}

func TestPopPendingFiring_FIFO(t *testing.T) {
	database := newTestDB(t)
	entity, _, _ := FindOrCreateEntity(database, "github", "owner/repo#1", "pr", "Test", "https://example.com/1")
	CreatePrompt(database, domain.Prompt{ID: "p1", Name: "P", Body: "x", Source: "user"})
	eventID, _ := RecordEvent(database, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entity.ID, MetadataJSON: `{"check_name":"build"}`,
	})

	// Two distinct (task, trigger) pairs queued in order. Triggers need
	// distinct prompts because (prompt_id, event_type, trigger_type) is
	// a unique index on prompt_triggers.
	if err := CreatePrompt(database, domain.Prompt{ID: "p2", Name: "P2", Body: "y", Source: "user"}); err != nil {
		t.Fatalf("create p2: %v", err)
	}
	taskA, _, _ := FindOrCreateTask(database, entity.ID, domain.EventGitHubPRCICheckFailed, "buildA", eventID, 0.5)
	taskB, _, _ := FindOrCreateTask(database, entity.ID, domain.EventGitHubPRCICheckFailed, "buildB", eventID, 0.5)
	SavePromptTrigger(database, domain.PromptTrigger{
		ID: "tA", PromptID: "p1", TriggerType: domain.TriggerTypeEvent,
		EventType: domain.EventGitHubPRCICheckFailed, BreakerThreshold: 4, Enabled: true,
	})
	SavePromptTrigger(database, domain.PromptTrigger{
		ID: "tB", PromptID: "p2", TriggerType: domain.TriggerTypeEvent,
		EventType: domain.EventGitHubPRCICheckFailed, BreakerThreshold: 4, Enabled: true,
	})

	if _, err := EnqueuePendingFiring(database, entity.ID, taskA.ID, "tA", eventID); err != nil {
		t.Fatalf("enqueue A: %v", err)
	}
	if _, err := EnqueuePendingFiring(database, entity.ID, taskB.ID, "tB", eventID); err != nil {
		t.Fatalf("enqueue B: %v", err)
	}

	first, err := PopPendingFiringForEntity(database, entity.ID)
	if err != nil {
		t.Fatalf("pop: %v", err)
	}
	if first == nil || first.TaskID != taskA.ID {
		t.Errorf("expected oldest (task A) first, got %+v", first)
	}

	// Pop is non-mutating — same row returned again until marked.
	again, _ := PopPendingFiringForEntity(database, entity.ID)
	if again == nil || again.ID != first.ID {
		t.Error("pop should be non-mutating")
	}

	// Mark fired; next pop returns task B. fired_run_id has an FK to
	// runs, so create a real run first.
	if err := CreateAgentRun(database, domain.AgentRun{
		ID: "run-A", TaskID: taskA.ID, PromptID: "p1", Status: "running",
		Model: "x", TriggerType: "event",
	}); err != nil {
		t.Fatalf("create run A: %v", err)
	}
	if err := MarkPendingFiringFired(database, first.ID, "run-A"); err != nil {
		t.Fatalf("mark fired: %v", err)
	}
	second, _ := PopPendingFiringForEntity(database, entity.ID)
	if second == nil || second.TaskID != taskB.ID {
		t.Errorf("expected task B after marking A fired, got %+v", second)
	}

	// Mark skipped; queue empty.
	if err := MarkPendingFiringSkipped(database, second.ID, domain.PendingFiringSkipTaskClosed); err != nil {
		t.Fatalf("mark skipped: %v", err)
	}
	empty, _ := PopPendingFiringForEntity(database, entity.ID)
	if empty != nil {
		t.Errorf("expected empty queue, got %+v", empty)
	}
}

func TestEntityCanFireImmediately_GateLogic(t *testing.T) {
	database := newTestDB(t)
	entity, _, _ := FindOrCreateEntity(database, "github", "owner/repo#1", "pr", "Test", "https://example.com/1")
	CreatePrompt(database, domain.Prompt{ID: "p1", Name: "P", Body: "x", Source: "user"})
	eventID, _ := RecordEvent(database, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entity.ID, MetadataJSON: `{"check_name":"build"}`,
	})
	task, _, _ := FindOrCreateTask(database, entity.ID, domain.EventGitHubPRCICheckFailed, "build", eventID, 0.5)
	SavePromptTrigger(database, domain.PromptTrigger{
		ID: "t1", PromptID: "p1", TriggerType: domain.TriggerTypeEvent,
		EventType: domain.EventGitHubPRCICheckFailed, BreakerThreshold: 4, Enabled: true,
	})

	// Empty: gate open.
	can, err := EntityCanFireImmediately(database, entity.ID)
	if err != nil {
		t.Fatalf("gate query: %v", err)
	}
	if !can {
		t.Error("empty entity should be fireable")
	}

	// Pending firing in queue → gate closed (FIFO fairness).
	if _, err := EnqueuePendingFiring(database, entity.ID, task.ID, "t1", eventID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	can, _ = EntityCanFireImmediately(database, entity.ID)
	if can {
		t.Error("entity with pending firing should not be fireable (FIFO)")
	}

	// Drain it (mark fired) — fired_run_id has an FK to runs, so the run
	// must exist first. Mark it completed immediately so the gate sees no
	// active auto run.
	if err := CreateAgentRun(database, domain.AgentRun{
		ID: "r-drained", TaskID: task.ID, PromptID: "p1", Status: "running",
		Model: "x", TriggerType: "event",
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	first, _ := PopPendingFiringForEntity(database, entity.ID)
	if err := MarkPendingFiringFired(database, first.ID, "r-drained"); err != nil {
		t.Fatalf("mark fired: %v", err)
	}
	if _, err := database.Exec(`UPDATE runs SET status = 'completed' WHERE id = 'r-drained'`); err != nil {
		t.Fatalf("complete run: %v", err)
	}
	can, _ = EntityCanFireImmediately(database, entity.ID)
	if !can {
		t.Error("empty queue + no auto runs should be fireable")
	}

	// Active auto run (trigger_type='event') → gate closed.
	if err := CreateAgentRun(database, domain.AgentRun{
		ID: "r-active", TaskID: task.ID, PromptID: "p1", Status: "running",
		Model: "x", TriggerType: "event",
	}); err != nil {
		t.Fatalf("create active run: %v", err)
	}
	can, _ = EntityCanFireImmediately(database, entity.ID)
	if can {
		t.Error("active auto run should close the gate")
	}

	// Complete the auto run; spin up a manual run; gate stays open per SKY-189.
	if _, err := database.Exec(`UPDATE runs SET status = 'completed' WHERE id = 'r-active'`); err != nil {
		t.Fatalf("complete active: %v", err)
	}
	if err := CreateAgentRun(database, domain.AgentRun{
		ID: "r-manual", TaskID: task.ID, PromptID: "p1", Status: "running",
		Model: "x", TriggerType: "manual",
	}); err != nil {
		t.Fatalf("create manual run: %v", err)
	}
	can, _ = EntityCanFireImmediately(database, entity.ID)
	if !can {
		t.Error("manual run should not close the auto-firing gate")
	}
}
