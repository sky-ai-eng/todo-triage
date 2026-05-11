package db

import (
	"context"
)

//go:generate go run github.com/vektra/mockery/v2 --name=SecretStore --output=./mocks --case=underscore --with-expecter

// SecretStore is the per-org secret bag — GitHub PATs, Jira tokens,
// any other long-lived credential the hosted product needs scoped to
// one tenant. Multi-only by design:
//
//   - Local mode: secrets live in the OS keychain (internal/auth),
//     keyed on the install — there's only one user. The DB has no
//     org-secrets table. All three methods on the SQLite impl
//     return ErrNotApplicableInLocal so a misconfigured caller
//     fails loudly instead of silently writing nowhere.
//
//   - Multi mode: secrets are persisted via the public.vault_*
//     wrapper functions defined in the D3 baseline. Those functions
//     wrap Supabase Vault, enforce a creator_user_id-bound naming
//     convention ("org/<uuid>/<key>"), and refuse calls whose
//     request.jwt.claims.org_id doesn't match the p_org_id argument.
//     The Postgres impl is a thin wrapper over those SQL functions.
//
// D5 owns the consumer side (wiring real handlers + secret-name
// catalog); D2 just provides the interface + working impls.
type SecretStore interface {
	// Put writes (or rotates) a secret. description is optional —
	// the wrapper coalesces NULL → "". Vault stores by name
	// "org/<orgID>/<key>"; rotations overwrite the same row.
	Put(ctx context.Context, orgID, key, value, description string) error

	// Get returns the stored secret value, or ("", nil) when no
	// row matches (a missing secret is not an error — callers
	// distinguish "not configured" from "fetch failed" without
	// having to sniff sentinel errors).
	Get(ctx context.Context, orgID, key string) (string, error)

	// Delete removes a secret. Returns ok=false when no row
	// matched, matching the pattern of other "did the write land"
	// helpers on Stores (RequeueTask, MarkAgentRunCancelledIfActive).
	Delete(ctx context.Context, orgID, key string) (ok bool, err error)
}
