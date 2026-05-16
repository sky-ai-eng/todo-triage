package routing

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/config"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// TestHandleEvent_MultipleTeams_FansOut pins the SKY-295 invariant:
// when one event matches rules belonging to N different teams, the
// router creates N tasks — one per team — instead of collapsing them
// onto a single (entity, event_type, dedup_key) row keyed off the
// arbitrary "oldest team in org" fallback the pre-SKY-295 SQL had.
func TestHandleEvent_MultipleTeams_FansOut(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)

	// Seed a second team alongside LocalDefaultTeamID.
	teamA := runmode.LocalDefaultTeamID
	teamB := uuid.New().String()
	if _, err := database.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
		teamB, runmode.LocalDefaultOrgID, "team-b", "Team B",
	); err != nil {
		t.Fatalf("seed team B: %v", err)
	}

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#multi", "pr", "Multi-team PR", "https://example.com/multi")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	// Two user-source rules on the same event, one per team. Both
	// match the metadata (empty predicate = match all), so the
	// router's per-team fanout should create one task per team.
	for i, teamID := range []string{teamA, teamB} {
		_ = i
		ruleID := uuid.New().String()
		if _, err := database.Exec(`
			INSERT INTO event_handlers
				(id, org_id, team_id, creator_user_id, visibility, kind, event_type,
				 scope_predicate_json, enabled, source,
				 name, default_priority, sort_order,
				 created_at, updated_at)
			VALUES (?, ?, ?, ?, 'team', 'rule', ?,
			        NULL, 1, 'user',
			        ?, 0.7, 100,
			        ?, ?)
		`, ruleID, runmode.LocalDefaultOrg, teamID, runmode.LocalDefaultUserID, domain.EventGitHubPRCICheckFailed,
			"CI rule "+teamID[:8], time.Now(), time.Now()); err != nil {
			t.Fatalf("seed rule for team %s: %v", teamID, err)
		}
	}

	meta := events.GitHubPRCICheckFailedMetadata{
		Author:    "aidan",
		CheckName: "build",
		Repo:      "owner/repo",
		HeadSHA:   "abc123",
	}
	metaJSON, _ := json.Marshal(meta)

	router := NewRouter(testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, nil, noopScorer{}, websocket.NewHub())

	router.HandleEvent(domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		DedupKey:     "build",
		MetadataJSON: string(metaJSON),
		CreatedAt:    time.Now(),
	})

	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrg, entity.ID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("expected 2 active tasks (one per team), got %d", len(active))
	}
	// IDs must be distinct (the whole point of the per-team index change).
	if active[0].ID == active[1].ID {
		t.Errorf("expected distinct task IDs across teams; got duplicates")
	}
}

// TestHandleEvent_BackfillCreatedAt_PreservesOccurredAt pins SKY-295
// (P1.2): when an event arrives with a non-zero OccurredAt (e.g. the
// tracker's review-requested backfill stamped with the PR's
// CreatedAt), the task's created_at should reflect when the event
// happened, not when the router processed it. Without this the queue
// ordering would treat a week-old review request as "just discovered"
// and surface it above genuinely new work.
func TestHandleEvent_BackfillCreatedAt_PreservesOccurredAt(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	if err := testEventHandlerStore(database).Seed(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("seed event handlers: %v", err)
	}

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#backfill", "pr", "Stale PR", "https://example.com/backfill")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	// 14 days ago — typical "PR has been awaiting your review for
	// weeks" backfill case.
	occurred := time.Now().Add(-14 * 24 * time.Hour).Truncate(time.Second)

	meta := events.GitHubPRReviewRequestedMetadata{
		Author:   "alice",
		Repo:     "owner/repo",
		PRNumber: 7,
		HeadSHA:  "abc123",
	}
	metaJSON, _ := json.Marshal(meta)

	router := NewRouter(testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, nil, noopScorer{}, websocket.NewHub())

	router.HandleEvent(domain.Event{
		EventType:    domain.EventGitHubPRReviewRequested,
		EntityID:     &entity.ID,
		MetadataJSON: string(metaJSON),
		OccurredAt:   occurred,
	})

	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrg, entity.ID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 active task, got %d (err=%v)", len(active), err)
	}
	if !active[0].CreatedAt.Equal(occurred) {
		t.Errorf("task.CreatedAt = %v, want OccurredAt %v (backfill timestamp regression)", active[0].CreatedAt, occurred)
	}
}

// TestHandleEvent_NoOccurredAt_FallsBackToNow ensures the OccurredAt
// path doesn't accidentally land zero times on poll-detected events
// that legitimately omit the field. Sanity-check companion to the
// backfill test above.
func TestHandleEvent_NoOccurredAt_FallsBackToNow(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	if err := testEventHandlerStore(database).Seed(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("seed event handlers: %v", err)
	}

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#now", "pr", "Now PR", "https://example.com/now")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	meta := events.GitHubPRCICheckFailedMetadata{
		Author:    "aidan",
		CheckName: "build",
		Repo:      "owner/repo",
		HeadSHA:   "abc123",
	}
	metaJSON, _ := json.Marshal(meta)

	before := time.Now()
	router := NewRouter(testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, nil, noopScorer{}, websocket.NewHub())
	router.HandleEvent(domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		DedupKey:     "build",
		MetadataJSON: string(metaJSON),
		// OccurredAt deliberately zero.
	})

	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrg, entity.ID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 active task, got %d (err=%v)", len(active), err)
	}
	if active[0].CreatedAt.Before(before) {
		t.Errorf("task.CreatedAt = %v predates router invocation (%v); want time.Now() fallback", active[0].CreatedAt, before)
	}
}

