package events

import (
	"slices"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tbilol/dxrl/services/identity/internal/domain"
)

func sampleAccount() domain.Account {
	created := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	return domain.Account{
		ID:        uuid.Must(uuid.NewV7()),
		Roles:     []commonv1.Role{domain.RoleAdmin, domain.RoleAdmin}, // duplicated
		Status:    domain.AccountActive,
		Version:   2,
		CreatedAt: created,
		UpdatedAt: created.Add(time.Hour),
	}
}

func sampleChange(event proto.Message) AccountChange {
	return AccountChange{
		Account:       sampleAccount(),
		Event:         event,
		OccurredAt:    time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC),
		CorrelationID: "req-7",
		Traceparent:   "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		Tracestate:    "vendor=1",
		Actor:         &commonv1.Actor{Kind: &commonv1.Actor_Service{Service: "auth-gateway"}},
	}
}

func TestNewAccountEvent(t *testing.T) {
	c := sampleChange(&eventsv1.AccountCreated{})
	m, err := NewAccountEvent(c)
	if err != nil {
		t.Fatal(err)
	}
	if AccountCreatedType != "agrogram.events.v1.AccountCreated" || TopicAccount != "agrogram.identity.account.v1" {
		t.Fatalf("event type = %q, topic = %q", AccountCreatedType, TopicAccount)
	}
	key := c.Account.ID.String()
	if m.Topic != TopicAccount || m.Key != key ||
		m.EventType != AccountCreatedType || m.ID.Version() != 7 || !m.CreatedAt.Equal(c.OccurredAt) {
		t.Fatalf("message = %+v", m)
	}
	wantHeaders := map[string]string{
		"content-type": "application/x-protobuf",
		"event-type":   AccountCreatedType,
		"event-id":     m.ID.String(),
		"x-request-id": "req-7",
		"traceparent":  c.Traceparent,
		"tracestate":   "vendor=1",
	}
	if len(m.Headers) != len(wantHeaders) {
		t.Fatalf("headers = %v", m.Headers)
	}
	for k, v := range wantHeaders {
		if m.Headers[k] != v {
			t.Fatalf("header %s = %q, want %q", k, m.Headers[k], v)
		}
	}

	var env eventsv1.AccountEvent
	if err := proto.Unmarshal(m.Value, &env); err != nil {
		t.Fatal(err)
	}
	if err := protovalidate.Validate(&env); err != nil {
		t.Fatalf("envelope does not validate: %v", err)
	}
	md := env.GetMetadata()
	if md.GetEventId() != m.ID.String() || md.GetEventType() != AccountCreatedType || md.GetProducer() != "identity" ||
		md.GetAggregateType() != commonv1.ResourceType_RESOURCE_TYPE_USER || md.GetAggregateId() != key ||
		md.GetAggregateVersion() != 2 || md.GetCorrelationId() != "req-7" || md.GetCausationId() != "" ||
		md.GetTraceparent() != c.Traceparent || md.GetTracestate() != "vendor=1" ||
		md.GetActor().GetService() != "auth-gateway" || !md.GetOccurredAt().AsTime().Equal(c.OccurredAt) {
		t.Fatalf("metadata = %v", md)
	}
	s := env.GetAccount()
	if s.GetId() != key || !slices.Equal(s.GetRoles(), []commonv1.Role{domain.RoleAdmin}) || s.GetVersion() != 2 ||
		!s.GetCreatedAt().AsTime().Equal(c.Account.CreatedAt) || !s.GetUpdatedAt().AsTime().Equal(c.Account.UpdatedAt) {
		t.Fatalf("snapshot = %v (roles must be sorted and distinct)", s)
	}
	if env.GetAccountCreated() == nil {
		t.Fatalf("event = %v", env.GetEvent())
	}
}

// A new account holds no role; the snapshot is valid without one.
func TestNewAccountEventWithoutRoles(t *testing.T) {
	c := sampleChange(&eventsv1.AccountCreated{})
	c.Account.Roles = nil
	m, err := NewAccountEvent(c)
	if err != nil {
		t.Fatal(err)
	}
	var env eventsv1.AccountEvent
	if err := proto.Unmarshal(m.Value, &env); err != nil || len(env.GetAccount().GetRoles()) != 0 {
		t.Fatalf("snapshot = %v, %v", env.GetAccount(), err)
	}
}

func TestNewAccountEventSetsEveryCase(t *testing.T) {
	for _, tc := range []struct {
		event    proto.Message
		wantType string
		check    func(*eventsv1.AccountEvent) bool
	}{
		{&eventsv1.AccountCreated{}, AccountCreatedType,
			func(e *eventsv1.AccountEvent) bool { return e.GetAccountCreated() != nil }},
		{&eventsv1.AccountRoleChanged{
			Roles: []commonv1.Role{domain.RoleAdmin}, GrantedRoles: []commonv1.Role{domain.RoleAdmin},
		}, AccountRoleChangedType,
			func(e *eventsv1.AccountEvent) bool { return e.GetAccountRoleChanged() != nil }},
	} {
		m, err := NewAccountEvent(sampleChange(tc.event))
		if err != nil {
			t.Fatalf("%s: %v", tc.wantType, err)
		}
		var env eventsv1.AccountEvent
		if err := proto.Unmarshal(m.Value, &env); err != nil {
			t.Fatal(err)
		}
		if m.EventType != tc.wantType || env.GetMetadata().GetEventType() != tc.wantType || !tc.check(&env) {
			t.Fatalf("%s: message %s, envelope %v", tc.wantType, m.EventType, &env)
		}
	}
}

func TestNewAccountEventValidates(t *testing.T) {
	for name, c := range map[string]AccountChange{
		"no event":            sampleChange(nil),
		"typed nil event":     sampleChange((*eventsv1.AccountCreated)(nil)),
		"not an account case": sampleChange(&eventsv1.NotificationRequested{}),
		"empty role change":   sampleChange(&eventsv1.AccountRoleChanged{Roles: []commonv1.Role{domain.RoleAdmin}}),
		"unspecified role": sampleChange(&eventsv1.AccountRoleChanged{
			GrantedRoles: []commonv1.Role{commonv1.Role_ROLE_UNSPECIFIED},
		}),
		"no correlation id": func() AccountChange {
			c := sampleChange(&eventsv1.AccountCreated{})
			c.CorrelationID = ""
			return c
		}(),
		"no actor": func() AccountChange {
			c := sampleChange(&eventsv1.AccountCreated{})
			c.Actor = nil
			return c
		}(),
		"version 0": func() AccountChange {
			c := sampleChange(&eventsv1.AccountCreated{})
			c.Account.Version = 0
			return c
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewAccountEvent(c); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}
