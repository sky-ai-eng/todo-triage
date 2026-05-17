package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestCuratorStore_SQLite_FullTurn pins the per-turn write set the
// curator goroutine produces against SQLite. Mirrors the Postgres
// attribution test but without RLS — SQLite has no auth concept and
// the assertion is purely behavioral. SKY-298.
func TestCuratorStore_SQLite_FullTurn(t *testing.T) {
	conn := newSQLiteForCuratorTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	projectID, err := stores.Projects.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		domain.Project{Name: "p"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	// Drive each lifecycle write through SyntheticClaimsWithTx so the
	// test exercises the production goroutine code path.
	var requestID string
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		id, err := ts.Curator.CreateRequest(ctx, runmode.LocalDefaultOrgID, projectID, runmode.LocalDefaultUserID, "hello")
		if err != nil {
			return err
		}
		requestID = id
		return nil
	}); err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}

	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		return ts.Curator.MarkRequestRunning(ctx, runmode.LocalDefaultOrgID, requestID)
	}); err != nil {
		t.Fatalf("MarkRequestRunning: %v", err)
	}

	// Second MarkRunning should return sql.ErrNoRows because the
	// status filter (status = 'queued') no longer matches.
	err = stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		return ts.Curator.MarkRequestRunning(ctx, runmode.LocalDefaultOrgID, requestID)
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("second MarkRequestRunning err = %v, want sql.ErrNoRows", err)
	}

	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		_, err := ts.Curator.InsertMessage(ctx, runmode.LocalDefaultOrgID, &domain.CuratorMessage{
			RequestID: requestID,
			Role:      "assistant",
			Subtype:   "text",
			Content:   "ack",
		})
		return err
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	// CompleteRequest flips terminal once; second call returns
	// flipped=false because the row is already terminal.
	var flipped bool
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		f, err := ts.Curator.CompleteRequest(ctx, runmode.LocalDefaultOrgID, requestID, "done", "", 0.01, 100, 1)
		if err != nil {
			return err
		}
		flipped = f
		return nil
	}); err != nil {
		t.Fatalf("first CompleteRequest: %v", err)
	}
	if !flipped {
		t.Error("first CompleteRequest flipped=false, want true")
	}
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		f, err := ts.Curator.CompleteRequest(ctx, runmode.LocalDefaultOrgID, requestID, "done", "", 0.02, 200, 2)
		if err != nil {
			return err
		}
		flipped = f
		return nil
	}); err != nil {
		t.Fatalf("second CompleteRequest: %v", err)
	}
	if flipped {
		t.Error("second CompleteRequest flipped=true, want false (already terminal)")
	}

	// GetRequest under the same claims sees the row.
	var seen *domain.CuratorRequest
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		r, err := ts.Curator.GetRequest(ctx, runmode.LocalDefaultOrgID, requestID)
		if err != nil {
			return err
		}
		seen = r
		return nil
	}); err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	if seen == nil {
		t.Fatal("GetRequest returned nil for existing row")
	}
	if seen.Status != "done" {
		t.Errorf("status = %q, want done", seen.Status)
	}
	if seen.CreatorUserID != runmode.LocalDefaultUserID {
		t.Errorf("CreatorUserID = %q, want %q", seen.CreatorUserID, runmode.LocalDefaultUserID)
	}
}

// TestCuratorStore_SQLite_PendingContextRoundTrip pins the consume →
// finalize and consume → revert flows the goroutine uses for pending
// context-change rows. The consume path is the most complex SQL in
// the store (UPDATE-first locking) and needs separate coverage from
// the higher-level fixtures.
func TestCuratorStore_SQLite_PendingContextRoundTrip(t *testing.T) {
	conn := newSQLiteForCuratorTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	projectID, err := stores.Projects.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		domain.Project{Name: "p", CuratorSessionID: "sess-1"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	// Seed a pending context row directly via the package-level helper
	// (the projects handler calls this on PATCH; the goroutine never
	// inserts pending rows itself, only consumes them).
	if err := db.InsertPendingContext(conn, projectID, "sess-1", domain.ChangeTypePinnedRepos, `["foo/bar"]`); err != nil {
		t.Fatalf("seed pending: %v", err)
	}

	requestID, err := db.CreateCuratorRequest(conn, projectID, "consume me")
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	var (
		project *domain.Project
		pending []domain.CuratorPendingContext
	)
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		p, ps, err := ts.Curator.ConsumePendingContext(ctx, runmode.LocalDefaultOrgID, projectID, requestID)
		if err != nil {
			return err
		}
		project = p
		pending = ps
		return nil
	}); err != nil {
		t.Fatalf("ConsumePendingContext: %v", err)
	}
	if project == nil || project.ID != projectID {
		t.Fatalf("Consume returned project %+v, want id=%s", project, projectID)
	}
	if len(pending) != 1 {
		t.Fatalf("Consume returned %d pending rows, want 1", len(pending))
	}
	if pending[0].ChangeType != domain.ChangeTypePinnedRepos {
		t.Errorf("pending row change_type = %q, want %q", pending[0].ChangeType, domain.ChangeTypePinnedRepos)
	}

	// Revert un-consumes the rows.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		return ts.Curator.RevertPendingContext(ctx, runmode.LocalDefaultOrgID, requestID)
	}); err != nil {
		t.Fatalf("RevertPendingContext: %v", err)
	}
	all, err := db.ListPendingContext(conn, projectID)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(all) != 1 || all[0].ConsumedAt != nil {
		t.Errorf("after revert, expected 1 unconsumed row; got %+v", all)
	}

	// Re-consume + finalize purges them.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		if _, _, err := ts.Curator.ConsumePendingContext(ctx, runmode.LocalDefaultOrgID, projectID, requestID); err != nil {
			return err
		}
		return ts.Curator.FinalizePendingContext(ctx, runmode.LocalDefaultOrgID, requestID)
	}); err != nil {
		t.Fatalf("consume+finalize: %v", err)
	}
	all, err = db.ListPendingContext(conn, projectID)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("after finalize, expected 0 rows; got %d", len(all))
	}
}

