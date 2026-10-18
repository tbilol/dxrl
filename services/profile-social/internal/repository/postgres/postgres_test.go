package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v5"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
	"github.com/tbilol/dxrl/services/profile-social/internal/service"
)

var (
	ctx             = context.Background()
	profileColNames = []string{"id", "status", "display_name", "avatar_media_id", "bio", "region", "district",
		"create_request_id", "version", "created_at", "updated_at", "deleted_at",
		"follower_count", "following_count", "roles", "account_version", "is_followed_by_me"}
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

func TestGetProfileMapsRow(t *testing.T) {
	mock, store := newMock(t)
	id, viewer, avatar, req := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	created := time.Date(2026, 9, 24, 15, 0, 0, 0, tashkent)
	name, region := "Gulnora", "SURXONDARYO"
	mock.ExpectQuery(`FROM profiles p WHERE p.id = \$1`).WithArgs(id, viewer).WillReturnRows(
		mock.NewRows(profileColNames).AddRow(id, "ACTIVE", &name, &avatar, "bio", &region, "Termiz",
			&req, int64(3), &created, created, (*time.Time)(nil),
			int64(5), int64(6), []string{"ADMIN"}, int64(7), true))

	var p domain.Profile
	run(t, store, func(q service.Queries) (err error) { p, err = q.GetProfile(ctx, id, viewer); return })
	if p.ID != id || p.Status != domain.ProfileActive || p.DisplayName != "Gulnora" || p.AvatarMediaID != avatar ||
		p.Region != commonv1.Region_REGION_SURXONDARYO || p.District != "Termiz" || p.CreateRequestID != req ||
		p.Version != 3 || p.FollowerCount != 5 || p.FollowingCount != 6 || !p.IsFollowedByMe || p.Account.Version != 7 {
		t.Fatalf("profile = %+v", p)
	}
	if len(p.Account.Roles) != 1 || p.Account.Roles[0] != domain.RoleAdmin {
		t.Fatalf("roles = %v", p.Account.Roles)
	}
	if p.CreatedAt.Location() != time.UTC || !p.CreatedAt.Equal(created) || p.DeletedAt != nil {
		t.Fatalf("times = %s, %v", p.CreatedAt, p.DeletedAt)
	}
}

func TestGetProfilePendingRowAndErrors(t *testing.T) {
	mock, store := newMock(t)
	id := uuid.New()
	now := time.Now()
	pendingRow := func(status string) *pgxmock.Rows {
		return mock.NewRows(profileColNames).AddRow(id, status, (*string)(nil), (*uuid.UUID)(nil), "", (*string)(nil), "",
			(*uuid.UUID)(nil), int64(0), (*time.Time)(nil), now, (*time.Time)(nil),
			int64(0), int64(0), []string{}, int64(1), false)
	}
	mock.ExpectQuery(`FROM profiles p`).WithArgs(id, uuid.Nil).WillReturnRows(pendingRow("PENDING"))
	mock.ExpectQuery(`FROM profiles p`).WithArgs(id, uuid.Nil).WillReturnRows(mock.NewRows(profileColNames))
	mock.ExpectQuery(`FROM profiles p`).WithArgs(id, uuid.Nil).WillReturnRows(pendingRow("FROZEN"))

	_ = store.Run(ctx, func(q service.Queries) error {
		p, err := q.GetProfile(ctx, id, uuid.Nil)
		if err != nil || p.Status != domain.ProfilePending || p.DisplayName != "" || !p.CreatedAt.IsZero() || p.Public() {
			t.Fatalf("pending = %+v, %v", p, err)
		}
		if _, err := q.GetProfile(ctx, id, uuid.Nil); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing row: %v", err)
		}
		if _, err := q.GetProfile(ctx, id, uuid.Nil); err == nil {
			t.Fatal("an unknown status must be an error")
		}
		return nil
	})
}

