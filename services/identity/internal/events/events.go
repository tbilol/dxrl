// Package events builds identity's Kafka events and relays them from the
// transactional outbox to Kafka.
//
// identity publishes two streams (proto/events/v1):
//
//   - agrogram.identity.account.v1: AccountEvent (account.proto), identity's
//     own stream, keyed by the user id.
//   - agrogram.notification.request.v1: NotificationRequestEvent
//     (notification.proto), keyed by the recipient.
//
// Every builder validates its envelope with protovalidate. The service writes
// the resulting Message to the outbox in the transaction of the state change
// it describes; the Relay publishes it.
package events

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tbilol/dxrl/services/identity/internal/domain"
)

// Kafka conventions (proto/events/v1/metadata.proto).
const (
	TopicAccount             = "agrogram.identity.account.v1"
	TopicNotificationRequest = "agrogram.notification.request.v1"
	ProducerName             = "identity"
	ContentType              = "application/x-protobuf"

	HeaderContentType = "content-type"
	HeaderEventType   = "event-type"
	HeaderEventID     = "event-id"
	HeaderRequestID   = "x-request-id"
	HeaderTraceparent = "traceparent"
	HeaderTracestate  = "tracestate"
)

// Notification templates identity requests (owned by the notification service).
const (
	TemplateFarmerVerified             = "account.farmer_verified"
	TemplateFarmerVerificationRejected = "account.farmer_verification_rejected"
	TemplateFarmerVerificationExpired  = "account.farmer_verification_expired"
)

// NotificationRequestedType is EventMetadata.event_type of NotificationRequested.
var NotificationRequestedType = string(proto.MessageName(&eventsv1.NotificationRequested{}))

// EventMetadata.event_type of the account stream's events.
var (
	AccountCreatedType             = string(proto.MessageName(&eventsv1.AccountCreated{}))
	AccountRoleChangedType         = string(proto.MessageName(&eventsv1.AccountRoleChanged{}))
	AccountPremiumChangedType      = string(proto.MessageName(&eventsv1.AccountPremiumChanged{}))
	FarmerVerifiedType             = string(proto.MessageName(&eventsv1.FarmerVerified{}))
	FarmerVerificationRejectedType = string(proto.MessageName(&eventsv1.FarmerVerificationRejected{}))
	FarmerVerificationExpiredType  = string(proto.MessageName(&eventsv1.FarmerVerificationExpired{}))
)

// Message is one outbox row: a Kafka record ready to publish.
type Message struct {
	ID        uuid.UUID // EventMetadata.event_id
	Topic     string
	Key       string
	Value     []byte
	Headers   map[string]string
	EventType string
	CreatedAt time.Time
}

// Notification describes a NotificationRequested event.
type Notification struct {
	RecipientID uuid.UUID
	Category    eventsv1.NotificationCategory
	TemplateKey string
	Params      map[string]string
	Subject     *commonv1.ResourceRef

	OccurredAt    time.Time // commit time of the state change
	CorrelationID string    // x-request-id, or a fresh id for background jobs
	Traceparent   string
	Tracestate    string
	Actor         *commonv1.Actor
}

// NewNotificationRequest builds and validates a NotificationRequestEvent for
// agrogram.notification.request.v1, keyed by the recipient.
func NewNotificationRequest(n Notification) (Message, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Message{}, fmt.Errorf("event id: %w", err)
	}
	env := &eventsv1.NotificationRequestEvent{
		Metadata: &eventsv1.EventMetadata{
			EventId:          id.String(),
			EventType:        NotificationRequestedType,
			OccurredAt:       timestamppb.New(n.OccurredAt),
			Producer:         ProducerName,
			AggregateType:    commonv1.ResourceType_RESOURCE_TYPE_USER,
			AggregateId:      n.RecipientID.String(),
			AggregateVersion: 0, // the notification stream has no versioned aggregate
			CorrelationId:    n.CorrelationID,
			Traceparent:      n.Traceparent,
			Tracestate:       n.Tracestate,
			Actor:            n.Actor,
		},
		Event: &eventsv1.NotificationRequestEvent_NotificationRequested{
			NotificationRequested: &eventsv1.NotificationRequested{
				RecipientId: n.RecipientID.String(),
				Category:    n.Category,
				TemplateKey: n.TemplateKey,
				Params:      n.Params,
				Subject:     n.Subject,
				// Channels stay empty: the category's defaults apply.
			},
		},
	}
	return newMessage(id, TopicNotificationRequest, n.RecipientID.String(), env, env.GetMetadata(), n.OccurredAt)
}

