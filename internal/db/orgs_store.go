package db

import "context"

// OrgsStore owns the orgs table — the tenancy root every other resource
// hangs off via FK. Pre-SKY-312 there was no Go-side store for orgs:
// reads of the orgs row at boot were done through ad-hoc SQL or implied
// via the runmode.LocalDefaultOrgID sentinel. SKY-312 introduces the
// store so background services (poller, tracker, projectclassify,
// repoprofile) can iterate active orgs at the top of each cycle
// instead of hardcoding the sentinel.
//
// # Pool split (Postgres)
//
// Every method on this store routes through the admin pool. The four
// callers are background goroutines launched at boot — they have no
// JWT-claims context, and iterating active orgs is by definition a
// cross-org system-service read. orgs_select RLS in the multi-mode
// schema only exposes rows the calling user is a member of, so the
// app pool wouldn't return the right set for these callers.
type OrgsStore interface {
	// ListActiveSystem returns the IDs of every active org in
	// ascending id order. "Active" means deleted_at IS NULL in
	// Postgres; SQLite has no soft-delete column, so the local-mode
	// impl returns every row (which collapses to the single
	// runmode.LocalDefaultOrgID sentinel).
	//
	// Ordering is stable so per-org iteration is reproducible across
	// poll cycles — useful for log/test assertions and means a partial
	// failure in cycle N is followed by the same org order in cycle
	// N+1 unless rows changed.
	ListActiveSystem(ctx context.Context) ([]string, error)
}
