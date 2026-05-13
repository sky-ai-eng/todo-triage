package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
	_ "modernc.org/sqlite"
)

// updateScores wraps the SQLite ScoreStore's UpdateTaskScores to keep
// the existing rederive tests terse while the D2 ScoreStore rewrite
// is in flight. Inlining the bundle constructor per call is fine
// for tests (negligible per-test cost) and avoids threading a
// Stores bundle through every helper in this file.
func updateScores(t *testing.T, database *sql.DB, updates []domain.TaskScoreUpdate) error {
	t.Helper()
	return sqlitestore.New(database).Scores.UpdateTaskScores(context.Background(), runmode.LocalDefaultOrg, updates)
}

// noopScorer satisfies the Scorer interface without doing anything.
type noopScorer struct{}

func (noopScorer) Trigger() {}

// newTestDB sets up an in-memory SQLite with schema + seed for integration tests.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })

	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return database
}

// setupReDeriveScenario creates an entity, event, task, trigger, and prompt
// to test the re-derive path. Returns the task ID and trigger ID.
func setupReDeriveScenario(t *testing.T, database *sql.DB, minAutonomy float64) (taskID, triggerID string) {
	t.Helper()

	// Create entity
	entity, _, err := db.FindOrCreateEntity(database, "github", "owner/repo#1", "pr", "Test PR", "https://github.com/owner/repo/pull/1")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	entityID := entity.ID

	// Create event with metadata
	meta := events.GitHubPRCICheckFailedMetadata{
		Author:       "aidan",
		AuthorIsSelf: true,
		CheckName:    "build",
		Repo:         "owner/repo",
	}
	metaJSON, _ := json.Marshal(meta)
	eventID, err := db.RecordEvent(database, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entityID,
		DedupKey:     "build",
		MetadataJSON: string(metaJSON),
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}

	// Create task
	task, _, err := db.FindOrCreateTask(database, entityID, domain.EventGitHubPRCICheckFailed, "build", eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Create prompt
	prompt := domain.Prompt{
		ID:     "test-prompt",
		Name:   "Test",
		Body:   "Do something",
		Source: "user",
	}
	createTestPrompt(t, database, prompt)

	// Create trigger with autonomy threshold
	trigger := domain.EventHandler{
		ID:                     "test-trigger",
		Kind:                   domain.EventHandlerKindTrigger,
		PromptID:               "test-prompt",
		TriggerType:            domain.TriggerTypeEvent,
		EventType:              domain.EventGitHubPRCICheckFailed,
		BreakerThreshold:       intPtr(4),
		MinAutonomySuitability: floatPtr(minAutonomy),
		Enabled:                true,
	}
	createTriggerForTestRouting(t, database, trigger)

	return task.ID, trigger.ID
}

func TestReDeriveAfterScoring_AboveThreshold_Delegates(t *testing.T) {
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)

	// Score the task above threshold
	score := 0.8
	err := updateScores(t, database, []domain.TaskScoreUpdate{{
		ID:                  taskID,
		PriorityScore:       0.5,
		AutonomySuitability: score,
		Summary:             "test",
	}})
	if err != nil {
		t.Fatalf("update scores: %v", err)
	}

	// Create router without spawner — fireDelegate guards nil spawner and
	// returns early, so the task stays queued. The test verifies the full
	// gate-check path runs (suitability >= threshold, predicate matched)
	// without panicking. The log output confirms the trigger fired.
	ws := websocket.NewHub()
	router := NewRouter(database, testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, noopScorer{}, ws)

	router.ReDeriveAfterScoring([]string{taskID})

	// Task stays queued because no spawner is configured, but the trigger
	// matched (visible in log output: "re-derive: task ... firing").
	task, _ := db.GetTask(database, taskID)
	if task == nil {
		t.Fatal("task not found")
	}
	if task.Status != "queued" {
		t.Errorf("expected queued (no spawner), got %s", task.Status)
	}
}

func TestReDeriveAfterScoring_BelowThreshold_Skips(t *testing.T) {
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)

	// Score below threshold
	score := 0.4
	err := updateScores(t, database, []domain.TaskScoreUpdate{{
		ID:                  taskID,
		PriorityScore:       0.5,
		AutonomySuitability: score,
		Summary:             "test",
	}})
	if err != nil {
		t.Fatalf("update scores: %v", err)
	}

	ws := websocket.NewHub()
	router := NewRouter(database, testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, noopScorer{}, ws)
	router.ReDeriveAfterScoring([]string{taskID})

	// Task should remain queued — trigger was skipped
	task, _ := db.GetTask(database, taskID)
	if task == nil {
		t.Fatal("task not found")
	}
	if task.Status != "queued" {
		t.Errorf("expected queued, got %s", task.Status)
	}
}

