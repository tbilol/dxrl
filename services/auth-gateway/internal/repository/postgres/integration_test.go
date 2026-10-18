//go:build integration

// Integration tests against a real PostgreSQL. They run only with
// `-tags=integration` and AUTH_GATEWAY_TEST_DATABASE_URL set (make
// test-integration loads it from .env). Each run migrates a fresh temporary
// schema (selected through search_path) and drops it afterwards, so the
// target database is left as it was.
package postgres_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	authgatewayv1 "github.com/tbilol/dxrl/gen/go/authgateway/v1"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	identityv1 "github.com/tbilol/dxrl/gen/go/identity/v1"

	"github.com/tbilol/dxrl/services/auth-gateway/internal/apperr"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/auth"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/domain"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/events"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/identityclient"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/password"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/provider"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/repository/postgres"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/service"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/sms"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/token"
	"github.com/tbilol/dxrl/services/auth-gateway/migrations"
)

var ctx = context.Background()

// setup creates a temporary schema, migrates it and returns a pool whose
// search_path points at it.
func setup(t *testing.T) (*pgxpool.Pool, *goose.Provider) {
	t.Helper()
	url := os.Getenv("AUTH_GATEWAY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AUTH_GATEWAY_TEST_DATABASE_URL is not set")
	}
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	schema := "auth_gateway_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+ident+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
		_ = admin.Close(context.Background())
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	provider, err := migrations.NewProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return pool, provider
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

// fakeAccounts stands in for identity (no gRPC in these tests).
type fakeAccounts struct {
	byRequest map[uuid.UUID]uuid.UUID
	accounts  map[uuid.UUID]identityclient.Account
}

func newFakeAccounts() *fakeAccounts {
	return &fakeAccounts{byRequest: map[uuid.UUID]uuid.UUID{}, accounts: map[uuid.UUID]identityclient.Account{}}
}

func (a *fakeAccounts) CreateAccount(_ context.Context, _ auth.Principal, requestID uuid.UUID) (identityclient.Account, error) {
	if id, ok := a.byRequest[requestID]; ok {
		return a.accounts[id], nil
	}
	acct := identityclient.Account{
		ID:     uuid.New(),
		Status: identityv1.AccountStatus_ACCOUNT_STATUS_ACTIVE,
	}
	a.byRequest[requestID], a.accounts[acct.ID] = acct.ID, acct
	return acct, nil
}

func (a *fakeAccounts) GetAccount(_ context.Context, _ auth.Principal, userID uuid.UUID) (identityclient.Account, error) {
	acct, ok := a.accounts[userID]
	if !ok {
		return identityclient.Account{}, apperr.New(commonv1.ErrorReason_ERROR_REASON_NOT_FOUND, "account not found")
	}
	return acct, nil
}

// recordingSender keeps the codes, which is the only way to answer a challenge.
type recordingSender struct{ codes []string }

func (s *recordingSender) Send(_ context.Context, _, code string, _ authgatewayv1.PhoneCodePurpose) error {
	s.codes = append(s.codes, code)
	return nil
}

