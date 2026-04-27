package db

import (
	"testing"
	"time"
)

// TestParseDBDatetime locks in the format coverage parseDBDatetime needs to
// handle without raising errors. The factory snapshot endpoint surfaces any
// parse failure as an HTTP 500, so a row in the events table with an
// unrecognized format silently breaks the entire view. Coverage matters.
//
// Two format families show up in the wild:
//
//   - SQLite-canonical (modernc with _time_format=sqlite, current default):
//     "2006-01-02 15:04:05.999999999-07:00", with the fractional segment
//     dropped when nanos==0 ("2026-04-27 19:02:11+00:00").
//   - Legacy Go time.String() (modernc default before _time_format=sqlite):
//     "2006-01-02 15:04:05.999999999 -0700 MST", optionally with a
//     " m=+..." monotonic clock suffix, optionally with the fractional
//     segment dropped when nanos==0.
//
// Go's time.Parse treats `.999...` as an optional fractional component,
// so a single layout matches both fractional and non-fractional inputs.
// This test pins that behavior so a future layout edit or stdlib change
// can't silently regress the no-fractional path — which would manifest
// as the factory page going blank with a 500 the moment any zero-nano
// timestamp hits the events table.
func TestParseDBDatetime(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Time // zero means "expected to error"
	}{
		// --- modernc _time_format=sqlite output ---
		{
			name: "sqlite_canonical_with_fractional",
			in:   "2026-04-27 19:02:11.123456789-07:00",
			want: time.Date(2026, 4, 27, 19, 2, 11, 123456789, time.FixedZone("", -7*3600)),
		},
		{
			name: "sqlite_canonical_no_fractional",
			in:   "2026-04-27 19:02:11-07:00",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.FixedZone("", -7*3600)),
		},
		// --- modernc legacy Go time.String() output ---
		{
			name: "go_string_with_fractional_pdt",
			in:   "2026-04-27 19:02:11.123456789 -0700 PDT",
			want: time.Date(2026, 4, 27, 19, 2, 11, 123456789, time.FixedZone("PDT", -7*3600)),
		},
		{
			name: "go_string_no_fractional_pdt",
			in:   "2026-04-27 19:02:11 -0700 PDT",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.FixedZone("PDT", -7*3600)),
		},
		{
			name: "go_string_no_fractional_utc",
			in:   "2026-04-27 19:02:11 +0000 UTC",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.UTC),
		},
		{
			name: "go_string_with_monotonic_suffix",
			in:   "2026-04-27 19:02:11.123 -0700 PDT m=+1.500",
			want: time.Date(2026, 4, 27, 19, 2, 11, 123000000, time.FixedZone("PDT", -7*3600)),
		},
		{
			name: "go_string_with_negative_monotonic",
			in:   "2026-04-27 19:02:11 -0700 PDT m=-0.250",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.FixedZone("PDT", -7*3600)),
		},
		// --- SQLite default CURRENT_TIMESTAMP ---
		{
			name: "sqlite_current_timestamp",
			in:   "2026-04-27 19:02:11",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.UTC),
		},
		// --- RFC3339 (the GitHub side, also our own RFC3339Nano writes) ---
		{
			name: "rfc3339_zulu",
			in:   "2026-04-27T19:02:11Z",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.UTC),
		},
		{
			name: "rfc3339_with_offset",
			in:   "2026-04-27T19:02:11-07:00",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.FixedZone("", -7*3600)),
		},
		// --- empty input is a non-error sentinel ---
		{
			name: "empty",
			in:   "",
			want: time.Time{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDBDatetime(tc.in)
			if err != nil {
				t.Fatalf("parseDBDatetime(%q): unexpected error: %v", tc.in, err)
			}
			// Use Equal so location/abbreviation differences don't fail the
			// test as long as the instant is the same. The legacy Go-String
			// form encodes "PDT" which Go can't fully round-trip, but the
			// underlying instant is unambiguous.
			if !got.Equal(tc.want) {
				t.Errorf("parseDBDatetime(%q) = %v, want %v (equal-instant)", tc.in, got, tc.want)
			}
		})
	}
}
