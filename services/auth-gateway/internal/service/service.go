// Package service is auth-gateway's business logic: the three sign-in paths,
// phone code challenges, sign-in method linking, sessions and the tokens that
// back them.
//
// What this service owns: credentials (argon2id password hashes, Google and
// Apple links), phone SMS challenges and refresh-token sessions. Accounts and
// roles belong to identity, which is called when an account is created and
// every time a token is issued or refreshed, so an access token always
// carries live roles.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"
	authgatewayv1 "github.com/tbilol/dxrl/gen/go/authgateway/v1"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/auth-gateway/internal/apperr"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/auth"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/domain"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/events"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/identityclient"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/password"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/provider"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/sms"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/token"
)

// Queries is the repository. Methods return domain.ErrNotFound for missing
// rows. Callers pass every timestamp, so the service clock drives all state.
type Queries interface {
	// CredentialBySubject finds the credential a sign-in identity belongs to.
	CredentialBySubject(ctx context.Context, method authgatewayv1.SignInMethod, subject string) (domain.Credential, error)
	// CredentialsByUser lists an account's credentials, oldest link first.
	CredentialsByUser(ctx context.Context, userID uuid.UUID) ([]domain.Credential, error)
	// InsertCredential stores a credential. It returns domain.ErrMethodLinked
	// when the account already has that method and domain.ErrSubjectTaken when
	// the subject (or phone number) belongs to another account.
	InsertCredential(ctx context.Context, c domain.Credential) error
	// UpdatePasswordHash replaces a credential's argon2id PHC string.
	UpdatePasswordHash(ctx context.Context, credentialID uuid.UUID, hash string) error
	// TouchCredential records a successful sign-in with this credential.
	TouchCredential(ctx context.Context, credentialID uuid.UUID, now time.Time) error
	// DeleteCredential removes a method from an account; false if it had none.
	DeleteCredential(ctx context.Context, userID uuid.UUID, method authgatewayv1.SignInMethod) (bool, error)

	InsertSession(ctx context.Context, s domain.Session) error
	GetSession(ctx context.Context, id uuid.UUID) (domain.Session, error)
	// TouchSession records activity on a session (a refresh).
	TouchSession(ctx context.Context, id uuid.UUID, now time.Time) error
	// RevokeSession ends one of the user's live sessions; false if it was not
	// theirs or was already revoked.
	RevokeSession(ctx context.Context, id, userID uuid.UUID, reason string, now time.Time) (bool, error)
	// RevokeUserSessions ends every live session of an account and returns how
	// many it ended.
	RevokeUserSessions(ctx context.Context, userID uuid.UUID, reason string, now time.Time) (int64, error)

	InsertRefreshToken(ctx context.Context, rt domain.RefreshToken) error
	// RefreshTokenByHash finds an issued refresh token by its SHA-256 digest.
	RefreshTokenByHash(ctx context.Context, hash []byte) (domain.RefreshToken, error)
	// UseRefreshToken marks a token used exactly once (guarded update): false
	// means another refresh won the race, which is token reuse.
	UseRefreshToken(ctx context.Context, id, replacedBy uuid.UUID, now time.Time) (bool, error)

	InsertChallenge(ctx context.Context, c domain.PhoneChallenge) error
	GetChallenge(ctx context.Context, id uuid.UUID) (domain.PhoneChallenge, error)
	// LatestChallenge returns the newest challenge for a number and purpose
	// (the resend window).
	LatestChallenge(ctx context.Context, phone string, purpose authgatewayv1.PhoneCodePurpose) (domain.PhoneChallenge, error)
	// CountChallengeAttempt records one wrong code.
	CountChallengeAttempt(ctx context.Context, id uuid.UUID) error
	// ConsumeChallenge spends an unconsumed, unexpired challenge; false means
	// somebody else spent it first.
	ConsumeChallenge(ctx context.Context, id uuid.UUID, now time.Time) (bool, error)

	EnqueueOutbox(ctx context.Context, m events.Message) error
}

// Store runs Queries without (Run) or within (InTx) a transaction. InTx
// commits when fn returns nil and rolls back otherwise.
type Store interface {
	Run(ctx context.Context, fn func(Queries) error) error
	InTx(ctx context.Context, fn func(Queries) error) error
}

