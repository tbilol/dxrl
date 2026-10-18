package service_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	authgatewayv1 "github.com/tbilol/dxrl/gen/go/authgateway/v1"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/identity/v1"
	"google.golang.org/grpc/codes"

	"github.com/tbilol/dxrl/services/auth-gateway/internal/apperr"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/auth"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/domain"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/password"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/provider"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/service"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/token"
)

var (
	ctx   = context.Background()
	start = time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
)

const (
	testPhone    = "+998901234567"
	testPassword = "dehqon-parol-2026"
	testCaller   = "api-gateway"
)

type harness struct {
	t        *testing.T
	store    *fakeStore
	accounts *fakeAccounts
	limiter  *fakeLimiter
	sender   *fakeSender
	clock    *fakeClock
	logs     *logBuffer
	issuer   *token.Issuer
	svc      *service.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:        t,
		store:    newFakeStore(),
		accounts: newFakeAccounts(),
		limiter:  newFakeLimiter(3),
		sender:   &fakeSender{},
		clock:    &fakeClock{t: start},
		logs:     &logBuffer{},
	}
	h.issuer = testIssuer(t, h.clock.Now)
	h.svc = service.New(service.Deps{
		Store:    h.store,
		Accounts: h.accounts,
		Issuer:   h.issuer,
		// Cheap parameters: the flows are what is under test, not argon2.
		Hasher:  password.New(password.Params{MemoryKiB: 8 * 1024, Time: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}),
		Google:  provider.GoogleStub{},
		Apple:   provider.AppleStub{},
		Sender:  h.sender,
		Limiter: h.limiter,
	}, service.Config{
		RefreshTokenTTL:      30 * 24 * time.Hour,
		ProviderTimeout:      time.Second,
		PhoneCodeTTL:         10 * time.Minute,
		PhoneCodeResend:      time.Minute,
		PhoneCodeLength:      6,
		PhoneCodeMaxAttempts: 3,
		VerifyTokenCaller:    testCaller,
	}, service.WithClock(h.clock.Now), service.WithLogger(debugLogger(h.logs)))
	return h
}

func testIssuer(t *testing.T, now func() time.Time) *token.Issuer {
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
	return issuer
}

// anon is an unauthenticated call (SignIn, RefreshToken, phone flows).
func anon() auth.Principal { return auth.Principal{RequestID: uuid.NewString()} }

// user is a call the edge decorated after VerifyToken.
func user(res service.SignInResult) auth.Principal {
	return auth.Principal{UserID: res.UserID, SessionID: res.SessionID, RequestID: uuid.NewString()}
}

func internal(caller string) auth.Principal {
	return auth.Principal{Caller: caller, RequestID: uuid.NewString()}
}

// requestCode asks for a code and returns the challenge and the code the
// (fake) SMS sender received.
func (h *harness) requestCode(phone string, purpose authgatewayv1.PhoneCodePurpose) (service.Challenge, string) {
	h.t.Helper()
	ch, err := h.svc.RequestPhoneCode(ctx, anon(), phone, purpose)
	if err != nil {
		h.t.Fatalf("RequestPhoneCode: %v", err)
	}
	sent := h.sender.last(h.t)
	if sent.phone != phone || sent.purpose != purpose {
		h.t.Fatalf("code sent to %+v, want %s/%s", sent, phone, purpose)
	}
	return ch, sent.code
}

// signUp runs the whole phone sign-up.
func (h *harness) signUp(phone, pw string) service.SignInResult {
	h.t.Helper()
	ch, code := h.requestCode(phone, domain.PurposeSignUp)
	res, err := h.svc.SignUpWithPhone(ctx, anon(), uuid.NewString(), service.PhoneSetup{
		Phone: phone, ChallengeID: ch.ID.String(), Code: code, Password: pw,
	}, domain.ClientInfo{Platform: authgatewayv1.ClientPlatform_CLIENT_PLATFORM_ANDROID, DeviceName: "Pixel 8"})
	if err != nil {
		h.t.Fatalf("SignUpWithPhone: %v", err)
	}
	return res
}

// assertError checks the gRPC code and the common.v1.Error reason.
func assertError(t *testing.T, err error, code codes.Code, reason commonv1.ErrorReason) *apperr.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s / %s, got no error", code, reason)
	}
	ae := apperr.From(err)
	if ae.Code != code || ae.Reason != reason {
		t.Fatalf("error = %s / %s (%v), want %s / %s", ae.Code, ae.Reason, err, code, reason)
	}
	return ae
}

