package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=DashboardStore --output=./mocks --case=underscore --with-expecter

// DashboardStore is a read-only projection over entities + their
// snapshot_json blobs. Doesn't own any table — every method
// aggregates data EntityStore would otherwise have to expose.
// Carved out as its own interface so the dashboard handler depends
// on a 2-method surface rather than pulling in the full
// EntityStore + JSON-snapshot reading.
//
// Both methods take the GitHub username because every aggregation
// attributes counts to "the user" — without it the totals are
// meaningless. The handler reads username from the auth context
// before calling.
type DashboardStore interface {
	// Stats returns aggregate PR counts (merged/closed/awaiting/
	// draft) for the user over the last `sinceDays` days, plus
	// reviews-given / reviews-received totals and a 14-day
	// merged-per-day timeline for the sparkline.
	Stats(ctx context.Context, orgID, username string, sinceDays int) (*domain.DashboardStats, error)

	// PRs returns the PR summary rows authored by username, newest
	// last_polled_at first. Drives the dashboard's "your open PRs"
	// list.
	PRs(ctx context.Context, orgID, username string) ([]domain.PRSummaryRow, error)
}
