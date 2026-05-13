package db

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// seedRunForMemoryTest seeds the minimum FK chain needed to attach a
// run_memory row: events_catalog → entity → event → prompt → task →
// run. Returns the runID. Centralized here so each test reads as
// behavior rather than ceremony.
func seedRunForMemoryTest(t *testing.T, db *sql.DB, runID, entityID string) {
	t.Helper()
	const eventType = "github:pr:opened"
	if _, err := db.Exec(`
		INSERT OR IGNORE INTO entities (id, source, source_id, kind, state)
		VALUES (?, 'github', ?, 'pr', 'active')
	`, entityID, "owner/repo#"+entityID); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	eventID := "ev_" + runID
	if _, err := db.Exec(`
		INSERT INTO events (id, entity_id, event_type, dedup_key)
		VALUES (?, ?, ?, '')
	`, eventID, entityID, eventType); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := db.Exec(`
		INSERT OR IGNORE INTO prompts (id, name, body, creator_user_id, team_id) VALUES ('p_test', 'Test', 'body', '00000000-0000-0000-0000-000000000100', '00000000-0000-0000-0000-000000000010')
	`); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	taskID := "t_" + runID
	if _, err := db.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		VALUES (?, ?, ?, ?, 'completed')
	`, taskID, entityID, eventType, eventID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO runs (id, task_id, prompt_id, status) VALUES (?, ?, 'p_test', 'completed')
	`, runID, taskID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
}

// TestUpsertAgentMemory_RealContent pins the common path: agent wrote
// a real memory file, the gate teardown calls UpsertAgentMemory, the
// row appears with agent_content populated and human_content NULL
// (waiting for SKY-205 / SKY-206 writers).
func TestUpsertAgentMemory_RealContent(t *testing.T) {
	db := newTestDB(t)
	seedRunForMemoryTest(t, db, "r1", "e1")

	if err := UpsertAgentMemory(db, "r1", "e1", "agent wrote this"); err != nil {
		t.Fatalf("UpsertAgentMemory: %v", err)
	}

	var agent sql.NullString
	var human sql.NullString
	if err := db.QueryRow(
		`SELECT agent_content, human_content FROM run_memory WHERE run_id = ?`, "r1",
	).Scan(&agent, &human); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !agent.Valid || agent.String != "agent wrote this" {
		t.Errorf("agent_content = %v, want %q", agent, "agent wrote this")
	}
	if human.Valid {
		t.Errorf("human_content = %q, want NULL", human.String)
	}
}

// TestUpsertAgentMemory_EmptySignalsNonCompliance is the regression
// for the row-presence-as-signal contract: empty and whitespace-only
// content (the signals that the agent didn't pass through the gate)
// both land as SQL NULL, not as a literal empty/whitespace string.
// Downstream consumers (factory's memory_missing derivation) rely on
// the canonical NULL form for "didn't comply" — encoding "" or "  "
// as the actual string would leave them looking like they had real
// memory unless every read site applied a TRIM. Canonicalizing at
// the write side keeps the read side simple.
func TestUpsertAgentMemory_EmptySignalsNonCompliance(t *testing.T) {
	cases := []struct {
		name    string
		runID   string
		content string
	}{
		{"empty", "r2_empty", ""},
		{"whitespace_only", "r2_ws", "   \n\t  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			seedRunForMemoryTest(t, db, tc.runID, "e2_"+tc.name)

			if err := UpsertAgentMemory(db, tc.runID, "e2_"+tc.name, tc.content); err != nil {
				t.Fatalf("UpsertAgentMemory: %v", err)
			}

			var agent sql.NullString
			if err := db.QueryRow(
				`SELECT agent_content FROM run_memory WHERE run_id = ?`, tc.runID,
			).Scan(&agent); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if agent.Valid {
				t.Errorf("agent_content = %q, want NULL (empty/whitespace must canonicalize to NULL)", agent.String)
			}
		})
	}
}

