// Package service is identity's business logic: accounts and authorization.
// AGROGRAM has one flat tier and no seller verification, so identity keeps
// no plan and verifies nobody; ADMIN (staff) is the only role.
package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tbilol/dxrl/services/identity/internal/apperr"
	"github.com/tbilol/dxrl/services/identity/internal/auth"
	"github.com/tbilol/dxrl/services/identity/internal/domain"
	"github.com/tbilol/dxrl/services/identity/internal/events"
)

// Queries is the repository. Methods return domain.ErrNotFound for missing
// rows. Callers pass every timestamp, so the service clock drives all state.
type Queries interface {
	// CreateAccount inserts a (with its roles) unless an account with the same
	// request id exists; it returns the stored account and whether it is new.
	CreateAccount(ctx context.Context, a domain.Account, requestID uuid.UUID) (domain.Account, bool, error)
	GetAccount(ctx context.Context, id uuid.UUID) (domain.Account, error)
	EnqueueOutbox(ctx context.Context, m events.Message) error
}

// Store runs Queries without (Run) or within (InTx) a transaction. InTx
// commits when fn returns nil and rolls back otherwise.
type Store interface {
	Run(ctx context.Context, fn func(Queries) error) error
	InTx(ctx context.Context, fn func(Queries) error) error
}

// Service implements identity's use cases.
type Service struct {
	store Store
	now   func() time.Time
	log   *slog.Logger
}

// Option customizes a Service.
type Option func(*Service)

// WithClock replaces time.Now (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(s *Service) { s.log = l } }

// New returns a Service.
func New(store Store, opts ...Option) *Service {
	s := &Service{
		store: store,
		now:   time.Now,
		log:   slog.New(slog.DiscardHandler),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// clock returns the current time in UTC.
func (s *Service) clock() time.Time { return s.now().UTC() }

func notFound(what string, id uuid.UUID) error {
	return apperr.New(commonv1.ErrorReason_ERROR_REASON_NOT_FOUND, what+" not found", "user_id", id.String())
}

func newID() (uuid.UUID, error) { return uuid.NewV7() }

func parseRequestID(s string) (uuid.UUID, error) { return parseID("request_id", s) }

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

// enqueueAccountEvent writes one AccountEvent to the outbox through q, so it
// commits or rolls back with the state change. acct is the account right
// after the event (the snapshot).
func (s *Service) enqueueAccountEvent(ctx context.Context, q Queries, acct domain.Account, event proto.Message,
	at time.Time, tr trigger,
) error {
	msg, err := events.NewAccountEvent(events.AccountChange{
		Account:       acct,
		Event:         event,
		OccurredAt:    at,
		CorrelationID: tr.correlationID,
		Traceparent:   tr.traceparent,
		Tracestate:    tr.tracestate,
		Actor:         tr.actor,
	})
	if err != nil {
		return err
	}
	return q.EnqueueOutbox(ctx, msg)
}