// Accounts is the part of identity auth-gateway needs (*identityclient.Client).
type Accounts interface {
	CreateAccount(ctx context.Context, p auth.Principal, requestID uuid.UUID) (identityclient.Account, error)
	GetAccount(ctx context.Context, p auth.Principal, userID uuid.UUID) (identityclient.Account, error)
}

// RateLimiter counts hits per key in a fixed window. Exceeded asks without
// counting, so only failed sign-ins land in the counter.
type RateLimiter interface {
	Allow(ctx context.Context, key string) (allowed bool, retryAfter time.Duration, err error)
	Exceeded(ctx context.Context, key string) (exceeded bool, retryAfter time.Duration, err error)
}

// accountNamespace derives the CreateAccount idempotency key from the sign-in
// identity (UUIDv5), so a retried first Google or Apple sign-in - or two
// parallel ones - can never create two accounts. Changing this value would
// orphan every account created before the change.
var accountNamespace = uuid.MustParse("6f9619ff-8b86-d011-b42d-00c04fc964ff")

// Config tunes the flows.
type Config struct {
	RefreshTokenTTL time.Duration
	// ProviderTimeout bounds the Google/Apple verification call.
	ProviderTimeout time.Duration

	PhoneCodeTTL         time.Duration
	PhoneCodeResend      time.Duration
	PhoneCodeLength      int
	PhoneCodeMaxAttempts int

	// STUB: DevPhoneCode is a fixed SMS code for local development, because
	// nothing is ever sent. Empty everywhere else, and then codes are random.
	DevPhoneCode string

	// VerifyTokenCaller is the only x-agrogram-caller allowed to call
	// VerifyToken; empty disables the check.
	VerifyTokenCaller string
}

// Service implements auth-gateway's use cases.
type Service struct {
	store    Store
	accounts Accounts
	issuer   *token.Issuer
	hasher   *password.Hasher
	google   provider.Verifier
	apple    provider.Verifier
	sender   sms.Sender
	limiter  RateLimiter // nil: rate limiting disabled
	cfg      Config
	now      func() time.Time
	log      *slog.Logger
}

// Deps are the collaborators of a Service; all but limiter are required.
type Deps struct {
	Store    Store
	Accounts Accounts
	Issuer   *token.Issuer
	Hasher   *password.Hasher
	Google   provider.Verifier
	Apple    provider.Verifier
	Sender   sms.Sender
	Limiter  RateLimiter
}

// Option customizes a Service.
type Option func(*Service)

// WithClock replaces time.Now (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(s *Service) { s.log = l } }

// New returns a Service. Deps.Limiter may be nil (no rate limiting).
func New(d Deps, cfg Config, opts ...Option) *Service {
	s := &Service{
		store:    d.Store,
		accounts: d.Accounts,
		issuer:   d.Issuer,
		hasher:   d.Hasher,
		google:   d.Google,
		apple:    d.Apple,
		sender:   d.Sender,
		limiter:  d.Limiter,
		cfg:      cfg,
		now:      time.Now,
		log:      slog.New(slog.DiscardHandler),
	}
	for _, o := range opts {
		o(s)
	}
	if s.cfg.RefreshTokenTTL <= 0 {
		s.cfg.RefreshTokenTTL = 30 * 24 * time.Hour
	}
	if s.cfg.ProviderTimeout <= 0 {
		s.cfg.ProviderTimeout = 5 * time.Second
	}
	if s.cfg.PhoneCodeTTL <= 0 {
		s.cfg.PhoneCodeTTL = 10 * time.Minute
	}
	if s.cfg.PhoneCodeResend <= 0 {
		s.cfg.PhoneCodeResend = time.Minute
	}
	if s.cfg.PhoneCodeLength == 0 {
		s.cfg.PhoneCodeLength = 6
	}
	if s.cfg.PhoneCodeMaxAttempts <= 0 {
		s.cfg.PhoneCodeMaxAttempts = 5
	}
	return s
}

// JWKS returns the public signing keys (GetJwks).
func (s *Service) JWKS() []token.JSONWebKey { return s.issuer.JWKS() }

// clock returns the current time in UTC.
func (s *Service) clock() time.Time { return s.now().UTC() }

func newID() (uuid.UUID, error) { return uuid.NewV7() }

// invalidCredentials is the one answer every failed sign-in gets, whatever
// went wrong: a wrong password, an unknown number and a rejected provider
// token are indistinguishable from outside.
func invalidCredentials() error {
	return apperr.New(commonv1.ErrorReason_ERROR_REASON_INVALID_CREDENTIALS,
		"the sign-in credentials are not valid")
}