func TestSignInWithGoogleCreatesTheAccountOnce(t *testing.T) {
	h := newHarness(t)
	in := service.SignInInput{
		Method: domain.MethodGoogle,
		Token:  "dev-google:104711920733:alice@gmail.com",
		Client: domain.ClientInfo{Platform: authgatewayv1.ClientPlatform_CLIENT_PLATFORM_WEB},
	}

	first, err := h.svc.SignIn(ctx, anon(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !first.IsNewAccount || first.UserID == uuid.Nil || first.Tokens.AccessToken == "" ||
		first.Tokens.RefreshToken == "" {
		t.Fatalf("first sign-in = %+v", first)
	}
	if !first.Tokens.AccessTokenExpiresAt.Equal(start.Add(15*time.Minute)) ||
		!first.Tokens.RefreshTokenExpiresAt.Equal(start.Add(30*24*time.Hour)) {
		t.Fatalf("token expiries = %+v", first.Tokens)
	}

	second, err := h.svc.SignIn(ctx, anon(), in)
	if err != nil {
		t.Fatal(err)
	}
	if second.IsNewAccount || second.UserID != first.UserID {
		t.Fatalf("second sign-in = %+v, want the same account and is_new_account=false", second)
	}
	if second.SessionID == first.SessionID {
		t.Fatal("a second sign-in must open a new session")
	}
	if h.accounts.creates != 1 {
		t.Fatalf("identity CreateAccount called %d times, want 1", h.accounts.creates)
	}

	// The credential carries a masked hint, never the raw e-mail.
	creds, err := h.svc.ListSignInMethods(ctx, user(first))
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 || creds[0].DisplayHint != "a***@gmail.com" || creds[0].Method != domain.MethodGoogle {
		t.Fatalf("methods = %+v", creds)
	}
}

// A retried first sign-in must reuse identity's idempotency key, even when the
// credential row was lost in between (identity is the one deciding).
func TestSignInDerivesADeterministicRequestID(t *testing.T) {
	h := newHarness(t)
	in := service.SignInInput{Method: domain.MethodApple, Token: "dev-apple:000123.abc"}
	first, err := h.svc.SignIn(ctx, anon(), in)
	if err != nil {
		t.Fatal(err)
	}
	// Drop the credential: the next sign-in looks like a first one again.
	h.store.mu.Lock()
	h.store.db.creds = map[uuid.UUID]domain.Credential{}
	h.store.mu.Unlock()

	again, err := h.svc.SignIn(ctx, anon(), in)
	if err != nil {
		t.Fatal(err)
	}
	if again.UserID != first.UserID {
		t.Fatalf("second account %s, want the first one %s (same CreateAccount request_id)", again.UserID, first.UserID)
	}
	if h.accounts.creates != 2 || len(h.accounts.byRequest) != 1 {
		t.Fatalf("CreateAccount calls=%d distinct request ids=%d, want 2 and 1",
			h.accounts.creates, len(h.accounts.byRequest))
	}
}

func TestSignInWithProviderRejectsBadTokens(t *testing.T) {
	h := newHarness(t)
	for name, tok := range map[string]string{
		"a real Google token": "eyJhbGciOiJSUzI1NiIsImtpZCI6IjEyMyJ9.e30.sig",
		"empty subject":       "dev-google:",
		"wrong provider":      "dev-apple:123",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.svc.SignIn(ctx, anon(), service.SignInInput{Method: domain.MethodGoogle, Token: tok})
			assertError(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_INVALID_CREDENTIALS)
		})
	}
}

func TestSignInWhenIdentityIsUnavailable(t *testing.T) {
	h := newHarness(t)
	h.accounts.unavailable()
	_, err := h.svc.SignIn(ctx, anon(), service.SignInInput{
		Method: domain.MethodGoogle, Token: "dev-google:104711920733",
	})
	assertError(t, err, codes.Unavailable, commonv1.ErrorReason_ERROR_REASON_DEPENDENCY_UNAVAILABLE)
	if n := len(h.store.snapshot().sessions); n != 0 {
		t.Fatalf("%d sessions were opened although identity was down", n)
	}
}

