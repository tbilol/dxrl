package events

import (
	"testing"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	testNow         = time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	testTraceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
)

// accountEvent is a valid AccountEvent as identity publishes it: the snapshot
// of user at version, with event as the oneof case (nil: no case, as a
// consumer on an older schema sees a newer one).
func accountEvent(user uuid.UUID, version int64, event proto.Message, roles ...commonv1.Role) *eventsv1.AccountEvent {
	eventType := "agrogram.events.v1.AccountFrozen" // a case newer than this code
	if event != nil {
		eventType = string(proto.MessageName(event))
	}
	env := &eventsv1.AccountEvent{
		Metadata: &eventsv1.EventMetadata{
			EventId:          uuid.Must(uuid.NewV7()).String(),
			EventType:        eventType,
			OccurredAt:       timestamppb.New(testNow),
			Producer:         "identity",
			AggregateType:    commonv1.ResourceType_RESOURCE_TYPE_USER,
			AggregateId:      user.String(),
			AggregateVersion: version,
			CorrelationId:    "req-1",
			Traceparent:      testTraceparent,
			Actor:            &commonv1.Actor{Kind: &commonv1.Actor_UserId{UserId: user.String()}},
		},
		Account: &eventsv1.AccountSnapshot{
			Id:        user.String(),
			Roles:     roles,
			Version:   version,
			CreatedAt: timestamppb.New(testNow.Add(-time.Hour)),
			UpdatedAt: timestamppb.New(testNow),
		},
	}
	switch e := event.(type) {
	case *eventsv1.AccountCreated:
		env.Event = &eventsv1.AccountEvent_AccountCreated{AccountCreated: e}
	case *eventsv1.AccountRoleChanged:
		env.Event = &eventsv1.AccountEvent_AccountRoleChanged{AccountRoleChanged: e}
	}
	return env
}

// record wraps an envelope into a Kafka record the way the relays publish it.
func record(t *testing.T, topic string, env proto.Message, key string) *kgo.Record {
	t.Helper()
	value, err := proto.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return &kgo.Record{
		Topic: topic,
		Key:   []byte(key),
		Value: value,
		Headers: []kgo.RecordHeader{
			{Key: HeaderContentType, Value: []byte(ContentType)},
			{Key: HeaderEventID, Value: []byte(eventID(env))},
		},
	}
}

func eventID(env proto.Message) string {
	switch e := env.(type) {
	case *eventsv1.AccountEvent:
		return e.GetMetadata().GetEventId()
	}
	return ""
}

func timestampOf(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }
