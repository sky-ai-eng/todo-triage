package sqlite_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// anyReview is a minimal fixture for orgID-guard tests where the
// review's actual fields don't matter — the assertion fires before
// the INSERT runs.
func anyReview() domain.PendingReview {
	return domain.PendingReview{
		ID: uuid.New().String(), PRNumber: 1, Owner: "o", Repo: "r", CommitSHA: "sha",
	}
}

// TestReviewStore_SQLite runs the shared conformance suite against
// the SQLite ReviewStore impl. Each subtest gets a fresh in-memory
// DB.
func TestReviewStore_SQLite(t *testing.T) {
	dbtest.RunReviewStoreConformance(t, func(t *testing.T) (db.ReviewStore, string, dbtest.ReviewSeeder) {
		t.Helper()
		conn := newSQLiteForReviewTest(t)
		seed := newSQLiteReviewSeeder(conn)
		stores := sqlitestore.New(conn)
		return stores.Reviews, runmode.LocalDefaultOrgID, seed
	})
}

// TestReviewStore_SQLite_RejectsNonLocalOrg pins the assertLocalOrg
// guard — every method must refuse a non-local orgID.
func TestReviewStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := newSQLiteForReviewTest(t)
	stores := sqlitestore.New(conn)

	const bogusOrg = "11111111-1111-1111-1111-111111111111"
	if err := stores.Reviews.Create(t.Context(), bogusOrg, anyReview()); err == nil {
		t.Errorf("Create with non-local orgID should error")
	}
	if _, err := stores.Reviews.Get(t.Context(), bogusOrg, "any"); err == nil {
		t.Errorf("Get with non-local orgID should error")
	}
}

func newSQLiteForReviewTest(t *testing.T) *sql.DB {
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

// newSQLiteReviewSeeder returns the bag of raw-SQL helpers the
// conformance suite drives. SQLite has no FK enforcement on
// pending_reviews.run_id, so the Run seeder just returns a uuid
// without needing to chain an entity/event/task to back it.
func newSQLiteReviewSeeder(conn *sql.DB) dbtest.ReviewSeeder {
	return dbtest.ReviewSeeder{
		Run: func(t *testing.T) string {
			t.Helper()
			return uuid.New().String()
		},
		SetReviewOriginals: func(t *testing.T, reviewID string, body, event *string) {
			t.Helper()
			var bodyArg, eventArg any
			if body != nil {
				bodyArg = *body
			}
			if event != nil {
				eventArg = *event
			}
			if _, err := conn.Exec(
				`UPDATE pending_reviews SET original_review_body = ?, original_review_event = ? WHERE id = ?`,
				bodyArg, eventArg, reviewID,
			); err != nil {
				t.Fatalf("SetReviewOriginals: %v", err)
			}
		},
		SetCommentOriginalNull: func(t *testing.T, commentID string) {
			t.Helper()
			if _, err := conn.Exec(
				`UPDATE pending_review_comments SET original_body = NULL WHERE id = ?`,
				commentID,
			); err != nil {
				t.Fatalf("SetCommentOriginalNull: %v", err)
			}
		},
	}
}
