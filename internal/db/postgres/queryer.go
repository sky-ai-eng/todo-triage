package postgres

import (
	"context"
	"database/sql"
)

// queryer is the minimum interface a Postgres store impl needs from
// its underlying handle. Both *sql.DB and *sql.Tx satisfy it via the
// pgx stdlib driver, so the same store body runs inside or outside a
// transaction. database/sql doesn't ship a common interface that both
// types satisfy, so we declare our own.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
