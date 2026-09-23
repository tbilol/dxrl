// Package events builds identity's Kafka events and relays them from the
// transactional outbox to Kafka.
//
// The only contract identity can publish today is the notification request
// stream (proto/events/v1/notification.proto). The account stream
// (agrogram.identity.account.v1, AccountEvent) is planned in metadata.proto
// but events/v1/account.proto does not exist yet; see the AccountEvent TODO
// comments in the service package.
package events

import (
	"fmt"
	"time"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Kafka conventions (proto/events/v1/metadata.proto).
const (
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
	if err := protovalidate.Validate(env); err != nil {
		return Message{}, fmt.Errorf("invalid NotificationRequestEvent: %w", err)
	}
	value, err := proto.Marshal(env)
	if err != nil {
		return Message{}, fmt.Errorf("marshal NotificationRequestEvent: %w", err)
	}
	headers := map[string]string{
		HeaderContentType: ContentType,
		HeaderEventType:   NotificationRequestedType,
		HeaderEventID:     id.String(),
		HeaderRequestID:   n.CorrelationID,
	}
	if n.Traceparent != "" {
		headers[HeaderTraceparent] = n.Traceparent
	}
	if n.Tracestate != "" {
		headers[HeaderTracestate] = n.Tracestate
	}
	return Message{
		ID:        id,
		Topic:     TopicNotificationRequest,
		Key:       n.RecipientID.String(),
		Value:     value,
		Headers:   headers,
		EventType: NotificationRequestedType,
		CreatedAt: n.OccurredAt,
	}, nil
}
