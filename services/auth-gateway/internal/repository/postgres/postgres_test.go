package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v5"

	"github.com/tbilol/dxrl/services/auth-gateway/internal/domain"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/events"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/service"
)

var (
	ctx      = context.Background()
	credCols = []string{"id", "user_id", "method", "subject", "password_hash", "display_hint", "linked_at", "last_used_at"}
	sessCols = []string{"id", "user_id", "method", "platform", "device_id", "device_name", "app_version",
		"created_at", "last_seen_at", "expires_at", "revoked_at", "revoked_reason"}
	challengeCols = []string{"id", "phone_number", "purpose", "code_hash", "attempts", "max_attempts",
		"created_at", "expires_at", "resend_available_at", "consumed_at"}
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

// anyArgs matches n arguments of any value.
func anyArgs(n int) []any {
	args := make([]any, n)
	for i := range args {
		args[i] = pgxmock.AnyArg()
	}
	return args
}

// run executes fn outside a transaction and fails the test on error.
func run(t *testing.T, s *Store, fn func(q service.Queries) error) {
	t.Helper()
	if err := s.Run(ctx, fn); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialBySubjectMapsRow(t *testing.T) {
	mock, store := newMock(t)
	id, userID := uuid.New(), uuid.New()
	linked := time.Date(2026, 9, 22, 15, 0, 0, 0, tashkent)
	used := linked.Add(time.Hour)
	mock.ExpectQuery(`FROM sign_in_methods WHERE method = \$1 AND subject = \$2`).
		WithArgs("PHONE_PASSWORD", "+998901234567").
		WillReturnRows(mock.NewRows(credCols).AddRow(id, userID, "PHONE_PASSWORD", "+998901234567",
			"$argon2id$v=19$m=8192,t=1,p=1$c2FsdA$aGFzaA", "+998 90 *** ** 67", linked, &used))

	var got domain.Credential
	run(t, store, func(q service.Queries) error {
		var err error
		got, err = q.CredentialBySubject(ctx, domain.MethodPhonePassword, "+998901234567")
		return err
	})
	if got.ID != id || got.UserID != userID || got.Method != domain.MethodPhonePassword ||
		got.DisplayHint != "+998 90 *** ** 67" || got.PasswordHash == "" {
		t.Fatalf("credential = %+v", got)
	}
	// Timestamps are normalized to UTC.
	if got.LinkedAt.Location() != time.UTC || !got.LinkedAt.Equal(linked) || got.LastUsedAt == nil ||
		got.LastUsedAt.Location() != time.UTC {
		t.Fatalf("timestamps = %v / %v", got.LinkedAt, got.LastUsedAt)
	}
}

func TestCredentialNotFound(t *testing.T) {
	mock, store := newMock(t)
	mock.ExpectQuery(`FROM sign_in_methods`).WithArgs("GOOGLE", "sub").WillReturnRows(mock.NewRows(credCols))
	err := store.Run(ctx, func(q service.Queries) error {
		_, err := q.CredentialBySubject(ctx, domain.MethodGoogle, "sub")
		return err
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want domain.ErrNotFound", err)
	}
}

// The two unique constraints are the guards behind PROVIDER_ALREADY_LINKED and
// PHONE_ALREADY_REGISTERED; the repository must recognize them by name.
func TestInsertCredentialUniqueViolations(t *testing.T) {
	cred := domain.Credential{
		ID: uuid.New(), UserID: uuid.New(), Method: domain.MethodGoogle, Subject: "104711920733",
		DisplayHint: "a***@gmail.com", LinkedAt: time.Now(),
	}
	for name, tc := range map[string]struct {
		constraint string
		want       error
	}{
		"same method twice":  {userMethodKey, domain.ErrMethodLinked},
		"subject taken":      {methodSubjectKey, domain.ErrSubjectTaken},
		"another constraint": {"sign_in_methods_password_check", nil},
	} {
		t.Run(name, func(t *testing.T) {
			mock, store := newMock(t)
			mock.ExpectExec(`INSERT INTO sign_in_methods`).WithArgs(anyArgs(8)...).
				WillReturnError(&pgconn.PgError{Code: "23505", ConstraintName: tc.constraint})
			err := store.Run(ctx, func(q service.Queries) error { return q.InsertCredential(ctx, cred) })
			switch {
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("error = %v, want %v", err, tc.want)
			case tc.want == nil && (errors.Is(err, domain.ErrMethodLinked) || errors.Is(err, domain.ErrSubjectTaken)):
				t.Fatalf("an unrelated constraint must not be translated: %v", err)
			case err == nil:
				t.Fatal("want an error")
			}
		})
	}
}

// A password hash is stored as NULL for Google and Apple (the CHECK constraint
// enforces it).
func TestInsertCredentialWithoutPassword(t *testing.T) {
	mock, store := newMock(t)
	cred := domain.Credential{
		ID: uuid.New(), UserID: uuid.New(), Method: domain.MethodApple, Subject: "000123.abc",
		DisplayHint: "0***bc", LinkedAt: time.Now(),
	}
	mock.ExpectExec(`INSERT INTO sign_in_methods`).
		WithArgs(cred.ID, cred.UserID, "APPLE", cred.Subject, (*string)(nil), cred.DisplayHint, cred.LinkedAt,
			(*time.Time)(nil)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	run(t, store, func(q service.Queries) error { return q.InsertCredential(ctx, cred) })
}

func TestSessionMappingAndRevocation(t *testing.T) {
	mock, store := newMock(t)
	id, userID := uuid.New(), uuid.New()
	created := time.Date(2026, 9, 22, 15, 0, 0, 0, tashkent)
	mock.ExpectQuery(`FROM sessions WHERE id = \$1`).WithArgs(id).WillReturnRows(
		mock.NewRows(sessCols).AddRow(id, userID, "GOOGLE", "ANDROID", "d-1", "Pixel 8", "1.4.0",
			created, created, created.Add(720*time.Hour), nil, ""))

	var got domain.Session
	run(t, store, func(q service.Queries) error {
		var err error
		got, err = q.GetSession(ctx, id)
		return err
	})
	if got.Method != domain.MethodGoogle || got.Client.DeviceName != "Pixel 8" || got.RevokedAt != nil ||
		!got.Live(created.Add(time.Hour)) {
		t.Fatalf("session = %+v", got)
	}

	// RevokeSession is scoped to the owner and to live sessions.
	now := created.Add(time.Hour)
	mock.ExpectExec(`UPDATE sessions SET revoked_at`).
		WithArgs(id, userID, now, domain.RevokedSignOut).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	run(t, store, func(q service.Queries) error {
		ok, err := q.RevokeSession(ctx, id, userID, domain.RevokedSignOut, now)
		if ok {
			t.Fatal("revoking somebody else's session must report false")
		}
		return err
	})
}

// Rotation and single use are guarded updates: the row count decides.
func TestUseRefreshTokenIsGuarded(t *testing.T) {
	mock, store := newMock(t)
	id, next := uuid.New(), uuid.New()
	now := time.Now()
	mock.ExpectExec(`UPDATE refresh_tokens SET used_at`).WithArgs(id, now, next).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`UPDATE refresh_tokens SET used_at`).WithArgs(id, now, next).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	run(t, store, func(q service.Queries) error {
		used, err := q.UseRefreshToken(ctx, id, next, now)
		if err != nil || !used {
			t.Fatalf("first use = %v, %v", used, err)
		}
		used, err = q.UseRefreshToken(ctx, id, next, now)
		if used {
			t.Fatal("a token can only be used once")
		}
		return err
	})
}

func TestChallengeMappingAndConsumption(t *testing.T) {
	mock, store := newMock(t)
	id := uuid.New()
	created := time.Date(2026, 9, 22, 15, 0, 0, 0, tashkent)
	mock.ExpectQuery(`FROM phone_challenges\s+WHERE phone_number = \$1 AND purpose = \$2`).
		WithArgs("+998901234567", "SIGN_UP").
		WillReturnRows(mock.NewRows(challengeCols).AddRow(id, "+998901234567", "SIGN_UP", make([]byte, 32),
			1, 5, created, created.Add(10*time.Minute), created.Add(time.Minute), nil))

	var got domain.PhoneChallenge
	run(t, store, func(q service.Queries) error {
		var err error
		got, err = q.LatestChallenge(ctx, "+998901234567", domain.PurposeSignUp)
		return err
	})
	if got.Purpose != domain.PurposeSignUp || got.Attempts != 1 || got.MaxAttempts != 5 ||
		!got.Usable(created.Add(time.Minute)) || got.ExpiresAt.Location() != time.UTC {
		t.Fatalf("challenge = %+v", got)
	}

	now := created.Add(2 * time.Minute)
	mock.ExpectExec(`UPDATE phone_challenges SET consumed_at`).WithArgs(id, now).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	run(t, store, func(q service.Queries) error {
		consumed, err := q.ConsumeChallenge(ctx, id, now)
		if consumed {
			t.Fatal("a spent challenge must report false")
		}
		return err
	})
}

func TestInTxRollsBackOnError(t *testing.T) {
	mock, store := newMock(t)
	mock.ExpectBegin()
	mock.ExpectRollback()
	want := errors.New("boom")
	if err := store.InTx(ctx, func(service.Queries) error { return want }); !errors.Is(err, want) {
		t.Fatalf("InTx = %v", err)
	}
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
}

func TestEnumMapsRoundTrip(t *testing.T) {
	for _, m := range []domain.Credential{{Method: domain.MethodGoogle}, {Method: domain.MethodApple},
		{Method: domain.MethodPhonePassword}} {
		text, err := methods.text(m.Method)
		if err != nil {
			t.Fatal(err)
		}
		back, err := methods.value(text)
		if err != nil || back != m.Method {
			t.Fatalf("%v -> %q -> %v (%v)", m.Method, text, back, err)
		}
	}
	if _, err := methods.text(domain.MethodUnspecified); err == nil {
		t.Fatal("an unspecified method must not be storable")
	}
	if _, err := purposes.value("WHATEVER"); err == nil {
		t.Fatal("an unknown purpose in the database must be an error")
	}
	if _, err := platforms.text(-1); err == nil {
		t.Fatal("an unknown platform must not be storable")
	}
}