// AccountChange describes one AccountEvent of agrogram.identity.account.v1.
type AccountChange struct {
	// Account is the account right after the event: the envelope snapshot
	// and EventMetadata.aggregate_version.
	Account domain.Account
	// Event is the concrete event: *eventsv1.AccountCreated,
	// *eventsv1.AccountRoleChanged, *eventsv1.AccountPremiumChanged,
	// *eventsv1.FarmerVerified, *eventsv1.FarmerVerificationRejected or
	// *eventsv1.FarmerVerificationExpired.
	Event proto.Message

	OccurredAt    time.Time // commit time of the state change
	CorrelationID string    // x-request-id, or a fresh id for background jobs
	Traceparent   string
	Tracestate    string
	Actor         *commonv1.Actor
}

// NewAccountEvent builds and validates an AccountEvent for
// agrogram.identity.account.v1, keyed by the user id. event_type is the full
// name of c.Event.
func NewAccountEvent(c AccountChange) (Message, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Message{}, fmt.Errorf("event id: %w", err)
	}
	env := &eventsv1.AccountEvent{Account: accountSnapshot(c.Account, c.OccurredAt)}
	if err := setAccountEvent(env, c.Event); err != nil {
		return Message{}, err
	}
	env.Metadata = &eventsv1.EventMetadata{
		EventId:          id.String(),
		EventType:        string(proto.MessageName(c.Event)),
		OccurredAt:       timestamppb.New(c.OccurredAt),
		Producer:         ProducerName,
		AggregateType:    commonv1.ResourceType_RESOURCE_TYPE_USER,
		AggregateId:      c.Account.ID.String(),
		AggregateVersion: c.Account.Version,
		CorrelationId:    c.CorrelationID,
		Traceparent:      c.Traceparent,
		Tracestate:       c.Tracestate,
		Actor:            c.Actor,
	}
	return newMessage(id, TopicAccount, c.Account.ID.String(), env, env.GetMetadata(), c.OccurredAt)
}

// setAccountEvent puts e into the envelope's oneof.
func setAccountEvent(env *eventsv1.AccountEvent, e proto.Message) error {
	if e == nil || !e.ProtoReflect().IsValid() {
		return errors.New("account event: no event given")
	}
	switch e := e.(type) {
	case *eventsv1.AccountCreated:
		env.Event = &eventsv1.AccountEvent_AccountCreated{AccountCreated: e}
	case *eventsv1.AccountRoleChanged:
		env.Event = &eventsv1.AccountEvent_AccountRoleChanged{AccountRoleChanged: e}
	case *eventsv1.AccountPremiumChanged:
		env.Event = &eventsv1.AccountEvent_AccountPremiumChanged{AccountPremiumChanged: e}
	case *eventsv1.FarmerVerified:
		env.Event = &eventsv1.AccountEvent_FarmerVerified{FarmerVerified: e}
	case *eventsv1.FarmerVerificationRejected:
		env.Event = &eventsv1.AccountEvent_FarmerVerificationRejected{FarmerVerificationRejected: e}
	case *eventsv1.FarmerVerificationExpired:
		env.Event = &eventsv1.AccountEvent_FarmerVerificationExpired{FarmerVerificationExpired: e}
	default:
		return fmt.Errorf("account event: %s is not an AccountEvent case", proto.MessageName(e))
	}
	return nil
}

// accountSnapshot is the AccountEvent snapshot of a. Premium is evaluated at
// the event time; premium_until is published only while Premium is active.
func accountSnapshot(a domain.Account, at time.Time) *eventsv1.AccountSnapshot {
	roles := roleSet(a.Roles)
	s := &eventsv1.AccountSnapshot{
		Id:               a.ID.String(),
		Roles:            roles,
		IsVerifiedFarmer: slices.Contains(roles, domain.RoleFarmer),
		IsPremium:        a.PremiumActive(at),
		Version:          a.Version,
		CreatedAt:        timestamppb.New(a.CreatedAt),
		UpdatedAt:        timestamppb.New(a.UpdatedAt),
	}
	if s.IsPremium && a.PremiumUntil != nil {
		s.PremiumUntil = timestamppb.New(*a.PremiumUntil)
	}
	return s
}

