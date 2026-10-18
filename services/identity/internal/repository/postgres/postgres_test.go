package postgres

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v5"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/identity/internal/domain"
	"github.com/tbilol/dxrl/services/identity/internal/events"
	"github.com/tbilol/dxrl/services/identity/internal/service"
)

var (
	ctx         = context.Background()
	accountCols = []string{"id", "status", "version", "created_at", "updated_at", "roles"}
	tashkent    = time.FixedZone("UZT", 5*3600)
)

func newMock(t *testing.T) (pgxmock.PgxPoolIface, *Store) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		mock.Close()
	})
	return mock, New(mock)
}

// run executes fn outside a transaction and fails the test on error.
func run(t *testing.T, s *Store, fn func(q service.Queries) error) {
	t.Helper()
	if err := s.Run(ctx, fn); err != nil {
		t.Fatal(err)
	}
}

func TestGetAccountMapsRow(t *testing.T) {
	mock, store := newMock(t)
	id := uuid.New()
	created := time.Date(2026, 9, 22, 15, 0, 0, 0, tashkent)
	mock.ExpectQuery(`FROM accounts a WHERE a.id = \$1`).WithArgs(id).WillReturnRows(
		mock.NewRows(accountCols).AddRow(id, "ACTIVE", int64(3), created, created, []string{"ADMIN"}))

	var a domain.Account
	run(t, store, func(q service.Queries) (err error) { a, err = q.GetAccount(ctx, id); return })
	if a.ID != id || a.Status != domain.AccountActive || a.Version != 3 ||
		!slices.Equal(a.Roles, []commonv1.Role{domain.RoleAdmin}) {
		t.Fatalf("account = %+v", a)
	}
	if a.CreatedAt.Location() != time.UTC || !a.CreatedAt.Equal(created) {
		t.Fatalf("times = %s", a.CreatedAt)
	}
}

func TestGetAccountErrors(t *testing.T) {
	mock, store := newMock(t)
	id := uuid.New()
	mock.ExpectQuery(`FROM accounts a`).WithArgs(id).WillReturnRows(mock.NewRows(accountCols))
	mock.ExpectQuery(`FROM accounts a`).WithArgs(id).WillReturnRows(
		mock.NewRows(accountCols).AddRow(id, "FROZEN", int64(1), time.Now(), time.Now(), []string{}))

	_ = store.Run(ctx, func(q service.Queries) error {
		if _, err := q.GetAccount(ctx, id); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("missing row: %v", err)
		}
		if _, err := q.GetAccount(ctx, id); err == nil {
			t.Error("unknown status accepted")
		}
		return nil
	})
}

func TestCreateAccount(t *testing.T) {
	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	a := domain.NewAccount(uuid.New(), now)
	reqID := uuid.New()

	t.Run("new", func(t *testing.T) {
		mock, store := newMock(t)
		mock.ExpectBegin()
		mock.ExpectQuery(`INSERT INTO accounts`).
			WithArgs(a.ID, "ACTIVE", int64(1), reqID, now).
			WillReturnRows(mock.NewRows([]string{"id"}).AddRow(a.ID))
		// A new account holds no role, so no account_roles insert.
		mock.ExpectQuery(`FROM accounts a WHERE a.id`).WithArgs(a.ID).WillReturnRows(
			mock.NewRows(accountCols).AddRow(a.ID, "ACTIVE", int64(1), now, now, []string{}))
		mock.ExpectCommit()

		var (
			got     domain.Account
			created bool
		)
		err := store.InTx(ctx, func(q service.Queries) (err error) { got, created, err = q.CreateAccount(ctx, a, reqID); return })
		if err != nil || !created || got.ID != a.ID || len(got.Roles) != 0 {
			t.Fatalf("got %+v created=%v err=%v", got, created, err)
		}
	})

	t.Run("same request id", func(t *testing.T) {
		mock, store := newMock(t)
		existing := uuid.New()
		mock.ExpectBegin()
		mock.ExpectQuery(`INSERT INTO accounts`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			reqID, pgxmock.AnyArg()).WillReturnRows(mock.NewRows([]string{"id"}))
		mock.ExpectQuery(`SELECT id FROM accounts WHERE create_request_id = \$1`).WithArgs(reqID).
			WillReturnRows(mock.NewRows([]string{"id"}).AddRow(existing))
		mock.ExpectQuery(`FROM accounts a WHERE a.id`).WithArgs(existing).WillReturnRows(
			mock.NewRows(accountCols).AddRow(existing, "ACTIVE", int64(4), now, now, []string{}))
		mock.ExpectCommit()

		var (
			got     domain.Account
			created bool
		)
		err := store.InTx(ctx, func(q service.Queries) (err error) { got, created, err = q.CreateAccount(ctx, a, reqID); return })
		if err != nil || created || got.ID != existing || got.Version != 4 {
			t.Fatalf("got %+v created=%v err=%v", got, created, err)
		}
	})
}