func TestSignInBlockedAccount(t *testing.T) {
	h := newHarness(t)
	in := service.SignInInput{Method: domain.MethodGoogle, Token: "dev-google:104711920733"}
	first, err := h.svc.SignIn(ctx, anon(), in)
	if err != nil {
		t.Fatal(err)
	}
	h.accounts.setStatus(first.UserID, eventsv1.AccountStatus_ACCOUNT_STATUS_BLOCKED)

	_, err = h.svc.SignIn(ctx, anon(), in)
	assertError(t, err, codes.PermissionDenied, commonv1.ErrorReason_ERROR_REASON_ACCOUNT_DISABLED)

	// A blocked account cannot refresh either.
	_, err = h.svc.RefreshToken(ctx, anon(), first.Tokens.RefreshToken)
	assertError(t, err, codes.PermissionDenied, commonv1.ErrorReason_ERROR_REASON_ACCOUNT_DISABLED)
}

func TestSignInWithPassword(t *testing.T) {
	h := newHarness(t)
	signedUp := h.signUp(testPhone, testPassword)

	res, err := h.svc.SignIn(ctx, anon(), service.SignInInput{
		Method: domain.MethodPhonePassword, Phone: testPhone, Password: testPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.UserID != signedUp.UserID || res.IsNewAccount {
		t.Fatalf("sign-in = %+v", res)
	}

	// Wrong password and unknown number answer the same thing, and both count
	// against the per-number limit.
	_, err = h.svc.SignIn(ctx, anon(), service.SignInInput{
		Method: domain.MethodPhonePassword, Phone: testPhone, Password: "wrong",
	})
	assertError(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_INVALID_CREDENTIALS)
	_, err = h.svc.SignIn(ctx, anon(), service.SignInInput{
		Method: domain.MethodPhonePassword, Phone: "+998900000000", Password: testPassword,
	})
	assertError(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_INVALID_CREDENTIALS)
	if got := h.limiter.count("sign-in:PHONE_PASSWORD:" + testPhone); got != 1 {
		t.Fatalf("failed sign-ins counted = %d, want 1 (successful ones are not counted)", got)
	}
}

func TestSignInRateLimited(t *testing.T) {
	h := newHarness(t) // limit 3
	h.signUp(testPhone, testPassword)
	bad := service.SignInInput{Method: domain.MethodPhonePassword, Phone: testPhone, Password: "wrong"}
	for i := range 4 {
		_, err := h.svc.SignIn(ctx, anon(), bad)
		assertError(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_INVALID_CREDENTIALS)
		_ = i
	}
	// The fifth attempt is refused before the password is even checked, and
	// the right password no longer helps until the window resets.
	_, err := h.svc.SignIn(ctx, anon(), service.SignInInput{
		Method: domain.MethodPhonePassword, Phone: testPhone, Password: testPassword,
	})
	ae := assertError(t, err, codes.ResourceExhausted, commonv1.ErrorReason_ERROR_REASON_RATE_LIMITED)
	if ae.Metadata["retry_after_seconds"] != "42" {
		t.Fatalf("metadata = %v, want retry_after_seconds", ae.Metadata)
	}
}

// A broken Redis must never stop a sign-in (fail open).
func TestRateLimiterFailsOpen(t *testing.T) {
	h := newHarness(t)
	res := h.signUp(testPhone, testPassword)
	h.limiter.fail = context.DeadlineExceeded
	if _, err := h.svc.SignIn(ctx, anon(), service.SignInInput{
		Method: domain.MethodPhonePassword, Phone: testPhone, Password: testPassword,
	}); err != nil {
		t.Fatalf("sign-in must succeed when the limiter is down: %v", err)
	}
	if _, err := h.svc.RequestPhoneCode(ctx, anon(), "+998901111111", domain.PurposeSignUp); err != nil {
		t.Fatalf("RequestPhoneCode must succeed when the limiter is down: %v", err)
	}
	_ = res
}

func TestPhoneSignUpEndToEnd(t *testing.T) {
	h := newHarness(t)
	res := h.signUp(testPhone, testPassword)
	if res.UserID == uuid.Nil || !res.IsNewAccount || res.Tokens.AccessToken == "" {
		t.Fatalf("sign-up = %+v", res)
	}

	// The access token verifies and carries the live roles.
	h.accounts.setRoles(res.UserID, commonv1.Role_ROLE_ADMIN)
	refreshed, err := h.svc.RefreshToken(ctx, anon(), res.Tokens.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	v, err := h.svc.VerifyToken(ctx, internal(testCaller), refreshed.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if v.UserID != res.UserID || v.SessionID != res.SessionID ||
		len(v.Roles) != 1 || v.Roles[0] != commonv1.Role_ROLE_ADMIN {
		t.Fatalf("verified = %+v", v)
	}

	// The challenge is spent and the credential stores a hash, never the
	// password.
	db := h.store.snapshot()
	for _, c := range db.challenges {
		if c.ConsumedAt == nil {
			t.Fatal("the challenge was not consumed")
		}
	}
	for _, c := range db.creds {
		if !strings.HasPrefix(c.PasswordHash, "$argon2id$") || strings.Contains(c.PasswordHash, testPassword) {
			t.Fatalf("stored password hash = %q", c.PasswordHash)
		}
		if c.DisplayHint != "+998 90 *** ** 67" {
			t.Fatalf("display hint = %q", c.DisplayHint)
		}
	}
}

func TestRequestPhoneCodePurposeRules(t *testing.T) {
	h := newHarness(t)
	h.signUp(testPhone, testPassword)

	// Sign-up and link need a free number.
	for _, purpose := range []authgatewayv1.PhoneCodePurpose{domain.PurposeSignUp, domain.PurposeLinkPhone} {
		_, err := h.svc.RequestPhoneCode(ctx, anon(), testPhone, purpose)
		assertError(t, err, codes.AlreadyExists, commonv1.ErrorReason_ERROR_REASON_PHONE_ALREADY_REGISTERED)
	}
	// Reset needs a registered one.
	_, err := h.svc.RequestPhoneCode(ctx, anon(), "+998900000000", domain.PurposeResetPassword)
	assertError(t, err, codes.NotFound, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)
	// An unspecified purpose is a validation error.
	_, err = h.svc.RequestPhoneCode(ctx, anon(), "+998900000000",
		authgatewayv1.PhoneCodePurpose_PHONE_CODE_PURPOSE_UNSPECIFIED)
	assertError(t, err, codes.InvalidArgument, commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED)
}

func TestRequestPhoneCodeResendWindow(t *testing.T) {
	h := newHarness(t)
	first, _ := h.requestCode("+998901111111", domain.PurposeSignUp)
	if !first.ResendAvailableAt.Equal(start.Add(time.Minute)) || !first.ExpiresAt.Equal(start.Add(10*time.Minute)) {
		t.Fatalf("challenge = %+v", first)
	}

	_, err := h.svc.RequestPhoneCode(ctx, anon(), "+998901111111", domain.PurposeSignUp)
	ae := assertError(t, err, codes.ResourceExhausted, commonv1.ErrorReason_ERROR_REASON_RATE_LIMITED)
	if ae.Metadata["retry_after_seconds"] != "60" {
		t.Fatalf("metadata = %v, want retry_after_seconds 60", ae.Metadata)
	}

	// After the window another code may be asked for, and it is a new one.
	h.clock.advance(time.Minute)
	second, code := h.requestCode("+998901111111", domain.PurposeSignUp)
	if second.ID == first.ID || code == "" {
		t.Fatalf("second challenge = %+v", second)
	}
}

func TestPhoneChallengeLifecycle(t *testing.T) {
	newSetup := func(h *harness, ch service.Challenge, code string) service.PhoneSetup {
		return service.PhoneSetup{
			Phone: testPhone, ChallengeID: ch.ID.String(), Code: code, Password: testPassword,
		}
	}

	t.Run("wrong code counts an attempt and locks out", func(t *testing.T) {
		h := newHarness(t) // 3 attempts
		ch, code := h.requestCode(testPhone, domain.PurposeSignUp)
		for range 3 {
			_, err := h.svc.SignUpWithPhone(ctx, anon(), uuid.NewString(),
				newSetup(h, ch, "000000"), domain.ClientInfo{})
			assertError(t, err, codes.InvalidArgument, commonv1.ErrorReason_ERROR_REASON_OTP_INVALID)
		}
		if got := h.store.snapshot().challenges[ch.ID].Attempts; got != 3 {
			t.Fatalf("attempts = %d, want 3", got)
		}
		// Out of attempts: even the right code is refused now.
		_, err := h.svc.SignUpWithPhone(ctx, anon(), uuid.NewString(),
			newSetup(h, ch, code), domain.ClientInfo{})
		assertError(t, err, codes.InvalidArgument, commonv1.ErrorReason_ERROR_REASON_OTP_INVALID)
	})

	t.Run("expired", func(t *testing.T) {
		h := newHarness(t)
		ch, code := h.requestCode(testPhone, domain.PurposeSignUp)
		h.clock.advance(11 * time.Minute)
		_, err := h.svc.SignUpWithPhone(ctx, anon(), uuid.NewString(),
			newSetup(h, ch, code), domain.ClientInfo{})
		assertError(t, err, codes.InvalidArgument, commonv1.ErrorReason_ERROR_REASON_OTP_INVALID)
	})

	t.Run("wrong purpose", func(t *testing.T) {
		h := newHarness(t)
		h.signUp("+998901111111", testPassword)
		ch, code := h.requestCode("+998901111111", domain.PurposeResetPassword)
		// A reset code must not work as a sign-up code.
		_, err := h.svc.SignUpWithPhone(ctx, anon(), uuid.NewString(), service.PhoneSetup{
			Phone: "+998901111111", ChallengeID: ch.ID.String(), Code: code, Password: testPassword,
		}, domain.ClientInfo{})
		assertError(t, err, codes.InvalidArgument, commonv1.ErrorReason_ERROR_REASON_OTP_INVALID)
	})

	t.Run("wrong number", func(t *testing.T) {
		h := newHarness(t)
		ch, code := h.requestCode(testPhone, domain.PurposeSignUp)
		_, err := h.svc.SignUpWithPhone(ctx, anon(), uuid.NewString(), service.PhoneSetup{
			Phone: "+998907654321", ChallengeID: ch.ID.String(), Code: code, Password: testPassword,
		}, domain.ClientInfo{})
		assertError(t, err, codes.InvalidArgument, commonv1.ErrorReason_ERROR_REASON_OTP_INVALID)
	})

	t.Run("unknown or malformed challenge", func(t *testing.T) {
		h := newHarness(t)
		_, err := h.svc.SignUpWithPhone(ctx, anon(), uuid.NewString(), service.PhoneSetup{
			Phone: testPhone, ChallengeID: uuid.NewString(), Code: "123456", Password: testPassword,
		}, domain.ClientInfo{})
		assertError(t, err, codes.InvalidArgument, commonv1.ErrorReason_ERROR_REASON_OTP_INVALID)

		_, err = h.svc.SignUpWithPhone(ctx, anon(), uuid.NewString(), service.PhoneSetup{
			Phone: testPhone, ChallengeID: "not-a-uuid", Code: "123456", Password: testPassword,
		}, domain.ClientInfo{})
		assertError(t, err, codes.InvalidArgument, commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED)
	})

	t.Run("used once", func(t *testing.T) {
		h := newHarness(t)
		ch, code := h.requestCode(testPhone, domain.PurposeSignUp)
		if _, err := h.svc.SignUpWithPhone(ctx, anon(), uuid.NewString(),
			newSetup(h, ch, code), domain.ClientInfo{}); err != nil {
			t.Fatal(err)
		}
		_, err := h.svc.SignUpWithPhone(ctx, anon(), uuid.NewString(),
			newSetup(h, ch, code), domain.ClientInfo{})
		// The number is taken now, and the code is spent either way.
		if ae := apperr.From(err); ae.Reason != commonv1.ErrorReason_ERROR_REASON_PHONE_ALREADY_REGISTERED &&
			ae.Reason != commonv1.ErrorReason_ERROR_REASON_OTP_INVALID {
			t.Fatalf("second use = %v", err)
		}
	})
}

func TestResetPassword(t *testing.T) {
	h := newHarness(t)
	res := h.signUp(testPhone, testPassword)
	other, err := h.svc.SignIn(ctx, anon(), service.SignInInput{
		Method: domain.MethodPhonePassword, Phone: testPhone, Password: testPassword,
	})
	if err != nil {
		t.Fatal(err)
	}

	ch, code := h.requestCode(testPhone, domain.PurposeResetPassword)
	const newPassword = "yangi-parol-2026"
	if err := h.svc.ResetPassword(ctx, anon(), service.PhoneSetup{
		Phone: testPhone, ChallengeID: ch.ID.String(), Code: code, Password: newPassword,
	}); err != nil {
		t.Fatal(err)
	}

	// Every session ended.
	db := h.store.snapshot()
	for _, s := range db.sessions {
		if s.RevokedAt == nil || s.RevokedReason != domain.RevokedPasswordReset {
			t.Fatalf("session %s = %+v, want revoked for the password reset", s.ID, s)
		}
	}
	if _, err := h.svc.RefreshToken(ctx, anon(), res.Tokens.RefreshToken); err == nil {
		t.Fatal("the old refresh token must stop working")
	}
	_, err = h.svc.VerifyToken(ctx, internal(testCaller), other.Tokens.AccessToken)
	assertError(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_TOKEN_REVOKED)

	// Only the new password works.
	_, err = h.svc.SignIn(ctx, anon(), service.SignInInput{
		Method: domain.MethodPhonePassword, Phone: testPhone, Password: testPassword,
	})
	assertError(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_INVALID_CREDENTIALS)
	if _, err := h.svc.SignIn(ctx, anon(), service.SignInInput{
		Method: domain.MethodPhonePassword, Phone: testPhone, Password: newPassword,
	}); err != nil {
		t.Fatalf("sign-in with the new password: %v", err)
	}

	// A SECURITY notification was written to the outbox, with no secrets.
	db = h.store.snapshot()
	if len(db.outbox) != 1 {
		t.Fatalf("outbox has %d messages, want 1", len(db.outbox))
	}
	msg := db.outbox[0]
	if msg.Topic != "agrogram.notification.request.v1" || msg.Key != res.UserID.String() {
		t.Fatalf("outbox message = %+v", msg)
	}
	for _, secret := range []string{testPassword, newPassword, code, testPhone, res.Tokens.RefreshToken} {
		if strings.Contains(string(msg.Value), secret) {
			t.Fatalf("the event payload contains a secret (%q)", secret)
		}
	}
}

func TestRefreshRotationAndReuse(t *testing.T) {
	h := newHarness(t)
	res := h.signUp(testPhone, testPassword)

	first, err := h.svc.RefreshToken(ctx, anon(), res.Tokens.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if first.RefreshToken == res.Tokens.RefreshToken {
		t.Fatal("the refresh token must rotate")
	}
	second, err := h.svc.RefreshToken(ctx, anon(), first.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}

	// Reusing the first (already rotated) token revokes the whole session.
	_, err = h.svc.RefreshToken(ctx, anon(), first.RefreshToken)
	assertError(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_TOKEN_REVOKED)
	db := h.store.snapshot()
	session := db.sessions[res.SessionID]
	if session.RevokedAt == nil || session.RevokedReason != domain.RevokedTokenReuse {
		t.Fatalf("session = %+v, want revoked for token reuse", session)
	}
	// The newest token is dead too, and so is the access token.
	_, err = h.svc.RefreshToken(ctx, anon(), second.RefreshToken)
	assertError(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_TOKEN_REVOKED)
	_, err = h.svc.VerifyToken(ctx, internal(testCaller), second.AccessToken)
	assertError(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_TOKEN_REVOKED)
}

func TestRefreshRejects(t *testing.T) {
	h := newHarness(t)
	res := h.signUp(testPhone, testPassword)

	t.Run("unknown token", func(t *testing.T) {
		_, err := h.svc.RefreshToken(ctx, anon(), "not-a-refresh-token")
		assertError(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_TOKEN_INVALID)
	})
	t.Run("expired token", func(t *testing.T) {
		h.clock.advance(31 * 24 * time.Hour)
		defer h.clock.advance(-31 * 24 * time.Hour)
		_, err := h.svc.RefreshToken(ctx, anon(), res.Tokens.RefreshToken)
		assertError(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_TOKEN_EXPIRED)
	})
}

func TestSignOutScopes(t *testing.T) {
	h := newHarness(t)
	first := h.signUp(testPhone, testPassword)
	second, err := h.svc.SignIn(ctx, anon(), service.SignInInput{
		Method: domain.MethodPhonePassword, Phone: testPhone, Password: testPassword,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The current session only.
	if err := h.svc.SignOut(ctx, user(first), domain.ScopeCurrentSession); err != nil {
		t.Fatal(err)
	}
	_, err = h.svc.RefreshToken(ctx, anon(), first.Tokens.RefreshToken)
	assertError(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_TOKEN_REVOKED)
	if _, err := h.svc.RefreshToken(ctx, anon(), second.Tokens.RefreshToken); err != nil {
		t.Fatalf("the other session must survive: %v", err)
	}

	// Every session.
	if err := h.svc.SignOut(ctx, user(second), domain.ScopeAllSessions); err != nil {
		t.Fatal(err)
	}
	for _, s := range h.store.snapshot().sessions {
		if s.RevokedAt == nil {
			t.Fatalf("session %s is still live", s.ID)
		}
	}

	// Signing out needs a caller, and the current-session scope needs a session.
	assertError(t, h.svc.SignOut(ctx, anon(), domain.ScopeCurrentSession),
		codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED)
	assertError(t, h.svc.SignOut(ctx, auth.Principal{UserID: first.UserID, RequestID: "r"}, domain.ScopeCurrentSession),
		codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED)

	// One user can never sign another one out.
	stranger := auth.Principal{UserID: uuid.New(), SessionID: second.SessionID, RequestID: "r"}
	if err := h.svc.SignOut(ctx, stranger, domain.ScopeCurrentSession); err != nil {
		t.Fatalf("SignOut = %v, want a silent no-op", err)
	}
}

func TestVerifyToken(t *testing.T) {
	h := newHarness(t)
	res := h.signUp(testPhone, testPassword)

	t.Run("internal callers only", func(t *testing.T) {
		_, err := h.svc.VerifyToken(ctx, anon(), res.Tokens.AccessToken)
		assertError(t, err, codes.PermissionDenied, commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED)
		_, err = h.svc.VerifyToken(ctx, internal("catalog"), res.Tokens.AccessToken)
		assertError(t, err, codes.PermissionDenied, commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED)
	})
	t.Run("garbage", func(t *testing.T) {
		_, err := h.svc.VerifyToken(ctx, internal(testCaller), "not.a.token")
		assertError(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_TOKEN_INVALID)
	})
	t.Run("expired", func(t *testing.T) {
		h.clock.advance(16 * time.Minute)
		defer h.clock.advance(-16 * time.Minute)
		_, err := h.svc.VerifyToken(ctx, internal(testCaller), res.Tokens.AccessToken)
		assertError(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_TOKEN_EXPIRED)
	})
	t.Run("signed by a stranger", func(t *testing.T) {
		other := testIssuer(t, h.clock.Now)
		foreign, _, err := other.Issue(token.Claims{UserID: res.UserID, SessionID: res.SessionID})
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.svc.VerifyToken(ctx, internal(testCaller), foreign)
		assertError(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_TOKEN_INVALID)
	})
	t.Run("session gone", func(t *testing.T) {
		raw, _, err := h.issuer.Issue(token.Claims{UserID: res.UserID, SessionID: uuid.New()})
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.svc.VerifyToken(ctx, internal(testCaller), raw)
		assertError(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_TOKEN_INVALID)
	})
	t.Run("another user's session", func(t *testing.T) {
		raw, _, err := h.issuer.Issue(token.Claims{UserID: uuid.New(), SessionID: res.SessionID})
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.svc.VerifyToken(ctx, internal(testCaller), raw)
		assertError(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_TOKEN_INVALID)
	})
}

func TestLinkAndUnlinkSignInMethods(t *testing.T) {
	h := newHarness(t)
	res := h.signUp(testPhone, testPassword)
	caller := user(res)

	// Link Google.
	creds, err := h.svc.LinkSignInMethod(ctx, caller, service.LinkInput{
		Method: domain.MethodGoogle, Token: "dev-google:104711920733:alice@gmail.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 2 {
		t.Fatalf("methods = %+v", creds)
	}
	// Signing in with the linked Google account reaches the same user.
	google, err := h.svc.SignIn(ctx, anon(), service.SignInInput{
		Method: domain.MethodGoogle, Token: "dev-google:104711920733:alice@gmail.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if google.UserID != res.UserID || google.IsNewAccount {
		t.Fatalf("sign-in after linking = %+v", google)
	}

	// The same method twice, and somebody else's Google account.
	_, err = h.svc.LinkSignInMethod(ctx, caller, service.LinkInput{
		Method: domain.MethodGoogle, Token: "dev-google:999:bob@gmail.com",
	})
	assertError(t, err, codes.AlreadyExists, commonv1.ErrorReason_ERROR_REASON_PROVIDER_ALREADY_LINKED)

	otherUser, err := h.svc.SignIn(ctx, anon(), service.SignInInput{
		Method: domain.MethodApple, Token: "dev-apple:000999.zzz",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.svc.LinkSignInMethod(ctx, user(otherUser), service.LinkInput{
		Method: domain.MethodGoogle, Token: "dev-google:104711920733:alice@gmail.com",
	})
	assertError(t, err, codes.AlreadyExists, commonv1.ErrorReason_ERROR_REASON_PROVIDER_ALREADY_LINKED)

	// A phone number that belongs to another account.
	ch, code := h.requestCode("+998907654321", domain.PurposeLinkPhone)
	h.store.mu.Lock()
	for id, c := range h.store.db.creds { // pretend the number got taken meanwhile
		if c.Method == domain.MethodPhonePassword {
			c.Subject = "+998907654321"
			h.store.db.creds[id] = c
		}
	}
	h.store.mu.Unlock()
	_, err = h.svc.LinkSignInMethod(ctx, user(otherUser), service.LinkInput{
		Method: domain.MethodPhonePassword,
		Phone: service.PhoneSetup{
			Phone: "+998907654321", ChallengeID: ch.ID.String(), Code: code, Password: testPassword,
		},
	})
	assertError(t, err, codes.AlreadyExists, commonv1.ErrorReason_ERROR_REASON_PHONE_ALREADY_REGISTERED)

	// Unlink: the last method may not go.
	left, err := h.svc.UnlinkSignInMethod(ctx, caller, domain.MethodGoogle)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].Method != domain.MethodPhonePassword {
		t.Fatalf("remaining methods = %+v", left)
	}
	_, err = h.svc.UnlinkSignInMethod(ctx, caller, domain.MethodPhonePassword)
	assertError(t, err, codes.FailedPrecondition, commonv1.ErrorReason_ERROR_REASON_LAST_SIGN_IN_METHOD)
	_, err = h.svc.UnlinkSignInMethod(ctx, caller, domain.MethodApple)
	assertError(t, err, codes.NotFound, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)

	// Linking and unlinking are worth a SECURITY notification each.
	var templates []string
	for _, m := range h.store.snapshot().outbox {
		templates = append(templates, m.EventType)
	}
	if len(templates) != 2 {
		t.Fatalf("outbox has %d messages, want 2 (linked, unlinked)", len(templates))
	}

	// Authentication is required.
	_, err = h.svc.ListSignInMethods(ctx, anon())
	assertError(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED)
	_, err = h.svc.LinkSignInMethod(ctx, anon(), service.LinkInput{Method: domain.MethodGoogle, Token: "dev-google:1"})
	assertError(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED)
}

// Nothing secret may reach the log, at any level.
func TestNoSecretIsLogged(t *testing.T) {
	h := newHarness(t)
	res := h.signUp(testPhone, testPassword)
	code := h.sender.last(t).code
	pair, err := h.svc.RefreshToken(ctx, anon(), res.Tokens.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SignIn(ctx, anon(), service.SignInInput{
		Method: domain.MethodPhonePassword, Phone: testPhone, Password: "wrong-password-attempt",
	}); err == nil {
		t.Fatal("want a failed sign-in")
	}
	if _, err := h.svc.VerifyToken(ctx, internal(testCaller), pair.AccessToken); err != nil {
		t.Fatal(err)
	}

	logs := h.logs.String()
	if logs == "" {
		t.Fatal("no logs were produced, so this test proves nothing")
	}
	for name, secret := range map[string]string{
		"the password":          testPassword,
		"a failed password":     "wrong-password-attempt",
		"the SMS code":          code,
		"the access token":      pair.AccessToken,
		"the refresh token":     pair.RefreshToken,
		"the old refresh token": res.Tokens.RefreshToken,
	} {
		if strings.Contains(logs, secret) {
			t.Fatalf("%s appears in the log", name)
		}
	}
	// The password hash must not be logged either.
	for _, c := range h.store.snapshot().creds {
		if c.PasswordHash != "" && strings.Contains(logs, c.PasswordHash) {
			t.Fatal("a password hash appears in the log")
		}
	}
}

func TestJWKSIsServed(t *testing.T) {
	h := newHarness(t)
	keys := h.svc.JWKS()
	if len(keys) != 1 || keys[0].Kty != "EC" || keys[0].Alg != "ES256" || keys[0].Kid == "" {
		t.Fatalf("JWKS = %+v", keys)
	}
}