func TestApplyAccountIsVersionGuarded(t *testing.T) {
	mock, store := newMock(t)
	id := uuid.New()
	now := time.Now().UTC()
	a := domain.Account{Roles: []commonv1.Role{domain.RoleAdmin}, Version: 4}
	const sql = `ON CONFLICT \(id\) DO UPDATE .* WHERE profiles.account_version < EXCLUDED.account_version`
	mock.ExpectExec(sql).WithArgs(id, []string{"ADMIN"}, int64(4), now).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(sql).WithArgs(id, []string{"ADMIN"}, int64(4), now).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))

	run(t, store, func(q service.Queries) error {
		if ok, err := q.ApplyAccount(ctx, id, a, now); err != nil || !ok {
			t.Fatalf("newer snapshot: %v, %v", ok, err)
		}
		if ok, err := q.ApplyAccount(ctx, id, a, now); err != nil || ok {
			t.Fatalf("stale snapshot: %v, %v", ok, err)
		}
		return nil
	})
	if _, err := (&queries{db: nil}).ApplyAccount(ctx, id, domain.Account{Roles: []commonv1.Role{commonv1.Role(99)}}, now); err == nil {
		t.Fatal("a role that cannot be stored must be an error")
	}
}

func TestMarkProcessed(t *testing.T) {
	mock, store := newMock(t)
	e := domain.ProcessedEvent{EventID: uuid.New(), Topic: "t", EventType: "agrogram.events.v1.AccountCreated",
		AggregateID: uuid.New(), AggregateVersion: 1, ProcessedAt: time.Now()}
	for _, n := range []int64{1, 0} {
		mock.ExpectExec(`INSERT INTO processed_events .* ON CONFLICT \(event_id\) DO NOTHING`).
			WithArgs(e.EventID, e.Topic, e.EventType, e.AggregateID, e.AggregateVersion, e.ProcessedAt).
			WillReturnResult(pgxmock.NewResult("INSERT", n))
	}
	run(t, store, func(q service.Queries) error {
		if fresh, err := q.MarkProcessed(ctx, e); err != nil || !fresh {
			t.Fatalf("first: %v, %v", fresh, err)
		}
		if fresh, err := q.MarkProcessed(ctx, e); err != nil || fresh {
			t.Fatalf("duplicate: %v, %v", fresh, err)
		}
		return nil
	})
}

func TestInsertFollowCountsOnlyNewEdges(t *testing.T) {
	mock, store := newMock(t)
	a, b := uuid.New(), uuid.New()
	now := time.Now()
	mock.ExpectExec(`INSERT INTO follows .* ON CONFLICT \(follower_id, followee_id\) DO NOTHING`).
		WithArgs(a, b, now).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`UPDATE profiles\s+SET follower_count = follower_count \+ \(id = \$2\)::int`).
		WithArgs(a, b).WillReturnResult(pgxmock.NewResult("UPDATE", 2))
	mock.ExpectExec(`INSERT INTO follows`).WithArgs(a, b, now).WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectExec(`DELETE FROM follows WHERE follower_id = \$1 AND followee_id = \$2`).
		WithArgs(a, b).WillReturnResult(pgxmock.NewResult("DELETE", 0))

	run(t, store, func(q service.Queries) error {
		if ok, err := q.InsertFollow(ctx, a, b, now); err != nil || !ok {
			t.Fatalf("new edge: %v, %v", ok, err)
		}
		if ok, err := q.InsertFollow(ctx, a, b, now); err != nil || ok {
			t.Fatalf("existing edge: %v, %v", ok, err)
		}
		if ok, err := q.DeleteFollow(ctx, a, b); err != nil || ok {
			t.Fatalf("missing edge: %v, %v", ok, err)
		}
		return nil
	})
}

func TestLockProfilesInIDOrder(t *testing.T) {
	mock, store := newMock(t)
	a, b := uuid.New(), uuid.New()
	mock.ExpectQuery(`WHERE p.id = ANY\(\$1\)\s+ORDER BY p.id\s+FOR NO KEY UPDATE`).
		WithArgs([]uuid.UUID{a, b}).WillReturnRows(mock.NewRows(profileColNames))
	run(t, store, func(q service.Queries) error {
		rows, err := q.LockProfiles(ctx, a, b)
		if err != nil || len(rows) != 0 {
			t.Fatalf("rows = %v, %v", rows, err)
		}
		return nil
	})
}

