package postgres

import (
	"context"
	"database/sql"
	"fmt"
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

// inTx runs fn against a queryer that's guaranteed to be a transaction:
//   - if q is already a *sql.Tx (caller composed us inside Stores.Tx.WithTx),
//     fn runs against that tx directly so the outer commit/rollback wins
//     and any RLS claims set by WithTx remain in effect.
//   - if q is a *sql.DB, inTx opens a fresh tx, runs fn against it, and
//     commits or rolls back based on fn's return. This is the path that
//     gives store methods documented as "atomic across the batch" their
//     atomicity even when called outside an explicit WithTx.
//
// The Postgres tx opened here does NOT set request.jwt.claims —
// callers operating outside WithTx are on the admin pool (RLS
// bypassed) or another non-user-scoped path. Anything that needs
// per-user claims must go through Stores.Tx.WithTx.
func inTx(ctx context.Context, q queryer, fn func(queryer) error) error {
	switch v := q.(type) {
	case *sql.Tx:
		return fn(v)
	case *sql.DB:
		tx, err := v.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	default:
		return fmt.Errorf("postgres store: unexpected queryer type %T", q)
	}
}