// TestUpsertAgentMemory_IdempotentPreservesHumanContent guards
// SKY-205's invariant before its writers exist: re-running the gate
// (e.g., a retry that produced a memory file the second time)
// overwrites agent_content but leaves human_content intact. Today
// nothing writes human_content, so we seed it via raw UPDATE; the
// test still pins the contract that the upsert won't trample it.
func TestUpsertAgentMemory_IdempotentPreservesHumanContent(t *testing.T) {
	db := newTestDB(t)
	seedRunForMemoryTest(t, db, "r3", "e3")

	if err := UpsertAgentMemory(db, "r3", "e3", "first attempt"); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE run_memory SET human_content = ? WHERE run_id = ?`, "user kept it as-is", "r3",
	); err != nil {
		t.Fatalf("seed human_content: %v", err)
	}
	if err := UpsertAgentMemory(db, "r3", "e3", "second attempt"); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	var agent, human sql.NullString
	if err := db.QueryRow(
		`SELECT agent_content, human_content FROM run_memory WHERE run_id = ?`, "r3",
	).Scan(&agent, &human); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if agent.String != "second attempt" {
		t.Errorf("agent_content = %q, want %q (re-upsert should overwrite)", agent.String, "second attempt")
	}
	if human.String != "user kept it as-is" {
		t.Errorf("human_content = %q, want %q (re-upsert must NOT trample human field)", human.String, "user kept it as-is")
	}
}

// TestGetMemoriesForEntity_MaterializesConcatWhenHumanContentSet
// pins the materialization contract: when both halves of a memory
// row are populated, the concatenated Content field carries the
// agent text, the stable separator, and the human verdict — in that
// order. The next agent's prompt context relies on this shape to
// parse the boundary, so a regression here would silently corrupt
// memory replay.
func TestGetMemoriesForEntity_MaterializesConcatWhenHumanContentSet(t *testing.T) {
	db := newTestDB(t)
	seedRunForMemoryTest(t, db, "r4", "e4")
	if err := UpsertAgentMemory(db, "r4", "e4", "agent reasoning"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE run_memory SET human_content = ? WHERE run_id = ?`, "human verdict", "r4",
	); err != nil {
		t.Fatalf("seed human_content: %v", err)
	}

	memories, err := GetMemoriesForEntity(db, "e4")
	if err != nil {
		t.Fatalf("GetMemoriesForEntity: %v", err)
	}
	if len(memories) != 1 {
		t.Fatalf("memories = %d, want 1", len(memories))
	}
	got := memories[0].Content
	if !strings.HasPrefix(got, "agent reasoning") {
		t.Errorf("Content prefix = %q, want to start with %q", got, "agent reasoning")
	}
	if !strings.Contains(got, "## Human feedback (post-run)") {
		t.Errorf("Content missing separator marker; got %q", got)
	}
	if !strings.HasSuffix(got, "human verdict") {
		t.Errorf("Content suffix = %q, want to end with %q", got, "human verdict")
	}
}

