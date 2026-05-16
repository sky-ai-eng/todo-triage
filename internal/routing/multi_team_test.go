package routing

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

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

	router := NewRouter(database, testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, nil, noopScorer{}, websocket.NewHub())

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

// TestHandleEvent_SingleTeam_OneTask is the regression baseline: with
// only one matching rule, exactly one task gets created. Pins that the
// SKY-295 fanout doesn't accidentally spawn multiples on the
// single-team-rule path that local-mode + most multi-mode setups use.
func TestHandleEvent_SingleTeam_OneTask(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	if err := testEventHandlerStore(database).Seed(t.Context(), runmode.LocalDefaultOrg); err != nil {
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

	router := NewRouter(database, testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, nil, noopScorer{}, websocket.NewHub())

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