func TestListFollowersKeyset(t *testing.T) {
	mock, store := newMock(t)
	id, viewer, last := uuid.New(), uuid.New(), uuid.New()
	at := time.Now().UTC()
	mock.ExpectQuery(`JOIN profiles p ON p.id = f.follower_id\s+WHERE f.followee_id = \$1 AND p.status = 'ACTIVE' `+
		`AND \(f.created_at, f.follower_id\) < \(\$4, \$5\)\s+ORDER BY f.created_at DESC, f.follower_id DESC\s+LIMIT \$3`).
		WithArgs(id, viewer, 3, at, last).WillReturnRows(mock.NewRows(append(profileColNames, "followed_at")))
	mock.ExpectQuery(`JOIN profiles p ON p.id = f.followee_id\s+WHERE f.follower_id = \$1 AND p.status = 'ACTIVE'\s+ORDER BY`).
		WithArgs(id, viewer, 3).WillReturnRows(mock.NewRows(append(profileColNames, "followed_at")))
	run(t, store, func(q service.Queries) error {
		if _, err := q.ListFollowers(ctx, id, viewer, &domain.Cursor{At: at, ID: last}, 3); err != nil {
			t.Fatal(err)
		}
		if _, err := q.ListFollowing(ctx, id, viewer, nil, 3); err != nil {
			t.Fatal(err)
		}
		return nil
	})
}

func TestInTxRollsBackOnError(t *testing.T) {
	mock, store := newMock(t)
	boom := errors.New("boom")
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO profiles`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectRollback()
	err := store.InTx(ctx, func(q service.Queries) error {
		if err := q.EnsureProfile(ctx, uuid.New(), time.Now()); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestIsTransient(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"nil":                {nil, false},
		"deadline":           {fmt.Errorf("apply: %w", context.DeadlineExceeded), true},
		"admin shutdown":     {&pgconn.PgError{Code: "57P01"}, true},
		"cannot connect now": {&pgconn.PgError{Code: "57P03"}, true},
		"connection failure": {&pgconn.PgError{Code: "08006"}, true},
		"too many conns":     {&pgconn.PgError{Code: "53300"}, true},
		"deadlock":           {fmt.Errorf("tx: %w", &pgconn.PgError{Code: "40P01"}), true},
		"serialization":      {&pgconn.PgError{Code: "40001"}, true},
		"lock not available": {&pgconn.PgError{Code: "55P03"}, true},
		"not migrated":       {fmt.Errorf("apply: %w", &pgconn.PgError{Code: "42P01"}), true},
		"no privilege":       {&pgconn.PgError{Code: "42501"}, true},
		"bad password":       {&pgconn.PgError{Code: "28P01"}, true},
		"no database":        {&pgconn.PgError{Code: "3D000"}, true},
		"network":            {&net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		"eof":                {fmt.Errorf("read: %w", io.ErrUnexpectedEOF), true},
		"check violation":    {&pgconn.PgError{Code: "23514"}, false},
		"unique violation":   {&pgconn.PgError{Code: "23505"}, false},
		"bad data":           {&pgconn.PgError{Code: "22P02"}, false},
		"other":              {errors.New("unknown role in database"), false},
	}
	for name, tc := range cases {
		if got := IsTransient(tc.err); got != tc.want {
			t.Errorf("%s: IsTransient = %v, want %v", name, got, tc.want)
		}
	}
}

func TestPurgeProcessedEvents(t *testing.T) {
	mock, store := newMock(t)
	cutoff := time.Now()
	mock.ExpectExec(`DELETE FROM processed_events WHERE processed_at < \$1`).WithArgs(cutoff).
		WillReturnResult(pgxmock.NewResult("DELETE", 4))
	if n, err := store.PurgeProcessedEvents(ctx, cutoff); err != nil || n != 4 {
		t.Fatalf("purged %d, %v", n, err)
	}
}
