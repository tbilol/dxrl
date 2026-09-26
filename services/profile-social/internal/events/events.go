// Package events is profile-social's Kafka side: it builds the events
// profile-social publishes, relays them from the transactional outbox to
// Kafka, and consumes the streams it projects.
//
// Published (outbox table -> Relay):
//
//   - agrogram.profilesocial.profile.v1: ProfileEvent (profile.proto), keyed
//     by the user id. Today the only case is RatingUpdated.
//
// Consumed (Consumer, one consumer group for both streams, "profilesocial" by
// default):
//
//   - agrogram.identity.account.v1: AccountEvent (account.proto), applied to
//     the account projection on profiles by AccountHandler.
//   - agrogram.orderescrow.transaction.v1: TransactionEvent
//     (transaction.proto). TransactionHandler records a transaction once its
//     snapshot is COMPLETED (PayoutReleased), which unlocks RateUser for both
//     parties and counts one deal for each; earlier states are skipped.
//
// Records that cannot be processed are published as DeadLetter to
// agrogram.profilesocial.deadletter.v1 (metadata.proto).
package events

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
)

// Kafka conventions (proto/events/v1/metadata.proto).
const (
	TopicProfile     = "agrogram.profilesocial.profile.v1"
	TopicAccount     = "agrogram.identity.account.v1"
	TopicTransaction = "agrogram.orderescrow.transaction.v1"
	TopicDeadLetter  = "agrogram.profilesocial.deadletter.v1"
	ProducerName     = "profile-social"
	ContentType      = "application/x-protobuf"

	HeaderContentType = "content-type"
	HeaderEventType   = "event-type"
	HeaderEventID     = "event-id"
	HeaderRequestID   = "x-request-id"
	HeaderTraceparent = "traceparent"
	HeaderTracestate  = "tracestate"
)

// RatingUpdatedType is EventMetadata.event_type of RatingUpdated.
var RatingUpdatedType = string(proto.MessageName(&eventsv1.RatingUpdated{}))

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

// ProfileChange describes one ProfileEvent of agrogram.profilesocial.profile.v1.
type ProfileChange struct {
	// Profile is the profile right after the event: the envelope snapshot and
	// EventMetadata.aggregate_version. It needs a display name (the snapshot
	// requires one), so only created profiles have events.
	Profile domain.Profile
	// Event is the concrete event: *eventsv1.RatingUpdated.
	Event proto.Message

	OccurredAt    time.Time // commit time of the state change
	CorrelationID string    // x-request-id
	Traceparent   string
	Tracestate    string
	Actor         *commonv1.Actor
}

// NewProfileEvent builds and validates a ProfileEvent for
// agrogram.profilesocial.profile.v1, keyed by the user id. event_type is the
// full name of c.Event.
func NewProfileEvent(c ProfileChange) (Message, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Message{}, fmt.Errorf("event id: %w", err)
	}
	env := &eventsv1.ProfileEvent{Profile: profileSnapshot(c.Profile, c.OccurredAt)}
	switch e := c.Event.(type) {
	case *eventsv1.RatingUpdated:
		env.Event = &eventsv1.ProfileEvent_RatingUpdated{RatingUpdated: e}
	case nil:
		return Message{}, fmt.Errorf("profile event: no event given")
	default:
		return Message{}, fmt.Errorf("profile event: %s is not a ProfileEvent case", proto.MessageName(e))
	}
	env.Metadata = &eventsv1.EventMetadata{
		EventId:          id.String(),
		EventType:        string(proto.MessageName(c.Event)),
		OccurredAt:       timestamppb.New(c.OccurredAt),
		Producer:         ProducerName,
		AggregateType:    commonv1.ResourceType_RESOURCE_TYPE_USER,
		AggregateId:      c.Profile.ID.String(),
		AggregateVersion: c.Profile.Version,
		CorrelationId:    c.CorrelationID,
		Traceparent:      c.Traceparent,
		Tracestate:       c.Tracestate,
		Actor:            c.Actor,
	}
	return newMessage(id, TopicProfile, c.Profile.ID.String(), env, env.GetMetadata(), c.OccurredAt)
}

// profileSnapshot is the ProfileEvent snapshot of p. The Premium badge is
// evaluated at the event time.
func profileSnapshot(p domain.Profile, at time.Time) *eventsv1.ProfileSnapshot {
	s := &eventsv1.ProfileSnapshot{
		Id:               p.ID.String(),
		DisplayName:      p.DisplayName,
		Region:           p.Region,
		District:         p.District,
		Rating:           &eventsv1.RatingSummary{Average: p.RatingAverage(), Count: p.RatingCount},
		DealsCount:       p.DealsCount,
		FollowerCount:    p.FollowerCount,
		FollowingCount:   p.FollowingCount,
		IsVerifiedFarmer: p.Account.IsVerifiedFarmer,
		IsPremium:        p.Account.PremiumActive(at),
		Version:          p.Version,
		CreatedAt:        timestamppb.New(p.CreatedAt),
		UpdatedAt:        timestamppb.New(p.UpdatedAt),
	}
	if p.AvatarMediaID != uuid.Nil {
		// TODO(media): only the id; media-pipeline fills type, status and URLs.
		s.Avatar = &commonv1.MediaRef{MediaId: p.AvatarMediaID.String()}
	}
	return s
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

// maxDeadLetterMessage bounds DeadLetter.error.message.
const maxDeadLetterMessage = 1000

// NewDeadLetter wraps a record that could not be processed into a DeadLetter
// record for agrogram.profilesocial.deadletter.v1, keyed by the original key.
// Poison records (Permanent errors) carry ERROR_REASON_VALIDATION_FAILED,
// records whose retry budget ran out ERROR_REASON_INTERNAL.
func NewDeadLetter(r *kgo.Record, group string, cause error, attempts int, firstFailedAt, deadLetteredAt time.Time) (*kgo.Record, error) {
	headers := make(map[string][]byte, len(r.Headers))
	for _, h := range r.Headers {
		headers[strings.ToValidUTF8(h.Key, "?")] = h.Value
	}
	reason, failure := commonv1.ErrorReason_ERROR_REASON_INTERNAL, "retries_exhausted"
	if IsPermanent(cause) {
		reason, failure = commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED, "poison"
	}
	dl := &eventsv1.DeadLetter{
		SourceTopic:     r.Topic,
		SourcePartition: r.Partition,
		SourceOffset:    r.Offset,
		Key:             r.Key,
		Value:           r.Value,
		Headers:         headers,
		ConsumerGroup:   group,
		Error: &commonv1.Error{
			Reason:   reason,
			Message:  truncate(strings.ToValidUTF8(cause.Error(), "?"), maxDeadLetterMessage),
			Domain:   ProducerName,
			Metadata: map[string]string{"failure": failure},
		},
		Attempts:       int32(min(attempts, 1<<30)),
		FirstFailedAt:  timestamppb.New(firstFailedAt),
		DeadLetteredAt: timestamppb.New(deadLetteredAt),
	}
	if err := protovalidate.Validate(dl); err != nil {
		return nil, fmt.Errorf("invalid DeadLetter: %w", err)
	}
	value, err := proto.Marshal(dl)
	if err != nil {
		return nil, fmt.Errorf("marshal DeadLetter: %w", err)
	}
	return &kgo.Record{
		Topic:   TopicDeadLetter,
		Key:     r.Key,
		Value:   value,
		Headers: []kgo.RecordHeader{{Key: HeaderContentType, Value: []byte(ContentType)}},
	}, nil
}

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// header returns the value of the record header key ("" when absent).
func header(r *kgo.Record, key string) string {
	for _, h := range r.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}