// TestReDeriveAfterScoring_BotClaimed_Skips covers the post-SKY-261 B+
// shape of "already delegated": the responsibility axis lives on
// claimed_by_agent_id, not on status='delegated' (which is gone).
// Re-derive must skip claim-stamped tasks regardless of their status
// — they're not the re-derive's business. Without this guard, a
// queued-but-bot-claimed task would fire a duplicate firing on every
// re-derive cycle.
func TestReDeriveAfterScoring_BotClaimed_Skips(t *testing.T) {
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)

	// Score above threshold so the re-derive's autonomy gate would
	// otherwise let the trigger fire.
	err := updateScores(t, database, []domain.TaskScoreUpdate{{
		ID:                  taskID,
		PriorityScore:       0.5,
		AutonomySuitability: 0.9,
		Summary:             "test",
	}})
	if err != nil {
		t.Fatalf("update scores: %v", err)
	}

	// Seed the local-sentinel agent row to satisfy the FK on
	// claimed_by_agent_id. setupReDeriveScenario doesn't bootstrap
	// agents — drain_test.go does the same inline seed.
	if _, err := database.Exec(
		`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	// Stamp the bot claim — task stays status='queued' but the
	// responsibility axis is committed.
	if err := db.SetTaskClaimedByAgent(database, taskID, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("stamp agent claim: %v", err)
	}

	ws := websocket.NewHub()
	router := NewRouter(database, testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, noopScorer{}, ws)
	router.ReDeriveAfterScoring([]string{taskID})

	// Task still bot-claimed, no second firing enqueued.
	task, _ := db.GetTask(database, taskID)
	if task.ClaimedByAgentID != runmode.LocalDefaultAgentID {
		t.Errorf("ClaimedByAgentID = %q, want %q (re-derive must not clear claim)", task.ClaimedByAgentID, runmode.LocalDefaultAgentID)
	}
	if task.Status != "queued" {
		t.Errorf("Status = %q, want queued", task.Status)
	}
	firings, err := db.ListPendingFiringsForEntity(database, task.EntityID)
	if err != nil {
		t.Fatalf("list firings: %v", err)
	}
	if len(firings) != 0 {
		t.Errorf("re-derive enqueued %d firing(s) on bot-claimed task; want 0", len(firings))
	}
}

// TestReDeriveAfterScoring_UserClaimed_Skips is the human-side guard:
// when a user has claimed a task ("I'll take this myself") the
// re-derive must not promote it to a bot run, which would also trip
// the XOR CHECK at the DB level (stamping agent claim on top of a
// user claim). The skip happens BEFORE the would-be DB write so the
// XOR is never tested; this test pins that earlier exit.
func TestReDeriveAfterScoring_UserClaimed_Skips(t *testing.T) {
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)

	err := updateScores(t, database, []domain.TaskScoreUpdate{{
		ID:                  taskID,
		PriorityScore:       0.5,
		AutonomySuitability: 0.9,
		Summary:             "test",
	}})
	if err != nil {
		t.Fatalf("update scores: %v", err)
	}

	ok, err := db.ClaimQueuedTaskForUser(database, taskID, runmode.LocalDefaultUserID)
	if err != nil || !ok {
		t.Fatalf("stamp user claim: ok=%v err=%v", ok, err)
	}

	ws := websocket.NewHub()
	router := NewRouter(database, testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, noopScorer{}, ws)
	router.ReDeriveAfterScoring([]string{taskID})

	task, _ := db.GetTask(database, taskID)
	if task.ClaimedByUserID != runmode.LocalDefaultUserID {
		t.Errorf("ClaimedByUserID = %q; want %q (re-derive must not clear user claim)", task.ClaimedByUserID, runmode.LocalDefaultUserID)
	}
	if task.ClaimedByAgentID != "" {
		t.Errorf("ClaimedByAgentID = %q; want empty (re-derive must not steal a user-claimed task)", task.ClaimedByAgentID)
	}
	firings, err := db.ListPendingFiringsForEntity(database, task.EntityID)
	if err != nil {
		t.Fatalf("list firings: %v", err)
	}
	if len(firings) != 0 {
		t.Errorf("re-derive enqueued %d firing(s) on user-claimed task; want 0", len(firings))
	}
}

// TestReDeriveAfterScoring_Snoozed_Skips guards the lifecycle axis:
// status='snoozed' is a "do not act" signal. A snoozed task should
// not be promoted to a bot run by re-derive, even if its score
// crosses the threshold — the wake-on-bump path is the correct path
// to revive the task, not a deferred re-derive.
func TestReDeriveAfterScoring_Snoozed_Skips(t *testing.T) {
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)

	err := updateScores(t, database, []domain.TaskScoreUpdate{{
		ID:                  taskID,
		PriorityScore:       0.5,
		AutonomySuitability: 0.9,
		Summary:             "test",
	}})
	if err != nil {
		t.Fatalf("update scores: %v", err)
	}

	if _, err := database.Exec(
		`UPDATE tasks SET status='snoozed', snooze_until='2099-01-01 00:00:00' WHERE id = ?`,
		taskID,
	); err != nil {
		t.Fatalf("snooze task: %v", err)
	}

	ws := websocket.NewHub()
	router := NewRouter(database, testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, noopScorer{}, ws)
	router.ReDeriveAfterScoring([]string{taskID})

	task, _ := db.GetTask(database, taskID)
	if task.Status != "snoozed" {
		t.Errorf("Status = %q; want snoozed (re-derive must not touch snoozed task)", task.Status)
	}
	if task.ClaimedByAgentID != "" {
		t.Errorf("ClaimedByAgentID = %q; want empty", task.ClaimedByAgentID)
	}
	firings, err := db.ListPendingFiringsForEntity(database, task.EntityID)
	if err != nil {
		t.Fatalf("list firings: %v", err)
	}
	if len(firings) != 0 {
		t.Errorf("re-derive enqueued %d firing(s) on snoozed task; want 0", len(firings))
	}
}

func TestReDeriveAfterScoring_ZeroThresholdTrigger_SkippedByReDerive(t *testing.T) {
	database := newTestDB(t)

	// Create entity + event + task
	entity2, _, _ := db.FindOrCreateEntity(database, "github", "owner/repo#2", "pr", "Test PR 2", "https://github.com/owner/repo/pull/2")
	entityID := entity2.ID
	meta := events.GitHubPRCICheckFailedMetadata{
		Author: "aidan", AuthorIsSelf: true, CheckName: "lint", Repo: "owner/repo",
	}
	metaJSON, _ := json.Marshal(meta)
	eventID, _ := db.RecordEvent(database, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entityID,
		DedupKey: "lint", MetadataJSON: string(metaJSON),
	})
	task, _, _ := db.FindOrCreateTask(database, entityID, domain.EventGitHubPRCICheckFailed, "lint", eventID, 0.5)

	// Prompt
	createTestPrompt(t, database, domain.Prompt{ID: "p2", Name: "Test2", Body: "Do", Source: "user"})

	// Trigger with min_autonomy_suitability=0 (immediate fire, not deferred)
	createTriggerForTestRouting(t, database, domain.EventHandler{
		ID: "t-zero", Kind: domain.EventHandlerKindTrigger,
		PromptID: "p2", TriggerType: domain.TriggerTypeEvent,
		EventType: domain.EventGitHubPRCICheckFailed, BreakerThreshold: intPtr(4),
		MinAutonomySuitability: floatPtr(0.0), Enabled: true,
	})

	// Score the task
	_ = updateScores(t, database, []domain.TaskScoreUpdate{{
		ID: task.ID, PriorityScore: 0.5, AutonomySuitability: 0.9, Summary: "test",
	}})

	ws := websocket.NewHub()
	router := NewRouter(database, testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, noopScorer{}, ws)
	router.ReDeriveAfterScoring([]string{task.ID})

	// Task should remain queued — zero-threshold trigger is skipped in re-derive
	// (it would have fired already in HandleEvent)
	got, _ := db.GetTask(database, task.ID)
	if got.Status != "queued" {
		t.Errorf("expected queued (zero-threshold trigger skipped), got %s", got.Status)
	}
}

func TestReDeriveAfterScoring_PredicateMismatch_Skips(t *testing.T) {
	database := newTestDB(t)

	// Create entity + event where author_is_self=false
	entity3, _, _ := db.FindOrCreateEntity(database, "github", "owner/repo#3", "pr", "Test PR 3", "https://github.com/owner/repo/pull/3")
	entityID := entity3.ID
	meta := events.GitHubPRCICheckFailedMetadata{
		Author: "someone-else", AuthorIsSelf: false, CheckName: "build", Repo: "owner/repo",
	}
	metaJSON, _ := json.Marshal(meta)
	eventID, _ := db.RecordEvent(database, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entityID,
		DedupKey: "build", MetadataJSON: string(metaJSON),
	})
	task, _, _ := db.FindOrCreateTask(database, entityID, domain.EventGitHubPRCICheckFailed, "build", eventID, 0.5)

	// Prompt
	createTestPrompt(t, database, domain.Prompt{ID: "p3", Name: "Test3", Body: "Do", Source: "user"})

	// Trigger with predicate requiring author_is_self=true
	pred := `{"author_is_self":true}`
	createTriggerForTestRouting(t, database, domain.EventHandler{
		ID: "t-pred", Kind: domain.EventHandlerKindTrigger,
		PromptID: "p3", TriggerType: domain.TriggerTypeEvent,
		EventType: domain.EventGitHubPRCICheckFailed, BreakerThreshold: intPtr(4),
		MinAutonomySuitability: floatPtr(0.5), Enabled: true,
		ScopePredicateJSON: &pred,
	})

	// Score above threshold
	_ = updateScores(t, database, []domain.TaskScoreUpdate{{
		ID: task.ID, PriorityScore: 0.5, AutonomySuitability: 0.9, Summary: "test",
	}})

	ws := websocket.NewHub()
	router := NewRouter(database, testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, noopScorer{}, ws)
	router.ReDeriveAfterScoring([]string{task.ID})

	// Task should stay queued — predicate doesn't match
	got, _ := db.GetTask(database, task.ID)
	if got.Status != "queued" {
		t.Errorf("expected queued (predicate mismatch), got %s", got.Status)
	}
}