func TestInTxRollsBackOnError(t *testing.T) {
	mock, store := newMock(t)
	mock.ExpectBegin()
	mock.ExpectRollback()
	boom := errors.New("boom")
	if err := store.InTx(ctx, func(service.Queries) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestProcessOutbox(t *testing.T) {
	msgCols := []string{"id", "topic", "record_key", "payload", "headers", "event_type", "created_at"}
	id := uuid.New()
	row := func(mock pgxmock.PgxPoolIface) *pgxmock.Rows {
		return mock.NewRows(msgCols).AddRow(id, events.TopicAccount, "key", []byte{1, 2},
			map[string]string{"event-id": id.String()}, events.AccountCreatedType, time.Now())
	}

	t.Run("published", func(t *testing.T) {
		mock, store := newMock(t)
		mock.ExpectBegin()
		mock.ExpectQuery(`FROM outbox`).WithArgs(10).WillReturnRows(row(mock))
		mock.ExpectExec(`SET published_at`).WithArgs([]uuid.UUID{id}, pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		mock.ExpectCommit()
		var seen []events.Message
		n, err := store.ProcessOutbox(ctx, 10, func(_ context.Context, ms []events.Message) error { seen = ms; return nil })
		if err != nil || n != 1 || len(seen) != 1 || seen[0].ID != id || seen[0].Headers["event-id"] != id.String() {
			t.Fatalf("n=%d err=%v seen=%+v", n, err, seen)
		}
	})

	t.Run("kafka down", func(t *testing.T) {
		mock, store := newMock(t)
		mock.ExpectBegin()
		mock.ExpectQuery(`FROM outbox`).WithArgs(10).WillReturnRows(row(mock))
		mock.ExpectExec(`SET attempts = attempts \+ 1`).WithArgs([]uuid.UUID{id}, "broker down").
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		mock.ExpectCommit()
		n, err := store.ProcessOutbox(ctx, 10, func(context.Context, []events.Message) error { return errors.New("broker down") })
		if err == nil || n != 1 {
			t.Fatalf("n=%d err=%v", n, err)
		}
	})

	t.Run("empty", func(t *testing.T) {
		mock, store := newMock(t)
		mock.ExpectBegin()
		mock.ExpectQuery(`FROM outbox`).WithArgs(10).WillReturnRows(mock.NewRows(msgCols))
		mock.ExpectCommit()
		n, err := store.ProcessOutbox(ctx, 10, func(context.Context, []events.Message) error {
			t.Fatal("publish called for an empty batch")
			return nil
		})
		if err != nil || n != 0 {
			t.Fatalf("n=%d err=%v", n, err)
		}
	})
}

func TestEnumMapsRoundTrip(t *testing.T) {
	check := func(name string, texts []string, round func(string) (string, error)) {
		for _, s := range texts {
			if got, err := round(s); err != nil || got != s {
				t.Errorf("%s %q -> %q, %v", name, s, got, err)
			}
		}
	}
	check("role", []string{"ADMIN"}, func(s string) (string, error) {
		v, err := roles.value(s)
		if err != nil {
			return "", err
		}
		return roles.text(v)
	})
	if _, err := roles.text(commonv1.Role_ROLE_UNSPECIFIED); err == nil {
		t.Error("ROLE_UNSPECIFIED must not be storable")
	}
	check("account status", []string{"ACTIVE", "BLOCKED", "DELETED"}, func(s string) (string, error) {
		v, err := accountStatuses.value(s)
		if err != nil {
			return "", err
		}
		return accountStatuses.text(v)
	})
}
