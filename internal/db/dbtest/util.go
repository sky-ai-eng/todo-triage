package dbtest

import "math"

// priorityTolerance is the float64 comparison fuzz used by the
// agent + team_agent conformance suites when checking persisted
// numeric values across the SQLite / Postgres serialization round-trip.
// Both backends store float64s but round-trip them through different
// drivers (modernc.org/sqlite vs jackc/pgx), and exact equality fails
// on the last bit or two for some values.
const priorityTolerance = 1e-5

func nearlyEqual(a, b float64) bool {
	return math.Abs(a-b) <= priorityTolerance
}
