package domain

import "time"

// externalTimeLayouts enumerates the timestamp formats we accept on
// values coming from external systems (Jira's `updated`, GitHub
// timeline items, our own entity.CreatedAt round-trips). Jira is the
// awkward one — it omits the colon in the UTC offset (`+0000` instead
// of `+00:00`), which RFC3339 rejects, and inconsistently includes
// milliseconds across endpoints. The non-RFC3339 forms come first so
// the common Jira shape succeeds on the first attempt.
//
// Ordering rationale (don't reorder without thought):
//
//   - Jira-with-ms ("2006-01-02T15:04:05.000-0700") covers most issue
//     timestamps from Jira Cloud's REST API.
//   - Jira-no-ms catches older endpoints and Jira Server.
//   - RFC3339Nano covers GitHub (timelineItems createdAt is RFC3339)
//     and any modern external source that uses fractional seconds.
//   - RFC3339 is the no-fractional-seconds catch-all.
var externalTimeLayouts = []string{
	"2006-01-02T15:04:05.000-0700",
	"2006-01-02T15:04:05-0700",
	time.RFC3339Nano,
	time.RFC3339,
}

// ParseExternalTime tries each known external timestamp layout in
// order, returning the first successful parse normalized to UTC. The
// boolean result is false when no layout matches — callers should
// treat that as "unknown source time" and degrade gracefully (typically
// by falling back to detection time).
//
// Centralized here because both the diff layer (source-time
// enrichment) and the stock-counting endpoint parse the same external
// timestamp shapes; a divergent layout list across call sites would
// silently drop OccurredAt on edge-case formats.
func ParseExternalTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range externalTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
