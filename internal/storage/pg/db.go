// Package pg is the generic Postgres-backed rest.Storage used by every
// feedback resource except FeedbackSource.
//
// Each kind has its own table (see migrations/00002_objects.sql) with the
// same leading columns: scope_type, scope_name, name, uid, resource_version,
// created_at, labels and object. A Kind adds extra columns copied from the
// object for field selectors and sorting.
//
// Writes run in a transaction carried on the context. A storage method that
// is called while another write's transaction is in ctx joins it (as a
// savepoint), so side effects such as the Subscription created with a Vote
// commit or roll back with the request.
package pg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is satisfied by both the pool and a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DB wraps the pool and the transaction-on-context helpers.
type DB struct {
	Pool *pgxpool.Pool
}

type txKey struct{}

// errRollback makes InTx roll back without reporting an error, for dry runs.
var errRollback = errors.New("rollback requested")

// InTx runs fn in a transaction. If ctx already carries one, fn runs in a
// savepoint inside it, so it commits only when the outer transaction does.
// fn must use Q(ctx) for every statement.
func (d *DB) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	var beginner interface {
		Begin(context.Context) (pgx.Tx, error)
	} = d.Pool
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		beginner = tx
	}
	err := pgx.BeginFunc(ctx, beginner, func(tx pgx.Tx) error {
		return fn(context.WithValue(ctx, txKey{}, tx))
	})
	if errors.Is(err, errRollback) {
		return nil
	}
	return err
}

// Q returns the transaction in ctx, or the pool when there is none.
func (d *DB) Q(ctx context.Context) Querier {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return tx
	}
	return d.Pool
}
