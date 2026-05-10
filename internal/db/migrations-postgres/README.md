# Postgres migrations

Empty by design until SKY-247 (D3 — multi-tenant Postgres schema) lands.
Once the Postgres backend has a real consumer, this tree gets the
multi-tenant baseline (orgs, teams, memberships, sessions, integrations,
Vault wrappers, RLS helpers + policies, and every TF table recreated
with `org_id NOT NULL` baked in).

The directory is checked in early — and embedded by `migrations.go`'s
dialect router — so the SQLite-vs-Postgres routing code is reviewable in
isolation. Goose called against an empty directory is a clean no-op.

Adding a migration here is the same shape as `migrations-sqlite/`:
12-digit version (YYYYMMDDNNNN) + `-- +goose Up` / `-- +goose Down`
markers. See the SQLite baseline for the file shape.
