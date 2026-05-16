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
