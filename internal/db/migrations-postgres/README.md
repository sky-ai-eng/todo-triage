# Postgres migrations

Empty by design until SKY-247 (D6 — Postgres bootstrap) lands. Once the
Postgres backend has a real consumer, this tree gets a Postgres baseline
that mirrors `migrations-sqlite/202605090001_baseline.sql` translated to
Postgres dialect (BYTEA, GENERATED columns, etc.).

The directory is checked in early — and embedded by `migrations.go`'s
dialect router — so the SQLite-vs-Postgres routing code is reviewable in
isolation. Goose called against an empty directory is a clean no-op.

Adding a migration here is the same shape as `migrations-sqlite/`:
12-digit version (YYYYMMDDNNNN) + `-- +goose Up` / `-- +goose Down`
markers. See the SQLite baseline for the file shape.
