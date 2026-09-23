package events

import (
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"google.golang.org/protobuf/proto"
)

func sampleNotification() Notification {
	return Notification{
		RecipientID: uuid.New(),
		Category:    eventsv1.NotificationCategory_NOTIFICATION_CATEGORY_ACCOUNT,
		TemplateKey: TemplateFarmerVerified,
		Params:      map[string]string{"lease_contract_number": "AB-1"},
		Subject: &commonv1.ResourceRef{
			Type: commonv1.ResourceType_RESOURCE_TYPE_FARMER_VERIFICATION,
			Id:   uuid.NewString(),
		},
		OccurredAt:    time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC),
		CorrelationID: "req-1",
		Traceparent:   "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		Actor:         &commonv1.Actor{Kind: &commonv1.Actor_Service{Service: "identity"}},
	}
}

func TestNewNotificationRequest(t *testing.T) {
	n := sampleNotification()
	m, err := NewNotificationRequest(n)
	if err != nil {
		t.Fatal(err)
	}
	if NotificationRequestedType != "agrogram.events.v1.NotificationRequested" {
		t.Fatalf("event type = %q", NotificationRequestedType)
	}
	if m.Topic != TopicNotificationRequest || m.Key != n.RecipientID.String() || m.EventType != NotificationRequestedType ||
		m.ID.Version() != 7 || !m.CreatedAt.Equal(n.OccurredAt) {
		t.Fatalf("message = %+v", m)
	}
	wantHeaders := map[string]string{
		"content-type": "application/x-protobuf",
		"event-type":   NotificationRequestedType,
		"event-id":     m.ID.String(),
		"x-request-id": "req-1",
		"traceparent":  n.Traceparent,
	}
	if len(m.Headers) != len(wantHeaders) {
		t.Fatalf("headers = %v", m.Headers)
	}
	for k, v := range wantHeaders {
		if m.Headers[k] != v {
			t.Fatalf("header %s = %q, want %q", k, m.Headers[k], v)
		}
	}

	var env eventsv1.NotificationRequestEvent
	if err := proto.Unmarshal(m.Value, &env); err != nil {
		t.Fatal(err)
	}
	if err := protovalidate.Validate(&env); err != nil {
		t.Fatalf("envelope does not validate: %v", err)
	}
	md, req := env.GetMetadata(), env.GetNotificationRequested()
	if md.GetEventId() != m.ID.String() || md.GetEventType() != NotificationRequestedType || md.GetProducer() != "identity" ||
		md.GetAggregateType() != commonv1.ResourceType_RESOURCE_TYPE_USER || md.GetAggregateId() != n.RecipientID.String() ||
		md.GetAggregateVersion() != 0 || md.GetCorrelationId() != "req-1" || md.GetTraceparent() != n.Traceparent ||
		md.GetActor().GetService() != "identity" || !md.GetOccurredAt().AsTime().Equal(n.OccurredAt) {
		t.Fatalf("metadata = %v", md)
	}
	if req.GetRecipientId() != n.RecipientID.String() || req.GetTemplateKey() != TemplateFarmerVerified ||
		req.GetParams()["lease_contract_number"] != "AB-1" || len(req.GetChannels()) != 0 ||
		req.GetSubject().GetId() != n.Subject.GetId() {
		t.Fatalf("payload = %v", req)
	}
}

func TestNewNotificationRequestValidates(t *testing.T) {
	for name, mutate := range map[string]func(*Notification){
		"bad template key":  func(n *Notification) { n.TemplateKey = "Account Verified" },
		"no correlation id": func(n *Notification) { n.CorrelationID = "" },
		"no actor":          func(n *Notification) { n.Actor = nil },
		"unspecified category": func(n *Notification) {
			n.Category = eventsv1.NotificationCategory_NOTIFICATION_CATEGORY_UNSPECIFIED
		},
	} {
		t.Run(name, func(t *testing.T) {
			n := sampleNotification()
			mutate(&n)
			if _, err := NewNotificationRequest(n); err == nil {
				t.Fatal("want a validation error")
			}
		})
	}
}
