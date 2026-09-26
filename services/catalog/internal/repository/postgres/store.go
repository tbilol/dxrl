// Package postgres is catalog's repository on PostgreSQL (pgx v5). Table
// names are unqualified: the schema comes from the connection's search_path.
package postgres

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tbilol/dxrl/services/catalog/internal/service"
)

// DB is what the repository needs from a pool, a connection or a transaction
// (satisfied by *pgxpool.Pool, pgx.Tx and pgxmock).
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store implements service.Store, events.OutboxStore, events.InboxStore and
// views.Store.
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

// IsTransient reports whether err is a failure of the environment rather than
// of the data being written, worth retrying for as long as it lasts: the
// database is unreachable, restarting, overloaded or too slow, rejects the
// credentials, does not exist, or has no (or an older) schema (`migrate up`
// not run), or the transaction lost a serialization race. The stream
// consumer retries such failures without spending its retry budget, so an
// outage or a deployment mistake never dead-letters a stream. Errors about
// the record's data (classes 22 and 23) are not transient.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) { // no connection at all, whatever the server said
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code[:2] {
		case "08", // connection exception
			"28", // invalid authorization (password, role)
			"3D", // invalid catalog name: the database does not exist
			"40", // transaction rollback: serialization failure, deadlock
			"42", // undefined table/column, insufficient privilege: schema not migrated
			"53", // insufficient resources (too many connections, disk full)
			"57", // operator intervention: admin shutdown, cannot connect now, query canceled
			"58": // system error (I/O)
			return true
		}
		return pgErr.Code == "55P03" // lock_not_available
	}
	if pgconn.Timeout(err) || pgconn.SafeToRetry(err) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// nullString maps "" to NULL.
func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// utc normalizes a scanned timestamp (pgx returns local time).
func utc(t time.Time) time.Time { return t.UTC() }

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
