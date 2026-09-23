// Package service is identity's business logic: accounts, authorization, the
// e-ijara farmer verification flow and its background worker.
package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/identity/internal/apperr"
	"github.com/tbilol/dxrl/services/identity/internal/domain"
	"github.com/tbilol/dxrl/services/identity/internal/eijara"
	"github.com/tbilol/dxrl/services/identity/internal/events"
)

// Queries is the repository. Methods return domain.ErrNotFound for missing
// rows. Callers pass every timestamp, so the service clock drives all state.
type Queries interface {
	// CreateAccount inserts a (with its roles) unless an account with the same
	// request id exists; it returns the stored account and whether it is new.
	CreateAccount(ctx context.Context, a domain.Account, requestID uuid.UUID) (domain.Account, bool, error)
	GetAccount(ctx context.Context, id uuid.UUID) (domain.Account, error)
	// LockAccount takes the account's row lock for the rest of the
	// transaction; role changes are serialized per account through it.
	LockAccount(ctx context.Context, id uuid.UUID) error
	// GrantRole adds a role and bumps the account version; false if held.
	GrantRole(ctx context.Context, id uuid.UUID, role commonv1.Role, now time.Time) (bool, error)
	// RevokeFarmerRoleUnlessVerified removes ROLE_FARMER (bumping the
	// version) unless a VERIFIED verification remains; false if unchanged.
	RevokeFarmerRoleUnlessVerified(ctx context.Context, id uuid.UUID, now time.Time) (bool, error)

	GetVerification(ctx context.Context, id uuid.UUID) (domain.Verification, error)
	VerificationByRequest(ctx context.Context, userID, requestID uuid.UUID) (domain.Verification, error)
	LatestVerification(ctx context.Context, userID uuid.UUID) (domain.Verification, error)
	// ContractClaimed reports whether another user's VERIFIED verification
	// holds the contract number.
	ContractClaimed(ctx context.Context, contractNumber string, userID uuid.UUID) (bool, error)
	// InsertVerification stores a PENDING verification unless (user_id,
	// request_id) exists; it returns the stored row and whether it is new.
	InsertVerification(ctx context.Context, v domain.Verification) (domain.Verification, bool, error)
	// DecideVerification applies d if the verification is still PENDING and
	// reports whether it did. It returns domain.ErrContractClaimed when the
	// contract already has a VERIFIED verification.
	DecideVerification(ctx context.Context, d domain.Decision) (bool, error)
	// ClaimPendingVerifications picks PENDING verifications due at now
	// (skipping rows other workers lock), counts an attempt and moves their
	// next attempt out by min(backoff*2^attempts, maxBackoff).
	ClaimPendingVerifications(ctx context.Context, now time.Time, limit int, backoff, maxBackoff time.Duration) ([]domain.Verification, error)
	RecordVerificationError(ctx context.Context, id uuid.UUID, msg string) error
	// ExpiryCandidates lists VERIFIED verifications whose lease ended before today.
	ExpiryCandidates(ctx context.Context, today time.Time, limit int) ([]domain.Verification, error)
	// ExpireVerification marks one of them EXPIRED if it still qualifies.
	ExpireVerification(ctx context.Context, id uuid.UUID, today, now time.Time) (domain.Verification, bool, error)

	EnqueueOutbox(ctx context.Context, m events.Message) error
}

// Store runs Queries without (Run) or within (InTx) a transaction. InTx
// commits when fn returns nil and rolls back otherwise.
type Store interface {
	Run(ctx context.Context, fn func(Queries) error) error
	InTx(ctx context.Context, fn func(Queries) error) error
}

// EIjaraChecker asks e-ijara about a lease contract number. It must honour
// ctx: the service calls it with a short timeout.
type EIjaraChecker interface {
	Check(ctx context.Context, contractNumber string) (eijara.Result, error)
}

// RateLimiter counts a hit for key and says whether it is allowed.
type RateLimiter interface {
	Allow(ctx context.Context, key string) (allowed bool, retryAfter time.Duration, err error)
}

// Config tunes the verification flow and the worker.
type Config struct {
	EIjaraTimeout   time.Duration // inline e-ijara call
	RetryBackoff    time.Duration // first background retry delay, doubled per attempt
	RetryBackoffMax time.Duration
	WorkerInterval  time.Duration
	WorkerBatchSize int
}

// Service implements identity's use cases.
type Service struct {
	store   Store
	checker EIjaraChecker
	limiter RateLimiter // nil: rate limiting disabled
	cfg     Config
	now     func() time.Time
	log     *slog.Logger
}

// Option customizes a Service.
type Option func(*Service)

// WithClock replaces time.Now (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(s *Service) { s.log = l } }

// New returns a Service. limiter may be nil (no rate limiting).
func New(store Store, checker EIjaraChecker, limiter RateLimiter, cfg Config, opts ...Option) *Service {
	s := &Service{
		store:   store,
		checker: checker,
		limiter: limiter,
		cfg:     cfg,
		now:     time.Now,
		log:     slog.New(slog.DiscardHandler),
	}
	for _, o := range opts {
		o(s)
	}
	if s.cfg.EIjaraTimeout <= 0 {
		s.cfg.EIjaraTimeout = 3 * time.Second
	}
	if s.cfg.RetryBackoff <= 0 {
		s.cfg.RetryBackoff = 30 * time.Second
	}
	if s.cfg.RetryBackoffMax < s.cfg.RetryBackoff {
		s.cfg.RetryBackoffMax = s.cfg.RetryBackoff
	}
	if s.cfg.WorkerInterval <= 0 {
		s.cfg.WorkerInterval = 30 * time.Second
	}
	if s.cfg.WorkerBatchSize <= 0 {
		s.cfg.WorkerBatchSize = 50
	}
	return s
}

// clock returns the current time in UTC.
func (s *Service) clock() time.Time { return s.now().UTC() }

func notFound(what string, id uuid.UUID) error {
	return apperr.New(commonv1.ErrorReason_ERROR_REASON_NOT_FOUND, what+" not found", "user_id", id.String())
}

func newID() (uuid.UUID, error) { return uuid.NewV7() }

func parseRequestID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, apperr.Validation(&commonv1.FieldViolation{
			Field: "request_id", Description: "value must be a valid UUID", Rule: "string.uuid",
		})
	}
	return id, nil
}