func newService(t *testing.T, store *postgres.Store, accounts service.Accounts, sender sms.Sender, now func() time.Time) *service.Service {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := token.LoadKeySet(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := token.NewIssuer(keys, token.Config{
		Issuer: "https://auth.agrogram.local", Audience: "agrogram-api", TTL: 15 * time.Minute,
	}, token.WithClock(now))
	if err != nil {
		t.Fatal(err)
	}
	return service.New(service.Deps{
		Store:    store,
		Accounts: accounts,
		Issuer:   issuer,
		Hasher:   password.New(password.Params{MemoryKiB: 8 * 1024, Time: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}),
		Google:   provider.GoogleStub{},
		Apple:    provider.AppleStub{},
		Sender:   sender,
	}, service.Config{
		RefreshTokenTTL:      30 * 24 * time.Hour,
		PhoneCodeTTL:         10 * time.Minute,
		PhoneCodeResend:      time.Minute,
		PhoneCodeLength:      6,
		PhoneCodeMaxAttempts: 3,
		VerifyTokenCaller:    auth.CallerAPIGateway,
	}, service.WithClock(now))
}

func TestIntegrationRepository(t *testing.T) {
	pool, provider := setup(t)
	store := postgres.New(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)

	var q service.Queries
	_ = store.Run(ctx, func(qq service.Queries) error { q = qq; return nil }) // autocommit queries

	userA, userB := uuid.New(), uuid.New()

	t.Run("credentials and their unique constraints", func(t *testing.T) {
		phone := domain.Credential{
			ID: uuid.Must(uuid.NewV7()), UserID: userA, Method: domain.MethodPhonePassword,
			Subject: "+998901234567", PasswordHash: "$argon2id$v=19$m=8192,t=1,p=1$c2FsdA$aGFzaA",
			DisplayHint: domain.MaskPhone("+998901234567"), LinkedAt: now,
		}
		if err := q.InsertCredential(ctx, phone); err != nil {
			t.Fatal(err)
		}
		// The same account cannot have two credentials of one method.
		dup := phone
		dup.ID, dup.Subject = uuid.Must(uuid.NewV7()), "+998907654321"
		if err := q.InsertCredential(ctx, dup); !errors.Is(err, domain.ErrMethodLinked) {
			t.Fatalf("second phone credential = %v, want ErrMethodLinked", err)
		}
		// And a number belongs to one account only.
		other := phone
		other.ID, other.UserID = uuid.Must(uuid.NewV7()), userB
		if err := q.InsertCredential(ctx, other); !errors.Is(err, domain.ErrSubjectTaken) {
			t.Fatalf("same number on another account = %v, want ErrSubjectTaken", err)
		}

		google := domain.Credential{
			ID: uuid.Must(uuid.NewV7()), UserID: userA, Method: domain.MethodGoogle, Subject: "104711920733",
			DisplayHint: "a***@gmail.com", LinkedAt: now.Add(time.Second),
		}
		if err := q.InsertCredential(ctx, google); err != nil {
			t.Fatal(err)
		}
		creds := must[[]domain.Credential](t)(q.CredentialsByUser(ctx, userA))
		if len(creds) != 2 || creds[0].Method != domain.MethodPhonePassword || creds[1].Method != domain.MethodGoogle {
			t.Fatalf("credentials = %+v", creds)
		}
		if creds[0].PasswordHash == "" || creds[1].PasswordHash != "" {
			t.Fatalf("password hashes = %q / %q (only PHONE_PASSWORD has one)", creds[0].PasswordHash, creds[1].PasswordHash)
		}

		if err := q.TouchCredential(ctx, phone.ID, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := q.UpdatePasswordHash(ctx, phone.ID, "$argon2id$v=19$m=8192,t=1,p=1$c2FsdA$bmV3"); err != nil {
			t.Fatal(err)
		}
		got := must[domain.Credential](t)(q.CredentialBySubject(ctx, domain.MethodPhonePassword, "+998901234567"))
		if got.LastUsedAt == nil || !strings.HasSuffix(got.PasswordHash, "bmV3") {
			t.Fatalf("credential = %+v", got)
		}

		deleted := must[bool](t)(q.DeleteCredential(ctx, userA, domain.MethodGoogle))
		if !deleted {
			t.Fatal("delete reported false")
		}
		if again := must[bool](t)(q.DeleteCredential(ctx, userA, domain.MethodGoogle)); again {
			t.Fatal("deleting twice must report false")
		}
	})

	t.Run("sessions and refresh token rotation", func(t *testing.T) {
		session := domain.Session{
			ID: uuid.Must(uuid.NewV7()), UserID: userA, Method: domain.MethodPhonePassword,
			Client: domain.ClientInfo{
				Platform: authgatewayv1.ClientPlatform_CLIENT_PLATFORM_ANDROID, DeviceID: "d-1", DeviceName: "Pixel 8",
			},
			CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(720 * time.Hour),
		}
		if err := q.InsertSession(ctx, session); err != nil {
			t.Fatal(err)
		}
		first := domain.RefreshToken{
			ID: uuid.Must(uuid.NewV7()), SessionID: session.ID, Hash: hash32("first"),
			IssuedAt: now, ExpiresAt: now.Add(720 * time.Hour),
		}
		second := first
		second.ID, second.Hash = uuid.Must(uuid.NewV7()), hash32("second")
		if err := q.InsertRefreshToken(ctx, first); err != nil {
			t.Fatal(err)
		}
		if err := q.InsertRefreshToken(ctx, second); err != nil {
			t.Fatal(err)
		}

		found := must[domain.RefreshToken](t)(q.RefreshTokenByHash(ctx, hash32("first")))
		if found.ID != first.ID || found.UsedAt != nil {
			t.Fatalf("refresh token = %+v", found)
		}
		if used := must[bool](t)(q.UseRefreshToken(ctx, first.ID, second.ID, now.Add(time.Minute))); !used {
			t.Fatal("the first use must succeed")
		}
		if used := must[bool](t)(q.UseRefreshToken(ctx, first.ID, second.ID, now.Add(2*time.Minute))); used {
			t.Fatal("the second use must fail (that is the reuse signal)")
		}
		if _, err := q.RefreshTokenByHash(ctx, hash32("nothing")); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("unknown token = %v", err)
		}

		// Only the owner revokes, and only once.
		if ok := must[bool](t)(q.RevokeSession(ctx, session.ID, userB, domain.RevokedSignOut, now)); ok {
			t.Fatal("another user must not revoke the session")
		}
		if ok := must[bool](t)(q.RevokeSession(ctx, session.ID, userA, domain.RevokedSignOut, now)); !ok {
			t.Fatal("the owner must revoke the session")
		}
		if ok := must[bool](t)(q.RevokeSession(ctx, session.ID, userA, domain.RevokedSignOut, now)); ok {
			t.Fatal("revoking twice must report false")
		}
		got := must[domain.Session](t)(q.GetSession(ctx, session.ID))
		if got.RevokedAt == nil || got.RevokedReason != domain.RevokedSignOut || got.Live(now) {
			t.Fatalf("session = %+v", got)
		}
		if n := must[int64](t)(q.RevokeUserSessions(ctx, userA, domain.RevokedPasswordReset, now)); n != 0 {
			t.Fatalf("revoked %d sessions, want 0 (none was live)", n)
		}
	})

	t.Run("phone challenges", func(t *testing.T) {
		mk := func(created time.Time) domain.PhoneChallenge {
			c := domain.PhoneChallenge{
				ID: uuid.Must(uuid.NewV7()), PhoneNumber: "+998901112233", Purpose: domain.PurposeSignUp,
				CodeHash: hash32("code"), MaxAttempts: 3, CreatedAt: created,
				ExpiresAt: created.Add(10 * time.Minute), ResendAvailableAt: created.Add(time.Minute),
			}
			if err := q.InsertChallenge(ctx, c); err != nil {
				t.Fatal(err)
			}
			return c
		}
		older := mk(now)
		newer := mk(now.Add(time.Second))
		latest := must[domain.PhoneChallenge](t)(q.LatestChallenge(ctx, "+998901112233", domain.PurposeSignUp))
		if latest.ID != newer.ID {
			t.Fatalf("latest challenge = %s, want the newest one", latest.ID)
		}
		if _, err := q.LatestChallenge(ctx, "+998901112233", domain.PurposeResetPassword); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("another purpose = %v, want ErrNotFound", err)
		}

		for range 3 {
			if err := q.CountChallengeAttempt(ctx, older.ID); err != nil {
				t.Fatal(err)
			}
		}
		if ok := must[bool](t)(q.ConsumeChallenge(ctx, older.ID, now.Add(time.Minute))); ok {
			t.Fatal("a challenge out of attempts must not be consumable")
		}
		if ok := must[bool](t)(q.ConsumeChallenge(ctx, newer.ID, now.Add(11*time.Minute))); ok {
			t.Fatal("an expired challenge must not be consumable")
		}
		if ok := must[bool](t)(q.ConsumeChallenge(ctx, newer.ID, now.Add(time.Minute))); !ok {
			t.Fatal("a usable challenge must be consumable")
		}
		if ok := must[bool](t)(q.ConsumeChallenge(ctx, newer.ID, now.Add(time.Minute))); ok {
			t.Fatal("a challenge is single-use")
		}
	})

	t.Run("outbox", func(t *testing.T) {
		msg := must[events.Message](t)(events.NewNotificationRequest(events.Notification{
			RecipientID:   userA,
			Category:      eventsv1.NotificationCategory_NOTIFICATION_CATEGORY_SECURITY,
			TemplateKey:   events.TemplatePasswordChanged,
			Params:        map[string]string{"method": "PHONE_PASSWORD"},
			OccurredAt:    now,
			CorrelationID: uuid.NewString(),
			Actor:         auth.ServiceActor(),
		}))
		if err := store.InTx(ctx, func(q service.Queries) error { return q.EnqueueOutbox(ctx, msg) }); err != nil {
			t.Fatal(err)
		}
		n, err := store.ProcessOutbox(ctx, 10, func(_ context.Context, batch []events.Message) error {
			if len(batch) != 1 || batch[0].ID != msg.ID || batch[0].Topic != events.TopicNotificationRequest {
				t.Fatalf("batch = %+v", batch)
			}
			return nil
		})
		if err != nil || n != 1 {
			t.Fatalf("ProcessOutbox = %d, %v", n, err)
		}
		// Published rows are not handed out twice.
		if n, err = store.ProcessOutbox(ctx, 10, func(context.Context, []events.Message) error {
			t.Fatal("a published message was relayed twice")
			return nil
		}); err != nil || n != 0 {
			t.Fatalf("ProcessOutbox = %d, %v", n, err)
		}
		if purged := must[int64](t)(store.PurgePublished(ctx, now.Add(time.Hour))); purged != 1 {
			t.Fatalf("purged %d rows, want 1", purged)
		}
	})

	t.Run("migrations roll back and forward", func(t *testing.T) {
		if _, err := provider.DownTo(ctx, 0); err != nil {
			t.Fatalf("migrate down: %v", err)
		}
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("migrate up again: %v", err)
		}
	})
}

