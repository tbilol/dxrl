// Package service is profile-social's business logic: profiles, follows,
// ratings, the account projection fed by identity's account stream, and a
// user's listings (read from catalog).
package service

import (
	"context"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/profile-social/internal/apperr"
	"github.com/tbilol/dxrl/services/profile-social/internal/auth"
	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
	"github.com/tbilol/dxrl/services/profile-social/internal/events"
)

// Queries is the repository. Methods return domain.ErrNotFound for missing
// rows. Callers pass every timestamp, so the service clock drives all state.
type Queries interface {
	// GetProfile returns the row of id in any status, with IsFollowedByMe
	// set for viewer (uuid.Nil: nobody).
	GetProfile(ctx context.Context, id, viewer uuid.UUID) (domain.Profile, error)
	// EnsureProfile inserts a PENDING row for id unless one exists.
	EnsureProfile(ctx context.Context, id uuid.UUID, now time.Time) error
	// LockProfiles takes the row locks of the existing ids in id order (so
	// concurrent callers cannot deadlock) for the rest of the transaction and
	// returns those rows; missing ids are absent from the map.
	LockProfiles(ctx context.Context, ids ...uuid.UUID) (map[uuid.UUID]domain.Profile, error)
	// LockFollowGraph locks id's row and the rows of everyone it follows or
	// is followed by, in id order, and returns id's row.
	LockFollowGraph(ctx context.Context, id uuid.UUID) (domain.Profile, error)
	// SaveProfile writes p's user-owned fields, status, version, create
	// request id and timestamps (never the counters or the projection).
	SaveProfile(ctx context.Context, p domain.Profile) error

	// InsertFollow adds the edge and raises both counters; false (and no
	// counter change) when the edge exists.
	InsertFollow(ctx context.Context, follower, followee uuid.UUID, now time.Time) (bool, error)
	// DeleteFollow removes the edge and lowers both counters; false when
	// there was no edge.
	DeleteFollow(ctx context.Context, follower, followee uuid.UUID) (bool, error)
	// RemoveFollows deletes every edge of id, lowers the other sides'
	// counters and zeroes id's counters.
	RemoveFollows(ctx context.Context, id uuid.UUID) error
	// ListFollowers and ListFollowing return up to limit ACTIVE profiles,
	// newest follow first, strictly after the cursor (nil: from the start).
	ListFollowers(ctx context.Context, id, viewer uuid.UUID, after *domain.Cursor, limit int) ([]domain.FollowEntry, error)
	ListFollowing(ctx context.Context, id, viewer uuid.UUID, after *domain.Cursor, limit int) ([]domain.FollowEntry, error)

	// MarkProcessed stores an inbox row; false when the event id is known.
	MarkProcessed(ctx context.Context, e domain.ProcessedEvent) (bool, error)
	// ApplyAccount upserts userID's account projection (creating a PENDING
	// row when there is none) if a.Version is newer than the stored one, and
	// reports whether it did.
	ApplyAccount(ctx context.Context, userID uuid.UUID, a domain.Account, now time.Time) (bool, error)

	GetCompletedTransaction(ctx context.Context, id uuid.UUID) (domain.CompletedTransaction, error)
	// InsertCompletedTransaction stores t unless its id is recorded; false
	// when it was.
	InsertCompletedTransaction(ctx context.Context, t domain.CompletedTransaction, now time.Time) (bool, error)
	// AddDeal adds one to deals_count of each id.
	AddDeal(ctx context.Context, ids ...uuid.UUID) error
	RatingByRequest(ctx context.Context, raterID, requestID uuid.UUID) (domain.Rating, error)
	// InsertRating stores r; false (nothing stored) when the rater already
	// rated the transaction or used the request id.
	InsertRating(ctx context.Context, r domain.Rating) (bool, error)
	// AddRating adds score to userID's aggregate, bumps the version, sets
	// updated_at and returns the profile.
	AddRating(ctx context.Context, userID uuid.UUID, score int32, now time.Time) (domain.Profile, error)
	// ListRatings returns up to limit ratings userID received, newest first,
	// strictly after the cursor (nil: from the start).
	ListRatings(ctx context.Context, userID uuid.UUID, after *domain.Cursor, limit int) ([]domain.Rating, error)

	EnqueueOutbox(ctx context.Context, m events.Message) error
}