// TestCuratorStore_SQLite_RevertCleansAuditRow pins the compound
// revert-and-delete-audit-row path that the goroutine's
// revertPendingFor helper drives on terminal cancel/fail. The audit
// row is the `context_change` curator_messages entry the dispatch
// loop persists when it renders a pending-context note into the
// user's message — if the turn doesn't complete successfully, the
// chat history must not show a phantom "context noted" entry for a
// delta the agent never absorbed.
func TestCuratorStore_SQLite_RevertCleansAuditRow(t *testing.T) {
	conn := newSQLiteForCuratorTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	projectID, err := stores.Projects.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		domain.Project{Name: "p", CuratorSessionID: "sess-1"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := db.InsertPendingContext(conn, projectID, "sess-1", domain.ChangeTypePinnedRepos, `["foo/bar"]`); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	requestID, err := db.CreateCuratorRequest(conn, projectID, "msg")
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	// Drive consume + audit-row insert under the same identity the
	// goroutine would use — this mirrors the dispatch sequence in
	// session.go around the context-change rendering.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		if _, _, err := ts.Curator.ConsumePendingContext(ctx, runmode.LocalDefaultOrgID, projectID, requestID); err != nil {
			return err
		}
		_, err := ts.Curator.InsertMessage(ctx, runmode.LocalDefaultOrgID, &domain.CuratorMessage{
			RequestID: requestID,
			Role:      "system",
			Subtype:   "context_change",
			Content:   "pinned_repos changed",
		})
		return err
	}); err != nil {
		t.Fatalf("consume + audit insert: %v", err)
	}

	// Audit row should be present before revert.
	auditCount := countMessages(t, conn, requestID, "context_change")
	if auditCount != 1 {
		t.Fatalf("pre-revert audit row count = %d, want 1", auditCount)
	}

	// Revert + DeleteMessagesBySubtype — the exact pair revertPendingFor runs.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		if err := ts.Curator.RevertPendingContext(ctx, runmode.LocalDefaultOrgID, requestID); err != nil {
			return err
		}
		return ts.Curator.DeleteMessagesBySubtype(ctx, runmode.LocalDefaultOrgID, requestID, "context_change")
	}); err != nil {
		t.Fatalf("revert + audit delete: %v", err)
	}

	// Audit row gone.
	if got := countMessages(t, conn, requestID, "context_change"); got != 0 {
		t.Errorf("post-revert audit row count = %d, want 0", got)
	}
	// Pending row re-armed (un-consumed).
	pending, err := db.ListPendingContext(conn, projectID)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 || pending[0].ConsumedAt != nil {
		t.Errorf("expected 1 unconsumed pending row after revert; got %+v", pending)
	}

	// Other-subtype messages on the same request must NOT be touched.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		_, err := ts.Curator.InsertMessage(ctx, runmode.LocalDefaultOrgID, &domain.CuratorMessage{
			RequestID: requestID,
			Role:      "assistant",
			Subtype:   "text",
			Content:   "should survive",
		})
		return err
	}); err != nil {
		t.Fatalf("insert assistant message: %v", err)
	}
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		return ts.Curator.DeleteMessagesBySubtype(ctx, runmode.LocalDefaultOrgID, requestID, "context_change")
	}); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if got := countMessages(t, conn, requestID, "text"); got != 1 {
		t.Errorf("text subtype message count = %d, want 1 (DeleteMessagesBySubtype clobbered an unrelated subtype)", got)
	}
}

func countMessages(t *testing.T, conn *sql.DB, requestID, subtype string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM curator_messages WHERE request_id = ? AND subtype = ?`,
		requestID, subtype,
	).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return n
}

// TestProjectStore_SQLite_SetCuratorSessionID verifies the new
// ProjectStore method used by the curator sink on first-session
// capture. Idempotent set-then-read.
func TestProjectStore_SQLite_SetCuratorSessionID(t *testing.T) {
	conn := newSQLiteForCuratorTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	projectID, err := stores.Projects.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		domain.Project{Name: "p"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	if err := stores.Projects.SetCuratorSessionID(ctx, runmode.LocalDefaultOrgID, projectID, "sess-xyz"); err != nil {
		t.Fatalf("SetCuratorSessionID: %v", err)
	}
	got, err := stores.Projects.Get(ctx, runmode.LocalDefaultOrgID, projectID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CuratorSessionID != "sess-xyz" {
		t.Errorf("CuratorSessionID = %q, want sess-xyz", got.CuratorSessionID)
	}

	// Idempotent re-set.
	if err := stores.Projects.SetCuratorSessionID(ctx, runmode.LocalDefaultOrgID, projectID, "sess-xyz"); err != nil {
		t.Errorf("re-SetCuratorSessionID should be idempotent, got %v", err)
	}

	// Missing project: silently no-op (nil error), not sql.ErrNoRows.
	// Pinned by the interface doc — diverges intentionally from
	// Update/Delete's not-found semantics because the curator sink
	// has nothing useful to do with an error when the project was
	// deleted mid-turn.
	if err := stores.Projects.SetCuratorSessionID(ctx, runmode.LocalDefaultOrgID, "00000000-0000-0000-0000-000000000ghost", "sess-x"); err != nil {
		t.Errorf("SetCuratorSessionID on missing project should be best-effort nil, got %v", err)
	}
}

func newSQLiteForCuratorTest(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return conn
}