// TestGetMemoriesForEntity_AgentOnlyHasNoSeparator covers the common
// case in this PR (no human writers exist yet): a row with
// agent_content but human_content NULL renders without the separator
// marker, just the agent text. Otherwise every materialized memory
// would carry an empty "## Human feedback (post-run)" section that
// would confuse the next agent.
func TestGetMemoriesForEntity_AgentOnlyHasNoSeparator(t *testing.T) {
	db := newTestDB(t)
	seedRunForMemoryTest(t, db, "r5", "e5")
	if err := UpsertAgentMemory(db, "r5", "e5", "agent reasoning only"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	memories, err := GetMemoriesForEntity(db, "e5")
	if err != nil {
		t.Fatalf("GetMemoriesForEntity: %v", err)
	}
	if len(memories) != 1 {
		t.Fatalf("memories = %d, want 1", len(memories))
	}
	got := memories[0].Content
	if got != "agent reasoning only" {
		t.Errorf("Content = %q, want %q (no separator when human_content is NULL)", got, "agent reasoning only")
	}
}

// TestUpdateRunMemoryHumanContent_LandsOnExistingRow pins the
// contract that SKY-205's writer assumes: the gate teardown's
// upsert at termination guarantees a row exists keyed by run_id, so
// the human-side write is a plain UPDATE that hits exactly one row.
// A regression that drifts UpsertAgentMemory's ON CONFLICT(run_id)
// would surface here as 0 rows affected.
func TestUpdateRunMemoryHumanContent_LandsOnExistingRow(t *testing.T) {
	db := newTestDB(t)
	seedRunForMemoryTest(t, db, "r_human", "e_human")
	if err := UpsertAgentMemory(db, "r_human", "e_human", "agent self-report"); err != nil {
		t.Fatalf("UpsertAgentMemory: %v", err)
	}

	if err := UpdateRunMemoryHumanContent(db, "r_human", "## Human feedback (post-run)\n\nLooks good."); err != nil {
		t.Fatalf("UpdateRunMemoryHumanContent: %v", err)
	}

	var agent, human sql.NullString
	if err := db.QueryRow(
		`SELECT agent_content, human_content FROM run_memory WHERE run_id = ?`, "r_human",
	).Scan(&agent, &human); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if agent.String != "agent self-report" {
		t.Errorf("agent_content trampled: got %q, want %q", agent.String, "agent self-report")
	}
	if !human.Valid || !strings.HasPrefix(human.String, "## Human feedback (post-run)") {
		t.Errorf("human_content not landed: %v", human)
	}
}

// TestUpdateRunMemoryHumanContent_EmptyCanonicalizesToNull mirrors
// UpsertAgentMemory's whitespace handling — empty / whitespace-only
// content collapses to NULL on the way in. This keeps the contract
// symmetric: NULL means "no human verdict captured" rather than
// "the human submitted a blank verdict".
func TestUpdateRunMemoryHumanContent_EmptyCanonicalizesToNull(t *testing.T) {
	db := newTestDB(t)
	seedRunForMemoryTest(t, db, "r_blank", "e_blank")
	if err := UpsertAgentMemory(db, "r_blank", "e_blank", "agent text"); err != nil {
		t.Fatalf("UpsertAgentMemory: %v", err)
	}

	if err := UpdateRunMemoryHumanContent(db, "r_blank", "   \t  \n  "); err != nil {
		t.Fatalf("UpdateRunMemoryHumanContent: %v", err)
	}

	var human sql.NullString
	if err := db.QueryRow(
		`SELECT human_content FROM run_memory WHERE run_id = ?`, "r_blank",
	).Scan(&human); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if human.Valid {
		t.Errorf("human_content = %q, want NULL (whitespace-only must canonicalize)", human.String)
	}
}

// TestUpdateRunMemoryHumanContent_NoRowIsLoggedNotFatal guards the
// non-agent-review path: the handler skips the call when run_id is
// empty, but if some other caller hits it with a runID that has no
// row, returning an error would push a 5xx after the GitHub submit
// already succeeded. Logged-and-nil keeps the response correct.
func TestUpdateRunMemoryHumanContent_NoRowIsLoggedNotFatal(t *testing.T) {
	db := newTestDB(t)
	if err := UpdateRunMemoryHumanContent(db, "no-such-run", "anything"); err != nil {
		t.Errorf("expected nil error on missing row (logged warning); got %v", err)
	}
}

// TestGetRunMemory_NilOnMissingRow keeps the nil-on-missing contract
// from before the rewrite: callers (factory's run summary, the
// resume picker) interpret `(nil, nil)` as "no memory recorded yet,"
// distinct from a returned struct with empty Content. Drift here
// would push them into branching on len(Content) instead of `m == nil`.
func TestGetRunMemory_NilOnMissingRow(t *testing.T) {
	db := newTestDB(t)
	got, err := GetRunMemory(db, "no-such-run")
	if err != nil {
		t.Fatalf("GetRunMemory: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

// _ keeps the domain.TaskMemory import used while the assertions
// above stick to the materialized Content field directly. Pinning
// the type here ensures a future rename of TaskMemory surfaces here
// rather than in ad-hoc reflection downstream.
var _ = domain.TaskMemory{}