// TestHandleEvent_BecameAtomic_PerTeam pins SKY-295 (P1.4): when team
// A has an active task on a Jira entity and team B's rule also
// matches a later jira:issue:became_atomic event, team B must still
// get its task. The pre-SKY-295 guard's "any active task on the
// entity → bail" check over-suppressed across teams.
func TestHandleEvent_BecameAtomic_PerTeam(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)

	teamA := runmode.LocalDefaultTeamID
	teamB := uuid.New().String()
	if _, err := database.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
		teamB, runmode.LocalDefaultOrgID, "team-b-atomic", "Team B Atomic",
	); err != nil {
		t.Fatalf("seed team B: %v", err)
	}

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "jira", "SKY-700", "issue", "Cross-team atomic", "https://jira.example.com/browse/SKY-700")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	// Pre-seed: team A already has an assigned task on this entity.
	priorEventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrg, domain.Event{
		EventType:    domain.EventJiraIssueAssigned,
		EntityID:     &entity.ID,
		MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("record prior event: %v", err)
	}
	if _, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrg, teamA, entity.ID, domain.EventJiraIssueAssigned, "", priorEventID, 0.5); err != nil {
		t.Fatalf("create teamA prior task: %v", err)
	}

	// Both teams have a rule matching became_atomic.
	for _, teamID := range []string{teamA, teamB} {
		ruleID := uuid.New().String()
		if _, err := database.Exec(`
			INSERT INTO event_handlers
				(id, org_id, team_id, creator_user_id, visibility, kind, event_type,
				 scope_predicate_json, enabled, source,
				 name, default_priority, sort_order,
				 created_at, updated_at)
			VALUES (?, ?, ?, ?, 'team', 'rule', ?,
			        NULL, 1, 'user',
			        ?, 0.7, 100, ?, ?)
		`, ruleID, runmode.LocalDefaultOrg, teamID, runmode.LocalDefaultUserID, domain.EventJiraIssueBecameAtomic,
			"became_atomic "+teamID[:8], time.Now(), time.Now()); err != nil {
			t.Fatalf("seed rule for team %s: %v", teamID, err)
		}
	}

	atomicMeta := events.JiraIssueBecameAtomicMetadata{
		Assignee:          "aidan",
		AssigneeAccountID: "557058:abc-aidan",
		IssueKey:          "SKY-700",
		Project:           "SKY",
	}
	atomicJSON, _ := json.Marshal(atomicMeta)

	router := NewRouter(testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, nil, noopScorer{}, websocket.NewHub())
	router.HandleEvent(domain.Event{
		EventType:    domain.EventJiraIssueBecameAtomic,
		EntityID:     &entity.ID,
		MetadataJSON: string(atomicJSON),
	})

	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrg, entity.ID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	// Expect 2 tasks: team A's pre-existing assigned task (kept, not
	// duplicated by became_atomic), and team B's new became_atomic
	// task (the per-team fanout did its job).
	if len(active) != 2 {
		t.Fatalf("expected 2 active tasks (teamA assigned + teamB became_atomic), got %d", len(active))
	}
	teamCount := map[string]int{}
	for _, a := range active {
		teamCount[a.TeamID]++
	}
	if teamCount[teamA] != 1 {
		t.Errorf("team A active task count = %d, want 1 (assigned task preserved, became_atomic suppressed for team A)", teamCount[teamA])
	}
	if teamCount[teamB] != 1 {
		t.Errorf("team B active task count = %d, want 1 (became_atomic should fire for team B even though team A had an active task)", teamCount[teamB])
	}
}

