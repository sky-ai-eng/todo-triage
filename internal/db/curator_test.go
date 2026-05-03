package db

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func seedProjectForCurator(t *testing.T, database *sql.DB) string {
	t.Helper()
	id, err := CreateProject(database, domain.Project{Name: "Curator test project"})
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return id
}

func TestCreateCuratorRequest_RoundtripDefaults(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)

	id, err := CreateCuratorRequest(database, projectID, "what's up")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == "" {
		t.Fatal("empty request id")
	}

	got, err := GetCuratorRequest(database, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("expected request, got nil")
	}
	if got.Status != "queued" {
		t.Errorf("status = %q, want queued", got.Status)
	}
	if got.UserInput != "what's up" {
		t.Errorf("user_input = %q", got.UserInput)
	}
	if got.StartedAt != nil || got.FinishedAt != nil {
		t.Errorf("started/finished should be nil for queued; got %v / %v", got.StartedAt, got.FinishedAt)
	}
	if got.IsTerminal() {
		t.Errorf("queued should not be terminal")
	}
}

func TestMarkCuratorRequestRunning_SecondCallNoOps(t *testing.T) {
	// Pin: the goroutine's pickup is the only legitimate queued→
	// running transition. A second pickup attempt (e.g., a duplicate
	// dispatch) must error out so the caller knows the row was
	// already claimed, rather than silently re-stamping started_at.
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)
	id, _ := CreateCuratorRequest(database, projectID, "hi")

	if err := MarkCuratorRequestRunning(database, id); err != nil {
		t.Fatalf("first running: %v", err)
	}
	err := MarkCuratorRequestRunning(database, id)
	if err != sql.ErrNoRows {
		t.Errorf("second running call: want sql.ErrNoRows, got %v", err)
	}
}

func TestMarkCuratorRequestCancelledIfActive_TerminalRowsLeftAlone(t *testing.T) {
	// The cancel endpoint and the project-delete handler both call
	// this from outside the per-project goroutine. The status filter
	// must keep them from clobbering a row the goroutine just
	// finished cleanly.
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)

	id, _ := CreateCuratorRequest(database, projectID, "x")
	if err := CompleteCuratorRequest(database, id, "done", "", 0.42, 1500, 3); err != nil {
		t.Fatalf("complete: %v", err)
	}

	flipped, err := MarkCuratorRequestCancelledIfActive(database, id, "user cancelled")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if flipped {
		t.Errorf("done row should not flip to cancelled")
	}

	got, _ := GetCuratorRequest(database, id)
	if got.Status != "done" {
		t.Errorf("status = %q, want done", got.Status)
	}
	if got.CostUSD != 0.42 || got.NumTurns != 3 {
		t.Errorf("accounting clobbered: %+v", got)
	}
}

func TestMarkCuratorRequestCancelledIfActive_FlipsRunningRow(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)
	id, _ := CreateCuratorRequest(database, projectID, "x")
	_ = MarkCuratorRequestRunning(database, id)

	flipped, err := MarkCuratorRequestCancelledIfActive(database, id, "user cancelled")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !flipped {
		t.Fatal("running row should flip to cancelled")
	}
	got, _ := GetCuratorRequest(database, id)
	if got.Status != "cancelled" || got.ErrorMsg != "user cancelled" {
		t.Errorf("post-cancel: %+v", got)
	}
	if got.FinishedAt == nil {
		t.Error("finished_at not stamped on cancel")
	}
}

func TestInFlightCuratorRequestForProject_PrefersRunning(t *testing.T) {
	// When both queued + running rows exist (transient state during
	// goroutine pickup), the cancel endpoint should target the
	// running one — otherwise we cancel the next message and let
	// the active one keep going.
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)

	queuedID, _ := CreateCuratorRequest(database, projectID, "first")
	_ = MarkCuratorRequestRunning(database, queuedID)
	_, _ = CreateCuratorRequest(database, projectID, "second") // queued

	got, err := InFlightCuratorRequestForProject(database, projectID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got == nil || got.ID != queuedID {
		t.Errorf("expected running row %q, got %+v", queuedID, got)
	}
}

func TestInFlightCuratorRequestForProject_NoneReturnsNil(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)
	id, _ := CreateCuratorRequest(database, projectID, "x")
	_ = CompleteCuratorRequest(database, id, "done", "", 0, 0, 0)

	got, err := InFlightCuratorRequestForProject(database, projectID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for project with only terminal rows, got %+v", got)
	}
}

func TestCancelOrphanedRunningCuratorRequests_FlipsOnlyRunningRows(t *testing.T) {
	// Pin the startup recovery contract: rows in `running` get
	// cancelled (their goroutine is gone with the previous process);
	// queued rows are left alone (the new process can pick them up);
	// terminal rows are untouched.
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)

	runningID, _ := CreateCuratorRequest(database, projectID, "running")
	_ = MarkCuratorRequestRunning(database, runningID)

	queuedID, _ := CreateCuratorRequest(database, projectID, "queued")

	doneID, _ := CreateCuratorRequest(database, projectID, "done")
	_ = CompleteCuratorRequest(database, doneID, "done", "", 0.1, 100, 1)

	n, err := CancelOrphanedRunningCuratorRequests(database)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if n != 1 {
		t.Errorf("flipped %d rows, want 1", n)
	}

	getStatus := func(id string) string {
		got, _ := GetCuratorRequest(database, id)
		return got.Status
	}
	if got := getStatus(runningID); got != "cancelled" {
		t.Errorf("running row status = %q, want cancelled", got)
	}
	if got := getStatus(queuedID); got != "queued" {
		t.Errorf("queued row status = %q, want queued (untouched)", got)
	}
	if got := getStatus(doneID); got != "done" {
		t.Errorf("done row status = %q, want done (untouched)", got)
	}
}

