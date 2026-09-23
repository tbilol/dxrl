package postgres

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v5"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/identity/internal/domain"
	"github.com/tbilol/dxrl/services/identity/internal/events"
	"github.com/tbilol/dxrl/services/identity/internal/service"
)

var (
	ctx         = context.Background()
	accountCols = []string{"id", "status", "plan", "premium_until", "version", "created_at", "updated_at", "roles", "fv_status"}
	verifCols   = []string{"id", "user_id", "request_id", "lease_contract_number", "status", "rejection_reason",
		"lease_valid_from", "lease_valid_until", "attempts", "next_attempt_at", "submitted_at", "decided_at"}
	tashkent = time.FixedZone("UZT", 5*3600)
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
	until := created.AddDate(0, 1, 0)
	mock.ExpectQuery(`FROM accounts a WHERE a.id = \$1`).WithArgs(id).WillReturnRows(
		mock.NewRows(accountCols).AddRow(id, "ACTIVE", "PREMIUM", &until, int64(3), created, created,
			[]string{"FARMER", "RESELLER"}, "VERIFIED"))

	var a domain.Account
	run(t, store, func(q service.Queries) (err error) { a, err = q.GetAccount(ctx, id); return })
	if a.ID != id || a.Status != domain.AccountActive || a.Plan != domain.PlanPremium || a.Version != 3 ||
		!slices.Equal(a.Roles, []commonv1.Role{domain.RoleReseller, domain.RoleFarmer}) ||
		a.FarmerVerificationStatus != domain.VerificationVerified {
		t.Fatalf("account = %+v", a)
	}
	if a.CreatedAt.Location() != time.UTC || !a.CreatedAt.Equal(created) || a.PremiumUntil == nil || !a.PremiumUntil.Equal(until) {
		t.Fatalf("times = %s, %v", a.CreatedAt, a.PremiumUntil)
	}
}

func TestGetAccountErrors(t *testing.T) {
	mock, store := newMock(t)
	id := uuid.New()
	mock.ExpectQuery(`FROM accounts a`).WithArgs(id).WillReturnRows(mock.NewRows(accountCols))
	mock.ExpectQuery(`FROM accounts a`).WithArgs(id).WillReturnRows(
		mock.NewRows(accountCols).AddRow(id, "FROZEN", "DEFAULT", (*time.Time)(nil), int64(1), time.Now(), time.Now(), []string{}, ""))

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
			WithArgs(a.ID, "ACTIVE", "DEFAULT", (*time.Time)(nil), int64(1), reqID, now).
			WillReturnRows(mock.NewRows([]string{"id"}).AddRow(a.ID))
		mock.ExpectExec(`INSERT INTO account_roles`).WithArgs(a.ID, []string{"RESELLER"}, now).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
		mock.ExpectQuery(`FROM accounts a WHERE a.id`).WithArgs(a.ID).WillReturnRows(
			mock.NewRows(accountCols).AddRow(a.ID, "ACTIVE", "DEFAULT", (*time.Time)(nil), int64(1), now, now, []string{"RESELLER"}, ""))
		mock.ExpectCommit()

		var (
			got     domain.Account
			created bool
		)
		err := store.InTx(ctx, func(q service.Queries) (err error) { got, created, err = q.CreateAccount(ctx, a, reqID); return })
		if err != nil || !created || got.ID != a.ID || got.FarmerVerificationStatus != 0 {
			t.Fatalf("got %+v created=%v err=%v", got, created, err)
		}
	})

	t.Run("same request id", func(t *testing.T) {
		mock, store := newMock(t)
		existing := uuid.New()
		mock.ExpectBegin()
		mock.ExpectQuery(`INSERT INTO accounts`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), reqID, pgxmock.AnyArg()).WillReturnRows(mock.NewRows([]string{"id"}))
		mock.ExpectQuery(`SELECT id FROM accounts WHERE create_request_id = \$1`).WithArgs(reqID).
			WillReturnRows(mock.NewRows([]string{"id"}).AddRow(existing))
		mock.ExpectQuery(`FROM accounts a WHERE a.id`).WithArgs(existing).WillReturnRows(
			mock.NewRows(accountCols).AddRow(existing, "ACTIVE", "DEFAULT", (*time.Time)(nil), int64(4), now, now, []string{"RESELLER"}, ""))
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

func TestDecideVerification(t *testing.T) {
	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	day := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	id := uuid.New()
	verified := domain.Decision{VerificationID: id, Status: domain.VerificationVerified, DecidedAt: now,
		Lease: &domain.Lease{ContractNumber: "AB-1", ValidFrom: day, ValidUntil: day.AddDate(1, 0, 0)}}
	rejected := domain.Decision{VerificationID: id, Status: domain.VerificationRejected,
		RejectionReason: domain.ReasonContractInvalid, DecidedAt: now}

	mock, store := newMock(t)
	from, until := verified.Lease.ValidFrom, verified.Lease.ValidUntil
	mock.ExpectExec(`UPDATE farmer_verifications`).WithArgs(id, "VERIFIED", (*string)(nil), &from, &until, now).
		WillReturnError(&pgconn.PgError{Code: "23505", ConstraintName: verifiedContractIndex})
	reason := "LEASE_CONTRACT_INVALID"
	mock.ExpectExec(`UPDATE farmer_verifications`).WithArgs(id, "REJECTED", &reason, (*time.Time)(nil), (*time.Time)(nil), now).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`AND status = 'PENDING'`).WithArgs(id, "REJECTED", &reason, (*time.Time)(nil), (*time.Time)(nil), now).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(`UPDATE farmer_verifications`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
		pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: "23505", ConstraintName: "some_other_key"})

	_ = store.Run(ctx, func(q service.Queries) error {
		if _, err := q.DecideVerification(ctx, verified); !errors.Is(err, domain.ErrContractClaimed) {
			t.Errorf("partial index violation: %v", err)
		}
		if ok, err := q.DecideVerification(ctx, rejected); err != nil || !ok {
			t.Errorf("rejected: ok=%v err=%v", ok, err)
		}
		if ok, err := q.DecideVerification(ctx, rejected); err != nil || ok {
			t.Errorf("no longer pending: ok=%v err=%v", ok, err)
		}
		if _, err := q.DecideVerification(ctx, verified); err == nil || errors.Is(err, domain.ErrContractClaimed) {
			t.Errorf("other unique violation: %v", err)
		}
		return nil
	})
}