// TestTryAutoDelegate_PerTeamBotGate pins the SKY-295 P1 reviewer
// catch: tryAutoDelegate's team_agents gate must read THIS task's
// team's row, not the local sentinel. Pre-fix the lookup was
// hardcoded to runmode.LocalDefaultTeamID, so in a two-team org
// where team B disabled the bot, team B's task would still
// auto-delegate by reading team A's flag (and vice-versa).
func TestTryAutoDelegate_PerTeamBotGate(t *testing.T) {
	database := newTestDB(t)
	if err := config.Init(database); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	// Seed two teams. teamA = LocalDefaultTeamID is already in the
	// schema; teamB is added explicitly.
	teamA := runmode.LocalDefaultTeamID
	teamB := uuid.New().String()
	if _, err := database.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
		teamB, runmode.LocalDefaultOrgID, "team-b-bot-gate", "Team B Gate",
	); err != nil {
		t.Fatalf("seed team B: %v", err)
	}

	// Seed agent + per-team enabled flags: team A enabled, team B
	// disabled. The reviewer's bug would read team A's flag (enabled)
	// for both teams, so team B's task would slip through the gate.
	stores := sqlitestore.New(database)
	if _, err := database.Exec(
		`INSERT INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if err := stores.TeamAgents.AddForTeam(t.Context(), runmode.LocalDefaultOrg, teamA, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("add agent to team A: %v", err)
	}
	if err := stores.TeamAgents.AddForTeam(t.Context(), runmode.LocalDefaultOrg, teamB, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("add agent to team B: %v", err)
	}
	if err := stores.TeamAgents.SetEnabled(t.Context(), runmode.LocalDefaultOrg, teamB, runmode.LocalDefaultAgentID, false); err != nil {
		t.Fatalf("disable agent for team B: %v", err)
	}

	// Entity, event, two tasks (one per team), prompt, trigger per team.
	entity, _, err := stores.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#gate", "pr", "Gate PR", "https://example.com/gate")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	createTestPrompt(t, database, domain.Prompt{ID: "p-gate", Name: "Gate", Body: "x", Source: "user"})
	eventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrg, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		DedupKey:     "build",
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	taskA, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrg, teamA, entity.ID, domain.EventGitHubPRCICheckFailed, "build", eventID, 0.5)
	if err != nil {
		t.Fatalf("create team A task: %v", err)
	}
	taskB, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrg, teamB, entity.ID, domain.EventGitHubPRCICheckFailed, "build", eventID, 0.5)
	if err != nil {
		t.Fatalf("create team B task: %v", err)
	}

	trigger := domain.EventHandler{
		ID:                     "trigger-gate",
		Kind:                   domain.EventHandlerKindTrigger,
		PromptID:               "p-gate",
		TriggerType:            domain.TriggerTypeEvent,
		EventType:              domain.EventGitHubPRCICheckFailed,
		BreakerThreshold:       intPtr(4),
		MinAutonomySuitability: floatPtr(0),
		Enabled:                true,
	}
	createTriggerForTestRouting(t, database, trigger)

	stub := &stubDelegator{db: database}
	router := NewRouter(testPromptStore(database), testEventHandlerStore(database), stores.Agents, stores.TeamAgents, nil, testTaskStore(database), stores.AgentRuns, stores.Entities, stores.PendingFirings, stores.Events, stub, noopScorer{}, websocket.NewHub())

	// Direct calls bypass the (config-gated) HandleEvent step 9 to
	// keep the assertion focused on tryAutoDelegate's gate. The
	// HandleEvent step-9 wrapper just iterates and dispatches; the
	// gate logic itself is what we need to pin.
	router.tryAutoDelegate(taskA, trigger, entity.ID, eventID)
	router.tryAutoDelegate(taskB, trigger, entity.ID, eventID)

	// Team A's task should have been delegated; team B's task should
	// have been blocked by the bot-disabled-for-team gate. With the
	// reviewer's bug both would have fired (gate hardcoded to team A's
	// flag = enabled) or both would have been blocked (depending on
	// which team's row LocalDefaultTeamID happens to be).
	if stub.calls != 1 {
		t.Fatalf("expected exactly 1 Delegate call (team A only); got %d", stub.calls)
	}
	gotA, _ := testTaskStore(database).Get(t.Context(), runmode.LocalDefaultOrg, taskA.ID)
	gotB, _ := testTaskStore(database).Get(t.Context(), runmode.LocalDefaultOrg, taskB.ID)
	if gotA.ClaimedByAgentID == "" {
		t.Errorf("team A task: ClaimedByAgentID empty; expected agent claim after successful fire")
	}
	if gotB.ClaimedByAgentID != "" {
		t.Errorf("team B task: ClaimedByAgentID = %q; expected empty (bot-disabled gate should have blocked the fire)", gotB.ClaimedByAgentID)
	}
}

// TestHandleEvent_SingleTeam_OneTask is the regression baseline: with
// only one matching rule, exactly one task gets created. Pins that the
// SKY-295 fanout doesn't accidentally spawn multiples on the
// single-team-rule path that local-mode + most multi-mode setups use.
func TestHandleEvent_SingleTeam_OneTask(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	if err := testEventHandlerStore(database).Seed(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("seed event handlers: %v", err)
	}

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#single", "pr", "Single team PR", "https://example.com/single")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	meta := events.GitHubPRCICheckFailedMetadata{
		Author:    "aidan",
		CheckName: "build",
		Repo:      "owner/repo",
		HeadSHA:   "abc123",
	}
	metaJSON, _ := json.Marshal(meta)

	router := NewRouter(testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, nil, noopScorer{}, websocket.NewHub())

	router.HandleEvent(domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		DedupKey:     "build",
		MetadataJSON: string(metaJSON),
		CreatedAt:    time.Now(),
	})

	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrg, entity.ID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(active) != 1 {
		t.Errorf("expected exactly 1 active task with one matching rule, got %d", len(active))
	}
	if len(active) >= 1 && active[0].EventType != domain.EventGitHubPRCICheckFailed {
		t.Errorf("task event_type=%q, want %q", active[0].EventType, domain.EventGitHubPRCICheckFailed)
	}
}