// TestIntegrationSignUpFlow runs the real service against PostgreSQL: sign-up,
// refresh (with rotation and reuse detection), verify and sign-out.
func TestIntegrationSignUpFlow(t *testing.T) {
	pool, _ := setup(t)
	store := postgres.New(pool)
	clock := time.Now().UTC().Truncate(time.Microsecond)
	sender := &recordingSender{}
	accounts := newFakeAccounts()
	svc := newService(t, store, accounts, sender, func() time.Time { return clock })

	const phone, pw = "+998901234567", "integration-parol"
	ch, err := svc.RequestPhoneCode(ctx, auth.Principal{RequestID: uuid.NewString()}, phone, domain.PurposeSignUp)
	if err != nil {
		t.Fatal(err)
	}
	if len(sender.codes) != 1 {
		t.Fatalf("codes sent = %d", len(sender.codes))
	}
	res, err := svc.SignUpWithPhone(ctx, auth.Principal{RequestID: uuid.NewString()}, uuid.NewString(),
		service.PhoneSetup{Phone: phone, ChallengeID: ch.ID.String(), Code: sender.codes[0], Password: pw},
		domain.ClientInfo{Platform: authgatewayv1.ClientPlatform_CLIENT_PLATFORM_WEB})
	if err != nil {
		t.Fatal(err)
	}

	signedIn, err := svc.SignIn(ctx, auth.Principal{RequestID: uuid.NewString()}, service.SignInInput{
		Method: domain.MethodPhonePassword, Phone: phone, Password: pw,
	})
	if err != nil {
		t.Fatal(err)
	}
	if signedIn.UserID != res.UserID {
		t.Fatalf("sign-in reached %s, want %s", signedIn.UserID, res.UserID)
	}

	pair, err := svc.RefreshToken(ctx, auth.Principal{RequestID: uuid.NewString()}, res.Tokens.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	internal := auth.Principal{Caller: auth.CallerAPIGateway, RequestID: uuid.NewString()}
	verified, err := svc.VerifyToken(ctx, internal, pair.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if verified.UserID != res.UserID || verified.SessionID != res.SessionID {
		t.Fatalf("verified = %+v", verified)
	}

	// Reusing the spent token kills the session.
	if _, err := svc.RefreshToken(ctx, auth.Principal{RequestID: uuid.NewString()}, res.Tokens.RefreshToken); err == nil {
		t.Fatal("reusing a rotated refresh token must fail")
	}
	if _, err := svc.VerifyToken(ctx, internal, pair.AccessToken); apperr.From(err).Reason !=
		commonv1.ErrorReason_ERROR_REASON_TOKEN_REVOKED {
		t.Fatalf("VerifyToken after reuse = %v, want TOKEN_REVOKED", err)
	}

	// The other session still works, until SignOut ends every session.
	if err := svc.SignOut(ctx, auth.Principal{UserID: res.UserID, SessionID: signedIn.SessionID,
		RequestID: uuid.NewString()}, domain.ScopeAllSessions); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RefreshToken(ctx, auth.Principal{RequestID: uuid.NewString()}, signedIn.Tokens.RefreshToken); err == nil {
		t.Fatal("a refresh token of a signed-out session must fail")
	}
}

func hash32(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