func accountDisabled() error {
	return apperr.New(commonv1.ErrorReason_ERROR_REASON_ACCOUNT_DISABLED,
		"the account is not active")
}

func otpInvalid() error {
	return apperr.New(commonv1.ErrorReason_ERROR_REASON_OTP_INVALID,
		"the code is wrong, expired or already used")
}

func tokenRevoked() error {
	return apperr.New(commonv1.ErrorReason_ERROR_REASON_TOKEN_REVOKED, "the session has ended")
}

func tokenExpired() error {
	return apperr.New(commonv1.ErrorReason_ERROR_REASON_TOKEN_EXPIRED, "the token has expired")
}

func tokenInvalid() error {
	return apperr.New(commonv1.ErrorReason_ERROR_REASON_TOKEN_INVALID, "the token is not valid")
}

func phoneTaken() error {
	return apperr.New(commonv1.ErrorReason_ERROR_REASON_PHONE_ALREADY_REGISTERED,
		"the phone number already belongs to an account")
}

func rateLimited(what string, retryAfter time.Duration) error {
	secs := max(int64(math.Ceil(retryAfter.Seconds())), 1)
	return apperr.New(commonv1.ErrorReason_ERROR_REASON_RATE_LIMITED, what,
		"retry_after_seconds", strconv.FormatInt(secs, 10))
}

func parseRequestID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, apperr.Validation(&commonv1.FieldViolation{
			Field: "request_id", Description: "value must be a valid UUID", Rule: "string.uuid",
		})
	}
	return id, nil
}

func parseChallengeID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, apperr.Validation(&commonv1.FieldViolation{
			Field: "challenge_id", Description: "value must be a valid UUID", Rule: "string.uuid",
		})
	}
	return id, nil
}

// allow counts a hit and refuses over the limit. It fails open: when Redis is
// unavailable the request is allowed and a warning logged.
func (s *Service) allow(ctx context.Context, key, message string) error {
	if s.limiter == nil {
		return nil
	}
	allowed, retryAfter, err := s.limiter.Allow(ctx, key)
	if err != nil {
		s.log.WarnContext(ctx, "rate limiter unavailable; allowing the request (fail open)", "error", err)
		return nil
	}
	if allowed {
		return nil
	}
	return rateLimited(message, retryAfter)
}

// exceeded refuses a request whose counter is already full, without counting
// it. Fails open like allow.
func (s *Service) exceeded(ctx context.Context, key, message string) error {
	if s.limiter == nil {
		return nil
	}
	over, retryAfter, err := s.limiter.Exceeded(ctx, key)
	if err != nil {
		s.log.WarnContext(ctx, "rate limiter unavailable; allowing the request (fail open)", "error", err)
		return nil
	}
	if !over {
		return nil
	}
	return rateLimited(message, retryAfter)
}

// countFailure records one failed sign-in. Errors are logged, never returned:
// a broken limiter must not turn a wrong password into an internal error.
func (s *Service) countFailure(ctx context.Context, key string) {
	if s.limiter == nil {
		return
	}
	if _, _, err := s.limiter.Allow(ctx, key); err != nil {
		s.log.WarnContext(ctx, "rate limiter unavailable; failed sign-in not counted", "error", err)
	}
}

// signInKey is the rate-limit key of a sign-in identity. It never contains a
// password or a token, and the phone number is the identifier being protected.
func signInKey(method authgatewayv1.SignInMethod, subject string) string {
	return "sign-in:" + methodText(method) + ":" + subject
}

func phoneCodeKey(phone string) string { return "phone-code:" + phone }

// methodText is the enum value name without its prefix ("GOOGLE"), the same
// text the repository stores.
func methodText(m authgatewayv1.SignInMethod) string {
	const prefix = "SIGN_IN_METHOD_"
	name := m.String()
	if len(name) > len(prefix) && name[:len(prefix)] == prefix {
		return name[len(prefix):]
	}
	return name
}

// wrapStore turns a repository failure into an INTERNAL error with context,
// leaving apperr errors (raised inside a transaction) alone.
func wrapStore(op string, err error) error {
	var ae *apperr.Error
	if err == nil || errors.As(err, &ae) {
		return err
	}
	return fmt.Errorf("%s: %w", op, err)
}
