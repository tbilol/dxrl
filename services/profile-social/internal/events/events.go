// Package events is profile-social's Kafka side: it relays the transactional
// outbox to Kafka and consumes identity's account stream.
//
// Published (outbox table -> Relay): agrogram.profilesocial.profile.v1
// (ProfileEvent, profile.proto), keyed by the user id. profile-social
// publishes no event case today (RatingUpdated, the only one, went with
// ratings); the relay drains rows queued before that and stays for later
// profile events.
//
// Consumed (Consumer, consumer group "profilesocial" by default):
//
//   - agrogram.identity.account.v1: AccountEvent (account.proto), applied to
//     the account projection on profiles by AccountHandler.
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
)

// Kafka conventions (proto/events/v1/metadata.proto).
const (
	TopicProfile    = "agrogram.profilesocial.profile.v1"
	TopicAccount    = "agrogram.identity.account.v1"
	TopicDeadLetter = "agrogram.profilesocial.deadletter.v1"
	ProducerName    = "profile-social"
	ContentType     = "application/x-protobuf"

	HeaderContentType = "content-type"
	HeaderEventType   = "event-type"
	HeaderEventID     = "event-id"
	HeaderRequestID   = "x-request-id"
	HeaderTraceparent = "traceparent"
	HeaderTracestate  = "tracestate"
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
