package db

import (
	"context"
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
	Stats(ctx context.Context, orgID, username string, sinceDays int) (*DashboardStats, error)

	// PRs returns the PR summary rows authored by username, newest
	// last_polled_at first. Drives the dashboard's "your open PRs"
	// list.
	PRs(ctx context.Context, orgID, username string) ([]PRSummaryRow, error)
}

// DashboardStats holds aggregated PR statistics derived from entity snapshots.
type DashboardStats struct {
	Merged          int              `json:"merged"`
	Closed          int              `json:"closed"`
	Awaiting        int              `json:"awaiting"`
	Draft           int              `json:"draft"`
	ReviewsGiven    int              `json:"reviews_given"`
	ReviewsReceived int              `json:"reviews_received"`
	MergedOverTime  []DashboardPoint `json:"merged_over_time"`
}

// DashboardPoint is one bucket of the merged-over-time sparkline.
type DashboardPoint struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// PRSummaryRow is a PR as displayed on the dashboard list.
type PRSummaryRow struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	Repo      string   `json:"repo"`
	Author    string   `json:"author"`
	State     string   `json:"state"`
	Draft     bool     `json:"draft"`
	Labels    []string `json:"labels"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
	HTMLURL   string   `json:"html_url"`
}