func TestProjectDelete_CascadesCuratorRows(t *testing.T) {
	// FK ON DELETE CASCADE drives the cleanup contract: removing the
	// project takes its requests + messages with it. Without this,
	// orphaned rows would build up over time.
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)

	requestID, _ := CreateCuratorRequest(database, projectID, "x")
	if _, err := InsertCuratorMessage(database, &domain.CuratorMessage{
		RequestID: requestID,
		Role:      "assistant",
		Subtype:   "text",
		Content:   "hello",
	}); err != nil {
		t.Fatalf("insert msg: %v", err)
	}

	if err := DeleteProject(database, projectID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if got, _ := GetCuratorRequest(database, requestID); got != nil {
		t.Errorf("request survived project delete: %+v", got)
	}
	msgs, err := ListCuratorMessagesByRequest(database, requestID)
	if err != nil {
		t.Fatalf("list msgs: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected no messages after cascade, got %d", len(msgs))
	}
}

func TestSetProjectCuratorSessionID_PersistsOnProjectRow(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)

	if err := SetProjectCuratorSessionID(database, projectID, "sess-curator-123"); err != nil {
		t.Fatalf("set session: %v", err)
	}
	got, _ := GetProject(database, projectID)
	if got.CuratorSessionID != "sess-curator-123" {
		t.Errorf("curator_session_id = %q", got.CuratorSessionID)
	}
}

func TestListCuratorMessagesByRequestIDs_GroupsByRequest(t *testing.T) {
	// Pin the batched-fetch contract: messages are returned in a map
	// keyed by request_id, each list ordered chronologically. This is
	// what lets the history handler render N requests with a single
	// IN-list query instead of N per-request round trips.
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)

	r1, _ := CreateCuratorRequest(database, projectID, "first")
	r2, _ := CreateCuratorRequest(database, projectID, "second")
	r3, _ := CreateCuratorRequest(database, projectID, "third — no replies")

	// Two messages on r1, one on r2, none on r3.
	for _, content := range []string{"r1-a", "r1-b"} {
		if _, err := InsertCuratorMessage(database, &domain.CuratorMessage{
			RequestID: r1, Role: "assistant", Subtype: "text", Content: content,
		}); err != nil {
			t.Fatalf("seed r1 msg %q: %v", content, err)
		}
	}
	if _, err := InsertCuratorMessage(database, &domain.CuratorMessage{
		RequestID: r2, Role: "assistant", Subtype: "text", Content: "r2-a",
	}); err != nil {
		t.Fatalf("seed r2 msg: %v", err)
	}

	got, err := ListCuratorMessagesByRequestIDs(database, []string{r1, r2, r3})
	if err != nil {
		t.Fatalf("batch fetch: %v", err)
	}

	if len(got[r1]) != 2 {
		t.Errorf("r1: got %d messages, want 2", len(got[r1]))
	}
	if got[r1][0].Content != "r1-a" || got[r1][1].Content != "r1-b" {
		t.Errorf("r1 ordering wrong: %+v", got[r1])
	}
	if len(got[r2]) != 1 || got[r2][0].Content != "r2-a" {
		t.Errorf("r2: got %+v, want one r2-a", got[r2])
	}
	// r3 had no messages — must NOT be present in the map. Callers
	// substitute an empty slice when rendering JSON; checking for
	// "missing key === no messages" keeps the helper allocation-free
	// for empty-stream requests.
	if _, ok := got[r3]; ok {
		t.Errorf("r3 should be absent from map (no messages), got %+v", got[r3])
	}
}

func TestListCuratorMessagesByRequestIDs_EmptyInputReturnsEmptyMap(t *testing.T) {
	// The handler iterates the result by request id; a nil map
	// would force every caller to nil-check before lookup. Pin the
	// non-nil empty contract.
	database := newTestDB(t)
	got, err := ListCuratorMessagesByRequestIDs(database, nil)
	if err != nil {
		t.Fatalf("empty fetch: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil empty map for nil input")
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

func TestInsertCuratorMessage_RoundtripsToolCallsAndTokens(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)
	requestID, _ := CreateCuratorRequest(database, projectID, "x")

	in := &domain.CuratorMessage{
		RequestID: requestID,
		Role:      "assistant",
		Subtype:   "tool_use",
		Content:   "calling Read",
		ToolCalls: []domain.ToolCall{{ID: "t1", Name: "Read", Input: map[string]any{"file_path": "/x"}}},
		Model:     "sonnet-4-6",
	}
	five := 5
	twelve := 12
	in.InputTokens = &five
	in.OutputTokens = &twelve

	id, err := InsertCuratorMessage(database, in)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id == 0 {
		t.Errorf("expected non-zero auto id")
	}

	out, err := ListCuratorMessagesByRequest(database, requestID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 row, got %d", len(out))
	}
	got := out[0]
	if got.Role != "assistant" || got.Subtype != "tool_use" {
		t.Errorf("role/subtype: %+v", got)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "Read" {
		t.Errorf("tool_calls roundtrip: %+v", got.ToolCalls)
	}
	if got.InputTokens == nil || *got.InputTokens != 5 {
		t.Errorf("input tokens: %+v", got.InputTokens)
	}
}