// RoleChange returns the AccountRoleChanged for a role set that went from
// previous to current, or nil when both hold the same roles: no event then.
// verificationID is the farmer verification that caused it (uuid.Nil: none).
func RoleChange(previous, current []commonv1.Role, verificationID uuid.UUID) *eventsv1.AccountRoleChanged {
	prev, cur := roleSet(previous), roleSet(current)
	var granted, revoked []commonv1.Role
	for _, r := range cur {
		if !slices.Contains(prev, r) {
			granted = append(granted, r)
		}
	}
	for _, r := range prev {
		if !slices.Contains(cur, r) {
			revoked = append(revoked, r)
		}
	}
	if len(granted) == 0 && len(revoked) == 0 {
		return nil
	}
	e := &eventsv1.AccountRoleChanged{
		PreviousRoles: prev,
		Roles:         cur,
		GrantedRoles:  granted,
		RevokedRoles:  revoked,
	}
	if verificationID != uuid.Nil {
		e.FarmerVerificationId = verificationID.String()
	}
	return e
}

// roleSet returns roles sorted and without duplicates, as the stream carries
// them.
func roleSet(roles []commonv1.Role) []commonv1.Role {
	s := slices.Clone(roles)
	slices.Sort(s)
	return slices.Compact(s)
}

// VerificationEvent returns the account-stream event of a farmer verification
// that was just decided or expired: FarmerVerified (VERIFIED),
// FarmerVerificationRejected (REJECTED) or FarmerVerificationExpired
// (EXPIRED). The contract number is deliberately left out.
func VerificationEvent(v domain.Verification) (proto.Message, error) {
	switch v.Status {
	case domain.VerificationVerified:
		if v.Lease == nil {
			return nil, fmt.Errorf("verification %s is VERIFIED without a lease", v.ID)
		}
		return &eventsv1.FarmerVerified{
			VerificationId:  v.ID.String(),
			LeaseValidFrom:  civilDate(v.Lease.ValidFrom),
			LeaseValidUntil: civilDate(v.Lease.ValidUntil),
		}, nil
	case domain.VerificationRejected:
		return &eventsv1.FarmerVerificationRejected{
			VerificationId:  v.ID.String(),
			RejectionReason: v.RejectionReason,
		}, nil
	case domain.VerificationExpired:
		if v.Lease == nil {
			return nil, fmt.Errorf("verification %s is EXPIRED without a lease", v.ID)
		}
		return &eventsv1.FarmerVerificationExpired{
			VerificationId:  v.ID.String(),
			LeaseValidUntil: civilDate(v.Lease.ValidUntil),
		}, nil
	default:
		return nil, fmt.Errorf("verification %s is %s: no account event for that status", v.ID, v.Status)
	}
}

// civilDate converts a civil date (00:00 UTC, see domain.TashkentDate).
func civilDate(t time.Time) *commonv1.Date {
	y, m, d := t.Date()
	return &commonv1.Date{Year: int32(y), Month: int32(m), Day: int32(d)}
}

// newMessage validates env, whose header is md, and turns it into an outbox
// message for topic, keyed by key, with the standard record headers.
func newMessage(id uuid.UUID, topic, key string, env proto.Message, md *eventsv1.EventMetadata, occurredAt time.Time) (Message, error) {
	name := env.ProtoReflect().Descriptor().Name()
	if err := protovalidate.Validate(env); err != nil {
		return Message{}, fmt.Errorf("invalid %s: %w", name, err)
	}
	value, err := proto.Marshal(env)
	if err != nil {
		return Message{}, fmt.Errorf("marshal %s: %w", name, err)
	}
	headers := map[string]string{
		HeaderContentType: ContentType,
		HeaderEventType:   md.GetEventType(),
		HeaderEventID:     id.String(),
		HeaderRequestID:   md.GetCorrelationId(),
	}
	if tp := md.GetTraceparent(); tp != "" {
		headers[HeaderTraceparent] = tp
	}
	if ts := md.GetTracestate(); ts != "" {
		headers[HeaderTracestate] = ts
	}
	return Message{
		ID:        id,
		Topic:     topic,
		Key:       key,
		Value:     value,
		Headers:   headers,
		EventType: md.GetEventType(),
		CreatedAt: occurredAt,
	}, nil
}
