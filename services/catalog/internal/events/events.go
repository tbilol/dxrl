// Package events is catalog's Kafka side: it builds the listing events
// catalog publishes and relays them from the transactional outbox to Kafka.
//
// Published (outbox table -> Relay):
//
//   - agrogram.catalog.listing.v1: ListingEvent (listing.proto), keyed by the
//     listing id. Today the only case is ListingCreated. TODO(events):
//     listing.proto has no ListingUpdated / ListingClosed / ListingDeleted
//     cases yet, so updates and deletions publish nothing.
//
// catalog consumes no stream. TODO(phase3): order-escrow's transaction
// stream (agrogram.orderescrow.transaction.v1) will drive the remaining
// quantity, SOLD_OUT and "parties of transactions" visibility; its dead
// letters would go to agrogram.catalog.deadletter.v1.
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

	"github.com/tbilol/dxrl/services/catalog/internal/domain"
)

// Kafka conventions (proto/events/v1/metadata.proto).
const (
	TopicListing = "agrogram.catalog.listing.v1"
	ProducerName = "catalog"
	ContentType  = "application/x-protobuf"

	HeaderContentType = "content-type"
	HeaderEventType   = "event-type"
	HeaderEventID     = "event-id"
	HeaderRequestID   = "x-request-id"
	HeaderTraceparent = "traceparent"
	HeaderTracestate  = "tracestate"
)

// ListingCreatedType is EventMetadata.event_type of ListingCreated.
var ListingCreatedType = string(proto.MessageName(&eventsv1.ListingCreated{}))

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

// ListingChange describes one ListingEvent of agrogram.catalog.listing.v1.
type ListingChange struct {
	// Listing is the listing right after the event: the envelope snapshot
	// and EventMetadata.aggregate_version.
	Listing domain.Listing
	// Event is the concrete event: *eventsv1.ListingCreated.
	Event proto.Message

	OccurredAt    time.Time // commit time of the state change
	CorrelationID string    // x-request-id
	Traceparent   string
	Tracestate    string
	Actor         *commonv1.Actor
}

// NewListingEvent builds and validates a ListingEvent for
// agrogram.catalog.listing.v1, keyed by the listing id. event_type is the
// full name of c.Event.
func NewListingEvent(c ListingChange) (Message, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Message{}, fmt.Errorf("event id: %w", err)
	}
	env := &eventsv1.ListingEvent{Listing: Snapshot(c.Listing)}
	switch e := c.Event.(type) {
	case *eventsv1.ListingCreated:
		env.Event = &eventsv1.ListingEvent_ListingCreated{ListingCreated: e}
	// TODO(events): ListingUpdated / ListingClosed / ListingDeleted once
	// listing.proto defines them (new oneof cases).
	case nil:
		return Message{}, fmt.Errorf("listing event: no event given")
	default:
		return Message{}, fmt.Errorf("listing event: %s is not a ListingEvent case", proto.MessageName(e))
	}
	env.Metadata = &eventsv1.EventMetadata{
		EventId:          id.String(),
		EventType:        string(proto.MessageName(c.Event)),
		OccurredAt:       timestamppb.New(c.OccurredAt),
		Producer:         ProducerName,
		AggregateType:    commonv1.ResourceType_RESOURCE_TYPE_LISTING,
		AggregateId:      c.Listing.ID.String(),
		AggregateVersion: c.Listing.Version,
		CorrelationId:    c.CorrelationID,
		Traceparent:      c.Traceparent,
		Tracestate:       c.Tracestate,
		Actor:            c.Actor,
	}
	return newMessage(id, TopicListing, c.Listing.ID.String(), env, env.GetMetadata(), c.OccurredAt)
}

// Snapshot is the ListingSnapshot of l: the public state of the listing.
// The contact phone is left out on purpose (metadata.proto: never publish
// contact details), and so are the owner card and views.
func Snapshot(l domain.Listing) *eventsv1.ListingSnapshot {
	return &eventsv1.ListingSnapshot{
		Id:                l.ID.String(),
		Kind:              l.Kind,
		OwnerId:           l.OwnerID.String(),
		Product:           l.Product,
		Quantity:          l.Quantity.Proto(),
		RemainingQuantity: l.Remaining().Proto(), // TODO(phase3): equals quantity for now
		Price:             l.Price.Proto(),
		Address:           l.Address.Proto(),
		Notes:             l.Notes,
		Media:             domain.MediaRefs(l.MediaIDs), // TODO(media): ids only
		Status:            l.Status,
		Version:           l.Version,
		CreatedAt:         timestamppb.New(l.CreatedAt),
		UpdatedAt:         timestamppb.New(l.UpdatedAt),
	}
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
