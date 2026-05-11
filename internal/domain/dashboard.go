package domain

// DashboardStats holds aggregated PR statistics derived from entity
// snapshots. Returned by db.DashboardStore.Stats and rendered as JSON
// by /api/dashboard/stats.
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

// PRSummaryRow is a PR as displayed on the dashboard list. Returned
// by db.DashboardStore.PRs and rendered as JSON by /api/dashboard/prs.
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
