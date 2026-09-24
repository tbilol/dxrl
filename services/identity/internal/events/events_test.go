package events

import (
	"slices"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	identityv1 "github.com/tbilol/dxrl/gen/go/identity/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tbilol/dxrl/services/identity/internal/domain"
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

func sampleAccount() domain.Account {
	created := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	return domain.Account{
		ID:        uuid.Must(uuid.NewV7()),
		Roles:     []commonv1.Role{domain.RoleFarmer, domain.RoleReseller, domain.RoleFarmer}, // unsorted, duplicated
		Status:    domain.AccountActive,
		Plan:      domain.PlanDefault,
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
		Actor:         &commonv1.Actor{Kind: &commonv1.Actor_Service{Service: "order-escrow"}},
	}
}

func TestNewAccountEvent(t *testing.T) {
	c := sampleChange(&eventsv1.AccountPremiumChanged{IsPremium: false})
	m, err := NewAccountEvent(c)
	if err != nil {
		t.Fatal(err)
	}
	if AccountPremiumChangedType != "agrogram.events.v1.AccountPremiumChanged" || TopicAccount != "agrogram.identity.account.v1" {
		t.Fatalf("event type = %q, topic = %q", AccountPremiumChangedType, TopicAccount)
	}
	key := c.Account.ID.String()
	if m.Topic != TopicAccount || m.Key != key ||
		m.EventType != AccountPremiumChangedType || m.ID.Version() != 7 || !m.CreatedAt.Equal(c.OccurredAt) {
		t.Fatalf("message = %+v", m)
	}
	wantHeaders := map[string]string{
		"content-type": "application/x-protobuf",
		"event-type":   AccountPremiumChangedType,
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
	if md.GetEventId() != m.ID.String() || md.GetEventType() != AccountPremiumChangedType || md.GetProducer() != "identity" ||
		md.GetAggregateType() != commonv1.ResourceType_RESOURCE_TYPE_USER || md.GetAggregateId() != key ||
		md.GetAggregateVersion() != 2 || md.GetCorrelationId() != "req-7" || md.GetCausationId() != "" ||
		md.GetTraceparent() != c.Traceparent || md.GetTracestate() != "vendor=1" ||
		md.GetActor().GetService() != "order-escrow" || !md.GetOccurredAt().AsTime().Equal(c.OccurredAt) {
		t.Fatalf("metadata = %v", md)
	}
	s := env.GetAccount()
	if s.GetId() != key || !slices.Equal(s.GetRoles(), []commonv1.Role{domain.RoleReseller, domain.RoleFarmer}) ||
		!s.GetIsVerifiedFarmer() || s.GetIsPremium() || s.GetPremiumUntil() != nil || s.GetVersion() != 2 ||
		!s.GetCreatedAt().AsTime().Equal(c.Account.CreatedAt) || !s.GetUpdatedAt().AsTime().Equal(c.Account.UpdatedAt) {
		t.Fatalf("snapshot = %v", s)
	}
	if env.GetAccountPremiumChanged() == nil || env.GetAccountPremiumChanged().GetIsPremium() {
		t.Fatalf("event = %v", env.GetEvent())
	}
}

func TestNewAccountEventSetsEveryCase(t *testing.T) {
	verificationID := uuid.New()
	date := &commonv1.Date{Year: 2027, Month: 9, Day: 22}
	for _, tc := range []struct {
		event proto.Message
		want  string
		check func(*eventsv1.AccountEvent) bool
	}{
		{&eventsv1.AccountCreated{}, AccountCreatedType,
			func(e *eventsv1.AccountEvent) bool { return e.GetAccountCreated() != nil }},
		{RoleChange([]commonv1.Role{domain.RoleReseller}, []commonv1.Role{domain.RoleReseller, domain.RoleFarmer}, verificationID),
			AccountRoleChangedType,
			func(e *eventsv1.AccountEvent) bool { return e.GetAccountRoleChanged() != nil }},
		{&eventsv1.AccountPremiumChanged{IsPremium: true}, AccountPremiumChangedType,
			func(e *eventsv1.AccountEvent) bool { return e.GetAccountPremiumChanged().GetIsPremium() }},
		{&eventsv1.FarmerVerified{VerificationId: verificationID.String(), LeaseValidFrom: date, LeaseValidUntil: date},
			FarmerVerifiedType,
			func(e *eventsv1.AccountEvent) bool { return e.GetFarmerVerified() != nil }},
		{&eventsv1.FarmerVerificationRejected{VerificationId: verificationID.String(), RejectionReason: domain.ReasonContractClaimed},
			FarmerVerificationRejectedType,
			func(e *eventsv1.AccountEvent) bool { return e.GetFarmerVerificationRejected() != nil }},
		{&eventsv1.FarmerVerificationExpired{VerificationId: verificationID.String(), LeaseValidUntil: date},
			FarmerVerificationExpiredType,
			func(e *eventsv1.AccountEvent) bool { return e.GetFarmerVerificationExpired() != nil }},
	} {
		t.Run(tc.want, func(t *testing.T) {
			m, err := NewAccountEvent(sampleChange(tc.event))
			if err != nil {
				t.Fatal(err)
			}
			var env eventsv1.AccountEvent
			if err := proto.Unmarshal(m.Value, &env); err != nil {
				t.Fatal(err)
			}
			if m.EventType != tc.want || m.Headers[HeaderEventType] != tc.want || env.GetMetadata().GetEventType() != tc.want || !tc.check(&env) {
				t.Fatalf("type %q, event %v", m.EventType, env.GetEvent())
			}
		})
	}
}

func TestNewAccountEventValidates(t *testing.T) {
	date := &commonv1.Date{Year: 2027, Month: 9, Day: 22}
	for name, c := range map[string]AccountChange{
		"no event":            sampleChange(nil),
		"typed nil event":     sampleChange((*eventsv1.AccountCreated)(nil)),
		"not an account case": sampleChange(&eventsv1.NotificationRequested{}),
		"empty role change":   sampleChange(&eventsv1.AccountRoleChanged{Roles: []commonv1.Role{domain.RoleReseller}}),
		"unspecified role": sampleChange(&eventsv1.AccountRoleChanged{
			GrantedRoles: []commonv1.Role{commonv1.Role_ROLE_UNSPECIFIED},
		}),
		"rejection reason outside the lease reasons": sampleChange(&eventsv1.FarmerVerificationRejected{
			VerificationId: uuid.NewString(), RejectionReason: commonv1.ErrorReason_ERROR_REASON_INTERNAL,
		}),
		"verified without lease dates": sampleChange(&eventsv1.FarmerVerified{VerificationId: uuid.NewString()}),
		"expired with a bad id":        sampleChange(&eventsv1.FarmerVerificationExpired{VerificationId: "x", LeaseValidUntil: date}),
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

func TestAccountSnapshotPremium(t *testing.T) {
	at := time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC)
	until := at.Add(24 * time.Hour)
	ended := at.Add(-time.Hour)
	for name, tc := range map[string]struct {
		plan        identityv1.Plan
		until       *time.Time
		wantPremium bool
		wantUntil   bool
	}{
		"default plan":          {domain.PlanDefault, nil, false, false},
		"premium, no end date":  {domain.PlanPremium, nil, true, false},
		"premium, running":      {domain.PlanPremium, &until, true, true},
		"premium, period ended": {domain.PlanPremium, &ended, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			a := sampleAccount()
			a.Plan, a.PremiumUntil = tc.plan, tc.until
			s := accountSnapshot(a, at)
			if s.GetIsPremium() != tc.wantPremium || (s.GetPremiumUntil() != nil) != tc.wantUntil ||
				(tc.wantUntil && !s.GetPremiumUntil().AsTime().Equal(until)) {
				t.Fatalf("snapshot = %v", s)
			}
		})
	}
}

func TestRoleChange(t *testing.T) {
	reseller, farmer, admin := domain.RoleReseller, domain.RoleFarmer, domain.RoleAdmin
	id := uuid.New()

	if e := RoleChange([]commonv1.Role{farmer, reseller}, []commonv1.Role{reseller, farmer, farmer}, id); e != nil {
		t.Fatalf("same set, different order: %v", e)
	}

	e := RoleChange([]commonv1.Role{reseller, farmer}, []commonv1.Role{admin, reseller}, id)
	if !slices.Equal(e.GetPreviousRoles(), []commonv1.Role{reseller, farmer}) ||
		!slices.Equal(e.GetRoles(), []commonv1.Role{reseller, admin}) ||
		!slices.Equal(e.GetGrantedRoles(), []commonv1.Role{admin}) ||
		!slices.Equal(e.GetRevokedRoles(), []commonv1.Role{farmer}) || e.GetFarmerVerificationId() != id.String() {
		t.Fatalf("change = %v", e)
	}

	if e := RoleChange(nil, []commonv1.Role{reseller}, uuid.Nil); len(e.GetGrantedRoles()) != 1 || e.GetFarmerVerificationId() != "" {
		t.Fatalf("change without a verification = %v", e)
	}
}

func TestVerificationEvent(t *testing.T) {
	from := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	lease := &domain.Lease{ContractNumber: "AB-1", ValidFrom: from, ValidUntil: from.AddDate(1, 0, 0)}
	v := domain.Verification{ID: uuid.New(), UserID: uuid.New(), ContractNumber: "AB-1", Lease: lease}

	v.Status = domain.VerificationVerified
	e, err := VerificationEvent(v)
	fv, ok := e.(*eventsv1.FarmerVerified)
	if err != nil || !ok || fv.GetVerificationId() != v.ID.String() || fv.GetLeaseValidFrom().GetYear() != 2026 ||
		fv.GetLeaseValidUntil().GetYear() != 2027 || fv.GetLeaseValidUntil().GetMonth() != 9 || fv.GetLeaseValidUntil().GetDay() != 22 {
		t.Fatalf("verified: %v, %v", e, err)
	}

	v.Status, v.RejectionReason, v.Lease = domain.VerificationRejected, domain.ReasonContractInvalid, nil
	e, err = VerificationEvent(v)
	if r, ok := e.(*eventsv1.FarmerVerificationRejected); err != nil || !ok || r.GetRejectionReason() != domain.ReasonContractInvalid {
		t.Fatalf("rejected: %v, %v", e, err)
	}

	v.Status, v.RejectionReason, v.Lease = domain.VerificationExpired, 0, lease
	e, err = VerificationEvent(v)
	if x, ok := e.(*eventsv1.FarmerVerificationExpired); err != nil || !ok || x.GetLeaseValidUntil().GetYear() != 2027 {
		t.Fatalf("expired: %v, %v", e, err)
	}

	for _, bad := range []domain.Verification{
		{ID: v.ID, Status: domain.VerificationPending},
		{ID: v.ID, Status: domain.VerificationVerified}, // no lease
		{ID: v.ID, Status: domain.VerificationExpired},  // no lease
	} {
		if e, err := VerificationEvent(bad); err == nil {
			t.Fatalf("%s: want an error, got %v", bad.Status, e)
		}
	}
}
