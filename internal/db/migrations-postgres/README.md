# Postgres migrations

Multi-tenant Postgres schema, applied via goose when `internal/db.Migrate`
is called with `dialect = "postgres"`. The baseline (`202605100001_pg_baseline.sql`)
lands the full multi-mode schema: tenancy tables, settings tables, RLS
helpers + policies, Vault wrappers, and every TF table recreated with
`org_id NOT NULL` baked in. See `docs/specs/sky-247-d3-multi-tenant-postgres-schema.html`
for the design.

## Conventions

- **File naming:** `NNNNNNNNNNNN_description.sql` — 12-digit
  `YYYYMMDDNNNN` version, lowercase snake_case description. Matches
  `migrations-sqlite/`.
- **Goose markers:** `-- +goose Up` at the top, `-- +goose Down`
  trailing block. Down is `SELECT 'down not supported';` — we don't ship
  downgrades; users wanting to roll back rebuild from a known-good
  snapshot.
- **Multi-statement plpgsql:** wrap function bodies and `DO $$ ... $$`
  blocks in `-- +goose StatementBegin` / `-- +goose StatementEnd`. Goose's
  default statement splitter splits on semicolons; without the markers
  it tokenizes semicolons inside function bodies as statement
  terminators and the file fails to parse.
- **Forward-only:** never edit the baseline. New schema changes land as
  new `NNNNNNNNNNNN_*.sql` files.

## Why a separate tree from `migrations-sqlite/`

The two dialects diverge enough that a single SQL file with runtime
`if`/`else` would be unreadable. Keeping them side-by-side means each
parser only ever sees DDL it can interpret — `BYTEA` vs `BLOB`,
`TIMESTAMPTZ` vs `DATETIME`, `JSONB` vs `TEXT`, RLS policies vs
nothing. The runner (`migrations.go`) picks one based on the dialect
the caller passed to `Migrate(db, dialect)`.

## Testing

`internal/db/pgtest/` spins up a `supabase/postgres:15.1.0.147`
testcontainer, runs the baseline, exposes both an `AdminDB`
(supabase_admin superuser; RLS bypassed) and an `AppDB`
(authenticator → `SET LOCAL ROLE tf_app`; RLS active). Tests that
assert RLS use the `WithUser(t, userID, orgID, fn)` helper. See
`pgtest/baseline_test.go` for the patterns.

Tests skip cleanly when Docker is unavailable.
