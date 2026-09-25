// Package service is catalog's business logic: the listings of both boards
// (create, read, update, delete), browsing and search, and the product
// reference data.
//
// catalog owns listings and product types only. A listing carries its
// owner's user id; names, avatars, badges and ratings are profile-social's,
// and the owner card is composed at the edge (TODO(edge)), so catalog never
// calls profile-social and consumes no profile events.
package service

import (
	"context"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/catalog/internal/apperr"
	"github.com/tbilol/dxrl/services/catalog/internal/auth"
	"github.com/tbilol/dxrl/services/catalog/internal/domain"
	"github.com/tbilol/dxrl/services/catalog/internal/events"
)

// CallerOrderEscrow is the x-agrogram-caller of order-escrow, which reads
// listings in any status to snapshot them into transactions (catalog.proto).
//
// TODO(security): the header is trusted as is, because the edge strips
// client-supplied x-agrogram-* values; bind it to a service identity (mTLS
// or a mesh) once the platform has one.
const CallerOrderEscrow = "order-escrow"

// Queries is the repository. Methods return domain.ErrNotFound for missing
// rows. Callers pass every timestamp, so the service clock drives all state.
type Queries interface {
	// LockOwner serializes the owner's listing creations for the rest of the
	// transaction (a transaction-level advisory lock), so the idempotency
	// lookup, the quota count and the insert of concurrent creations cannot
	// interleave.
	LockOwner(ctx context.Context, owner uuid.UUID) error
	// ListingByRequest returns the owner's listing created with requestID.
	ListingByRequest(ctx context.Context, owner, requestID uuid.UUID) (domain.Listing, error)
	// CountActiveListings counts the owner's ACTIVE listings of kind.
	CountActiveListings(ctx context.Context, owner uuid.UUID, kind commonv1.ListingKind) (int, error)
	// InsertListing stores a new listing with its search document.
	InsertListing(ctx context.Context, l domain.Listing, doc domain.SearchDocument) error
	// GetListing returns the listing in any status.
	GetListing(ctx context.Context, id uuid.UUID) (domain.Listing, error)
	// LockListing returns the listing and holds its row lock for the rest of
	// the transaction.
	LockListing(ctx context.Context, id uuid.UUID) (domain.Listing, error)
	// SaveListing writes l's owner fields, remaining quantity, status,
	// version and timestamps (never views_count), and doc unless it is nil
	// (the stored search document stays).
	SaveListing(ctx context.Context, l domain.Listing, doc *domain.SearchDocument) error
	// FindListings runs a listing query and returns up to q.Limit hits in
	// the query's order, strictly after q.After.
	FindListings(ctx context.Context, q domain.ListingQuery) ([]domain.Hit, error)

	// GetProductType returns the reference data of t.
	GetProductType(ctx context.Context, t commonv1.ProductType) (domain.ProductType, error)
	// ListProductTypes returns the product types of category (UNSPECIFIED:
	// all), retired ones included, by sort order.
	ListProductTypes(ctx context.Context, category commonv1.ProductCategory) ([]domain.ProductType, error)

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

// ViewRecorder counts a listing view in the background (views.Counter).
// Record never blocks and never fails the read that caused it; viewer is
// uuid.Nil for anonymous callers.
type ViewRecorder interface {
	Record(listing, viewer uuid.UUID)
}

// Service implements catalog's use cases.
type Service struct {
	store   Store
	limiter RateLimiter  // nil: no create rate limit
	views   ViewRecorder // nil: views are not counted
	quotas  domain.Quotas
	now     func() time.Time
	log     *slog.Logger
}

// Option customizes a Service.
type Option func(*Service)

// WithClock replaces time.Now (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(s *Service) { s.log = l } }

// WithQuotas replaces domain.DefaultQuotas.
func WithQuotas(q domain.Quotas) Option { return func(s *Service) { s.quotas = q } }

// New returns a Service. limiter and views may be nil: no create rate limit,
// and views are not counted.
func New(store Store, limiter RateLimiter, views ViewRecorder, opts ...Option) *Service {
	s := &Service{
		store:   store,
		limiter: limiter,
		views:   views,
		quotas:  domain.DefaultQuotas,
		now:     time.Now,
		log:     slog.New(slog.DiscardHandler),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// clock returns the current time in UTC with the microsecond precision
// Postgres stores, so values and page cursors round-trip exactly.
func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

func listingNotFound(id uuid.UUID) error {
	return apperr.New(commonv1.ErrorReason_ERROR_REASON_NOT_FOUND, "listing not found", "listing_id", id.String())
}

func newID() (uuid.UUID, error) { return uuid.NewV7() }

// parseID parses the UUID request field named field (the gRPC layer has
// validated it already; this guards direct callers).
func parseID(field, s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, invalid(field, "value must be a valid UUID", "string.uuid")
	}
	return id, nil
}

// invalid is a VALIDATION_FAILED error for one field.
func invalid(field, description, rule string) error {
	return apperr.Validation(&commonv1.FieldViolation{Field: field, Description: description, Rule: rule})
}

// checkRateLimit enforces the per-user limit of listing creations. It fails
// open: when Redis is unavailable the call is allowed and a warning logged.
func (s *Service) checkRateLimit(ctx context.Context, userID uuid.UUID) error {
	if s.limiter == nil {
		return nil
	}
	allowed, retryAfter, err := s.limiter.Allow(ctx, "create-listing:"+userID.String())
	if err != nil {
		s.log.WarnContext(ctx, "rate limiter unavailable; allowing the call (fail open)", "error", err)
		return nil
	}
	if allowed {
		return nil
	}
	secs := max(int64(math.Ceil(retryAfter.Seconds())), 1)
	return apperr.New(commonv1.ErrorReason_ERROR_REASON_RATE_LIMITED,
		"too many listings created; retry later",
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