// Store runs Queries without (Run) or within (InTx) a transaction. InTx
// commits when fn returns nil and rolls back otherwise.
type Store interface {
	Run(ctx context.Context, fn func(Queries) error) error
	InTx(ctx context.Context, fn func(Queries) error) error
}

// RateLimiter counts a hit for key and says whether it is allowed.
type RateLimiter interface {
	Allow(ctx context.Context, key string) (allowed bool, retryAfter time.Duration, err error)
}

// ListingSource reads one owner's listings from catalog. Failures the
// caller may degrade on wrap domain.ErrDependencyUnavailable.
type ListingSource interface {
	ListUserListings(ctx context.Context, p auth.Principal, q domain.ListingQuery) (domain.ListingPage, error)
}

// Service implements profile-social's use cases.
type Service struct {
	store    Store
	limiter  RateLimiter   // nil: rate limiting disabled
	listings ListingSource // nil: catalog not configured
	now      func() time.Time
	log      *slog.Logger
}

// Option customizes a Service.
type Option func(*Service)

// WithClock replaces time.Now (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(s *Service) { s.log = l } }

// New returns a Service. limiter and listings may be nil: no follow rate
// limit, and ListUserListings answers empty pages.
func New(store Store, limiter RateLimiter, listings ListingSource, opts ...Option) *Service {
	s := &Service{
		store:    store,
		limiter:  limiter,
		listings: listings,
		now:      time.Now,
		log:      slog.New(slog.DiscardHandler),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// clock returns the current time in UTC with the microsecond precision
// Postgres stores, so values and page cursors round-trip exactly.
func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// present fills the fields computed per request.
func (s *Service) present(p domain.Profile) domain.Profile {
	p.IsPremium = p.Account.PremiumActive(s.clock())
	return p
}

func notFound(what string, id uuid.UUID) error {
	return apperr.New(commonv1.ErrorReason_ERROR_REASON_NOT_FOUND, what+" not found", "user_id", id.String())
}

func newID() (uuid.UUID, error) { return uuid.NewV7() }

// parseID parses the UUID request field named field (the gRPC layer has
// validated it already; this guards direct callers).
func parseID(field, s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, apperr.Validation(&commonv1.FieldViolation{
			Field: field, Description: "value must be a valid UUID", Rule: "string.uuid",
		})
	}
	return id, nil
}

// checkRateLimit enforces the per-user follow/unfollow limit. It fails open:
// when Redis is unavailable the call is allowed and a warning logged.
func (s *Service) checkRateLimit(ctx context.Context, userID uuid.UUID) error {
	if s.limiter == nil {
		return nil
	}
	allowed, retryAfter, err := s.limiter.Allow(ctx, "follow:"+userID.String())
	if err != nil {
		s.log.WarnContext(ctx, "rate limiter unavailable; allowing the call (fail open)", "error", err)
		return nil
	}
	if allowed {
		return nil
	}
	secs := max(int64(math.Ceil(retryAfter.Seconds())), 1)
	return apperr.New(commonv1.ErrorReason_ERROR_REASON_RATE_LIMITED,
		"too many follow changes; retry later",
		"retry_after_seconds", strconv.FormatInt(secs, 10))
}

// trigger is who caused a change, for the event metadata.
type trigger struct {
	actor         *commonv1.Actor
	correlationID string
	traceparent   string
	tracestate    string
}

// requestTrigger is the caller of an RPC (see auth.Principal.Actor) with the
// request's correlation id and trace context.
func requestTrigger(p auth.Principal) trigger {
	tr := trigger{actor: p.Actor(), correlationID: p.RequestID, traceparent: p.Traceparent, tracestate: p.Tracestate}
	if tr.correlationID == "" { // only direct callers skip the interceptor's fallback
		id, err := newID()
		if err != nil {
			id = uuid.New()
		}
		tr.correlationID = id.String()
	}
	return tr
}
