package tracker

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func TestShouldSkipRefresh_QuietPRSkips(t *testing.T) {
	stored := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
		CheckRuns: []domain.CheckRun{
			{Name: "ci", Status: "completed", Conclusion: "success"},
		},
	}
	fresh := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
	}
	if !shouldSkipRefresh(stored, fresh) {
		t.Fatalf("expected skip when updatedAt + SHA unchanged and CI is terminal")
	}
}

func TestShouldSkipRefresh_UpdatedAtAdvancedRefreshes(t *testing.T) {
	stored := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
		CheckRuns: []domain.CheckRun{{Name: "ci", Status: "completed", Conclusion: "success"}},
	}
	fresh := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:05:00Z",
		HeadSHA:   "abc123",
	}
	if shouldSkipRefresh(stored, fresh) {
		t.Fatalf("expected refresh when updatedAt advanced")
	}
}

func TestShouldSkipRefresh_HeadSHAChangedRefreshes(t *testing.T) {
	stored := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
		CheckRuns: []domain.CheckRun{{Name: "ci", Status: "completed", Conclusion: "success"}},
	}
	fresh := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "def456",
	}
	if shouldSkipRefresh(stored, fresh) {
		t.Fatalf("expected refresh when head SHA changed even with same updatedAt")
	}
}

func TestShouldSkipRefresh_InFlightCIRefreshes(t *testing.T) {
	stored := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
		CheckRuns: []domain.CheckRun{
			{Name: "lint", Status: "completed", Conclusion: "success"},
			{Name: "test", Status: "in_progress"},
		},
	}
	fresh := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
	}
	if shouldSkipRefresh(stored, fresh) {
		t.Fatalf("expected refresh when stored snapshot has in-flight CI (updatedAt doesn't bump on check completion)")
	}
}

func TestShouldSkipRefresh_QueuedCIRefreshes(t *testing.T) {
	stored := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
		CheckRuns: []domain.CheckRun{{Name: "test", Status: "queued"}},
	}
	fresh := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
	}
	if shouldSkipRefresh(stored, fresh) {
		t.Fatalf("expected refresh when stored snapshot has queued check runs")
	}
}

func TestShouldSkipRefresh_NilCheckRunsRefreshes(t *testing.T) {
	// Freshly discovered entity: snapshot was seeded from the lightweight
	// discovery fragment which doesn't return check_runs. Stored.CheckRuns
	// is nil. Skipping here would leave the entity with no CI data forever.
	stored := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
		CheckRuns: nil,
	}
	fresh := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
	}
	if shouldSkipRefresh(stored, fresh) {
		t.Fatalf("expected refresh on first cycle after discovery (CheckRuns nil)")
	}
}

func TestShouldSkipRefresh_EmptyCheckRunsCanSkip(t *testing.T) {
	// After a full refresh on a PR with zero check suites, CheckRuns is
	// non-nil empty. That's a different state from nil — it means
	// "polled, nothing here" — and is safe to skip on subsequent cycles.
	stored := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
		CheckRuns: []domain.CheckRun{},
	}
	fresh := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
	}
	if !shouldSkipRefresh(stored, fresh) {
		t.Fatalf("expected skip with empty (non-nil) check_runs")
	}
}

func TestShouldSkipRefresh_MissingUpdatedAtRefreshes(t *testing.T) {
	// Pre-field snapshot or parse failure — without updatedAt we have no
	// gate signal, so always refresh.
	stored := domain.PRSnapshot{HeadSHA: "abc123", CheckRuns: []domain.CheckRun{}}
	fresh := domain.PRSnapshot{UpdatedAt: "2026-05-03T12:00:00Z", HeadSHA: "abc123"}
	if shouldSkipRefresh(stored, fresh) {
		t.Fatalf("expected refresh when stored.UpdatedAt is empty")
	}
}

func TestShouldSkipRefresh_MissingHeadSHARefreshes(t *testing.T) {
	stored := domain.PRSnapshot{UpdatedAt: "2026-05-03T12:00:00Z", CheckRuns: []domain.CheckRun{}}
	fresh := domain.PRSnapshot{UpdatedAt: "2026-05-03T12:00:00Z", HeadSHA: "abc123"}
	if shouldSkipRefresh(stored, fresh) {
		t.Fatalf("expected refresh when stored.HeadSHA is empty")
	}
}
