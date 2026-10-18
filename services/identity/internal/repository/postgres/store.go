// Package postgres is identity's repository on PostgreSQL (pgx v5). Table
// names are unqualified: the schema comes from the connection's search_path.
package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tbilol/dxrl/services/identity/internal/service"
)

// DB is what the repository needs from a pool, a connection or a transaction
// (satisfied by *pgxpool.Pool, pgx.Tx and pgxmock).
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store implements service.Store and events.OutboxStore.
type Store struct {
	db  DB
	now func() time.Time
}

// New returns a Store on db.
func New(db DB) *Store { return &Store{db: db, now: time.Now} }

// Run implements service.Store: fn's statements run on the pool, each in its
// own implicit transaction.
func (s *Store) Run(_ context.Context, fn func(service.Queries) error) error {
	return fn(&queries{db: s.db})
}

// InTx implements service.Store: fn runs in one transaction, committed when
// fn returns nil.
func (s *Store) InTx(ctx context.Context, fn func(service.Queries) error) error {
	return inTx(ctx, s.db, func(tx pgx.Tx) error { return fn(&queries{db: tx}) })
}

// inTx commits when fn returns nil and rolls back once otherwise, including
// when fn panics. The rollback ignores ctx cancellation so the connection
// goes back to the pool clean.
func inTx(ctx context.Context, db DB, fn func(pgx.Tx) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	done = true // pgx closes the transaction even when Commit fails
	return tx.Commit(ctx)
}

// queries implements service.Queries on a pool or a transaction.
type queries struct {
	db DB
}

var _ service.Queries = (*queries)(nil)

// utc normalizes a scanned timestamp (pgx returns local time).
func utc(t time.Time) time.Time { return t.UTC() }

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
