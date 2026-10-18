// Package events builds identity's Kafka events and relays them from the
// transactional outbox to Kafka.
//
// identity publishes one stream (proto/events/v1):
// agrogram.identity.account.v1, AccountEvent (account.proto), keyed by the
// user id. Today the only event is AccountCreated.
//
// The builder validates its envelope with protovalidate. The service writes
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
	TopicAccount = "agrogram.identity.account.v1"
	ProducerName = "identity"
	ContentType  = "application/x-protobuf"

	HeaderContentType = "content-type"
	HeaderEventType   = "event-type"
	HeaderEventID     = "event-id"
	HeaderRequestID   = "x-request-id"
	HeaderTraceparent = "traceparent"
	HeaderTracestate  = "tracestate"
)

// EventMetadata.event_type of the account stream's events.
var (
	AccountCreatedType     = string(proto.MessageName(&eventsv1.AccountCreated{}))
	AccountRoleChangedType = string(proto.MessageName(&eventsv1.AccountRoleChanged{}))
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

// AccountChange describes one AccountEvent of agrogram.identity.account.v1.
type AccountChange struct {
	// Account is the account right after the event: the envelope snapshot
	// and EventMetadata.aggregate_version.
	Account domain.Account
	// Event is the concrete event: *eventsv1.AccountCreated or
	// *eventsv1.AccountRoleChanged.
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
	env := &eventsv1.AccountEvent{Account: accountSnapshot(c.Account)}
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
	default:
		return fmt.Errorf("account event: %s is not an AccountEvent case", proto.MessageName(e))
	}
	return nil
}

// accountSnapshot is the AccountEvent snapshot of a.
func accountSnapshot(a domain.Account) *eventsv1.AccountSnapshot {
	return &eventsv1.AccountSnapshot{
		Id:        a.ID.String(),
		Roles:     roleSet(a.Roles),
		Version:   a.Version,
		CreatedAt: timestamppb.New(a.CreatedAt),
		UpdatedAt: timestamppb.New(a.UpdatedAt),
	}
}

// roleSet returns roles sorted and without duplicates, as the stream carries
// them.
func roleSet(roles []commonv1.Role) []commonv1.Role {
	s := slices.Clone(roles)
	slices.Sort(s)
	return slices.Compact(s)
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
