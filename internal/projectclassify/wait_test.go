package projectclassify

import (
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"

	_ "modernc.org/sqlite"
)

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

// TestWaitFor_ReturnsImmediatelyWhenAlreadyClassified guards the
// fast-path: an entity that's already been classified should not
// trigger the runner or wait at all.
func TestWaitFor_ReturnsImmediatelyWhenAlreadyClassified(t *testing.T) {
	database := newTestDB(t)
	entity, _, err := db.FindOrCreateEntity(database, "github", "owner/repo#1", "pr", "T", "https://x/1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AssignEntityProject(database, entity.ID, nil, ""); err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(database)
	start := time.Now()
	WaitFor(database, runner, entity.ID, 5*time.Second)
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Errorf("WaitFor took %s — should have returned immediately for classified entity", elapsed)
	}
}

// TestWaitFor_TriggersRunnerOnEntry verifies that WaitFor pings the
// runner so it wakes up even if no post-poll trigger has fired for
// this entity yet. Relies on the runner being started so the trigger
// channel actually drains.
func TestWaitFor_TriggersRunnerOnEntry(t *testing.T) {
	database := newTestDB(t)
	entity, _, err := db.FindOrCreateEntity(database, "github", "owner/repo#2", "pr", "T", "https://x/2")
	if err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(database)
	// Don't Start the runner — we just want to observe that Trigger()
	// got invoked. Inspect the trigger channel directly: a buffered
	// chan with capacity 1 should have one signal queued after WaitFor.
	go WaitFor(database, runner, entity.ID, 200*time.Millisecond)
	// Give the goroutine a moment to enter WaitFor and call Trigger.
	time.Sleep(50 * time.Millisecond)

	select {
	case <-runner.trigger:
		// expected
	default:
		t.Errorf("runner.Trigger not invoked by WaitFor")
	}
}

// TestWaitFor_HonorsTimeout verifies WaitFor returns within the
// configured budget when classification never completes.
func TestWaitFor_HonorsTimeout(t *testing.T) {
	database := newTestDB(t)
	entity, _, err := db.FindOrCreateEntity(database, "github", "owner/repo#3", "pr", "T", "https://x/3")
	if err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(database)
	// Drain any trigger so the test focuses on the timeout path.
	go func() {
		<-runner.trigger
	}()

	start := time.Now()
	WaitFor(database, runner, entity.ID, 250*time.Millisecond)
	elapsed := time.Since(start)

	if elapsed < 200*time.Millisecond {
		t.Errorf("WaitFor returned in %s — too fast for a 250ms timeout", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("WaitFor took %s — far longer than the 250ms timeout", elapsed)
	}
}

// TestWaitFor_WakesOnceClassificationLands verifies the polling loop
// returns promptly once classified_at is set mid-wait.
func TestWaitFor_WakesOnceClassificationLands(t *testing.T) {
	database := newTestDB(t)
	entity, _, err := db.FindOrCreateEntity(database, "github", "owner/repo#4", "pr", "T", "https://x/4")
	if err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(database)
	go func() {
		<-runner.trigger
	}()

	// Mid-wait, mark the entity classified.
	go func() {
		time.Sleep(200 * time.Millisecond)
		if err := db.AssignEntityProject(database, entity.ID, nil, ""); err != nil {
			t.Errorf("AssignEntityProject in goroutine: %v", err)
		}
	}()

	start := time.Now()
	WaitFor(database, runner, entity.ID, 5*time.Second)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("WaitFor returned in %s — should have woken within the polling cadence after classification", elapsed)
	}
}

// silence unused-import warning when domain is not directly used in
// trivial paths above.
var _ = domain.Entity{}
var _ atomic.Int32