func TestClaimPendingVerifications(t *testing.T) {
	mock, store := newMock(t)
	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	id, user, req := uuid.New(), uuid.New(), uuid.New()
	mock.ExpectQuery(`FOR UPDATE SKIP LOCKED`).WithArgs(now, 10, 30.0, 3600.0).WillReturnRows(
		mock.NewRows(verifCols).AddRow(id, user, req, "AB-1", "PENDING", (*string)(nil),
			(*time.Time)(nil), (*time.Time)(nil), 2, now.Add(time.Minute), now.Add(-time.Hour), (*time.Time)(nil)))

	var got []domain.Verification
	run(t, store, func(q service.Queries) (err error) {
		got, err = q.ClaimPendingVerifications(ctx, now, 10, 30*time.Second, time.Hour)
		return
	})
	if len(got) != 1 || got[0].ID != id || got[0].Status != domain.VerificationPending || got[0].Attempts != 2 ||
		got[0].Lease != nil || got[0].DecidedAt != nil || got[0].RejectionReason != 0 {
		t.Fatalf("claimed = %+v", got)
	}
}

func TestScanVerificationWithLease(t *testing.T) {
	mock, store := newMock(t)
	id := uuid.New()
	from := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	until := from.AddDate(1, 0, 0)
	decided := time.Date(2026, 9, 22, 15, 0, 0, 0, tashkent)
	reason := "LEASE_CONTRACT_ALREADY_CLAIMED"
	mock.ExpectQuery(`WHERE id = \$1`).WithArgs(id).WillReturnRows(
		mock.NewRows(verifCols).AddRow(id, uuid.New(), uuid.New(), "AB-1", "EXPIRED", (*string)(nil),
			&from, &until, 0, decided, decided, &decided))
	mock.ExpectQuery(`WHERE id = \$1`).WithArgs(id).WillReturnRows(
		mock.NewRows(verifCols).AddRow(id, uuid.New(), uuid.New(), "AB-1", "REJECTED", &reason,
			(*time.Time)(nil), (*time.Time)(nil), 0, decided, decided, &decided))

	_ = store.Run(ctx, func(q service.Queries) error {
		v, err := q.GetVerification(ctx, id)
		if err != nil || v.Status != domain.VerificationExpired || v.Lease == nil ||
			!v.Lease.ValidUntil.Equal(until) || v.Lease.ContractNumber != "AB-1" || v.DecidedAt.Location() != time.UTC {
			t.Errorf("expired = %+v, %v", v, err)
		}
		v, err = q.GetVerification(ctx, id)
		if err != nil || v.RejectionReason != domain.ReasonContractClaimed || v.Lease != nil {
			t.Errorf("rejected = %+v, %v", v, err)
		}
		return nil
	})
}

func TestProcessOutbox(t *testing.T) {
	msgCols := []string{"id", "topic", "record_key", "payload", "headers", "event_type", "created_at"}
	id := uuid.New()
	row := func(mock pgxmock.PgxPoolIface) *pgxmock.Rows {
		return mock.NewRows(msgCols).AddRow(id, events.TopicNotificationRequest, "key", []byte{1, 2},
			map[string]string{"event-id": id.String()}, events.NotificationRequestedType, time.Now())
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
	check("role", []string{"RESELLER", "FARMER", "ADMIN"}, func(s string) (string, error) {
		v, err := roles.value(s)
		if err != nil {
			return "", err
		}
		return roles.text(v)
	})
	check("verification status", []string{"PENDING", "VERIFIED", "REJECTED", "EXPIRED"}, func(s string) (string, error) {
		v, err := verificationStatuses.value(s)
		if err != nil {
			return "", err
		}
		return verificationStatuses.text(v)
	})
	if _, err := roles.text(commonv1.Role_ROLE_UNSPECIFIED); err == nil {
		t.Error("ROLE_UNSPECIFIED must not be storable")
	}
	if _, err := rejectionReasons.text(commonv1.ErrorReason_ERROR_REASON_INTERNAL); err == nil {
		t.Error("only lease reasons are storable")
	}
}
