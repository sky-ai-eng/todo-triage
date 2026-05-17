package db

import "context"

// OrgsStore owns the orgs table — the tenancy root every other
// resource hangs off via FK. Background services (poller, tracker,
// projectclassify, repoprofile) iterate active orgs at the top of
// each cycle through this store instead of hardcoding the local-mode
// sentinel.
//
// # Pool split (Postgres)
//
// Every method on this store routes through the admin pool. The
// callers are background goroutines launched at boot — they have no
// JWT-claims context, and iterating active orgs is by definition a
// cross-org system-service read. The orgs_select RLS policy in the
// multi-mode schema only exposes rows the calling user is a member
// of, so the app pool wouldn't return the right set for these
// callers.
type OrgsStore interface {
	// ListActiveSystem returns the IDs of every active org in
	// ascending id order. "Active" means deleted_at IS NULL in
	// Postgres; SQLite has no soft-delete column, so the local-mode
	// impl returns every row (which collapses to the single
	// runmode.LocalDefaultOrgID sentinel seeded at install).
	//
	// Ordering is stable so per-org iteration is reproducible across
	// poll cycles — useful for log/test assertions and means a
	// partial failure in cycle N is followed by the same org order
	// in cycle N+1 unless rows changed.
	ListActiveSystem(ctx context.Context) ([]string, error)
}
