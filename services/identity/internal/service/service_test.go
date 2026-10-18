package service_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/tbilol/dxrl/services/identity/internal/apperr"
	"github.com/tbilol/dxrl/services/identity/internal/auth"
	"github.com/tbilol/dxrl/services/identity/internal/domain"
	"github.com/tbilol/dxrl/services/identity/internal/events"
	"github.com/tbilol/dxrl/services/identity/internal/service"
)

var start = time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)

const testTraceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

type harness struct {
	t     *testing.T
	store *fakeStore
	clock *fakeClock
	svc   *service.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, store: newFakeStore(), clock: &fakeClock{t: start}}
	h.svc = service.New(h.store, service.WithClock(h.clock.Now))
	return h
}

// newUser creates an account through CreateAccount and returns its principal.
func (h *harness) newUser() auth.Principal {
	h.t.Helper()
	a, err := h.svc.CreateAccount(context.Background(), auth.Principal{Caller: auth.CallerAuthGateway}, uuid.NewString())
	if err != nil {
		h.t.Fatalf("CreateAccount: %v", err)
	}
	return auth.Principal{UserID: a.ID, Roles: a.Roles, RequestID: uuid.NewString()}
}

// outbox returns the outbox and fails on a topic identity does not publish.
func (h *harness) outbox() []events.Message {
	h.t.Helper()
	msgs := h.store.snapshot().outbox
	for _, m := range msgs {
		if m.Topic != events.TopicAccount {
			h.t.Fatalf("outbox message on unexpected topic %q", m.Topic)
		}
	}
	return msgs
}

// accountEvents decodes the account-stream events of user, in outbox order,
// and checks that key, metadata and snapshot agree.
func (h *harness) accountEvents(user uuid.UUID) []*eventsv1.AccountEvent {
	h.t.Helper()
	var out []*eventsv1.AccountEvent
	for _, m := range h.outbox() {
		if m.Key != user.String() {
			continue
		}
		var env eventsv1.AccountEvent
		if err := proto.Unmarshal(m.Value, &env); err != nil {
			h.t.Fatalf("outbox payload: %v", err)
		}
		md := env.GetMetadata()
		if md.GetEventId() != m.ID.String() || md.GetEventType() != m.EventType || md.GetAggregateId() != m.Key ||
			env.GetAccount().GetId() != m.Key || md.GetAggregateVersion() != env.GetAccount().GetVersion() {
			h.t.Fatalf("account event %s: record, metadata and snapshot disagree: %v", m.ID, &env)
		}
		out = append(out, &env)
	}
	return out
}

// wantTypes fails unless envs have exactly the given event types, in order.
func wantTypes(t *testing.T, envs []*eventsv1.AccountEvent, want ...string) {
	t.Helper()
	got := make([]string, len(envs))
	for i, e := range envs {
		got[i] = e.GetMetadata().GetEventType()
	}
	if !slices.Equal(got, want) {
		t.Fatalf("account events = %v, want %v", got, want)
	}
}

func wantErr(t *testing.T, err error, code codes.Code, reason commonv1.ErrorReason) *apperr.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s/%s, got nil error", code, reason)
	}
	ae := apperr.From(err)
	if ae.Code != code || ae.Reason != reason {
		t.Fatalf("want %s/%s, got %s/%s (%v)", code, reason, ae.Code, ae.Reason, err)
	}
	return ae
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateAccount(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	gw := auth.Principal{Caller: auth.CallerAuthGateway, RequestID: "req-sign-in", Traceparent: testTraceparent}
	reqID := uuid.NewString()

	a, err := h.svc.CreateAccount(ctx, gw, reqID)
	mustNoErr(t, err)
	// A new account holds no role: any signed-in user may post on either board.
	if a.ID.Version() != 7 || a.Status != domain.AccountActive || a.Version != 1 || len(a.Roles) != 0 {
		t.Fatalf("new account = %+v", a)
	}
	again, err := h.svc.CreateAccount(ctx, gw, reqID)
	mustNoErr(t, err)
	if again.ID != a.ID {
		t.Fatalf("retry created a second account: %s != %s", again.ID, a.ID)
	}
	if n := len(h.store.snapshot().accounts); n != 1 {
		t.Fatalf("accounts = %d, want 1", n)
	}

	// AccountCreated once; the retry publishes nothing.
	envs := h.accountEvents(a.ID)
	wantTypes(t, envs, events.AccountCreatedType)
	md, snap := envs[0].GetMetadata(), envs[0].GetAccount()
	if envs[0].GetAccountCreated() == nil || md.GetProducer() != events.ProducerName ||
		md.GetAggregateType() != commonv1.ResourceType_RESOURCE_TYPE_USER || md.GetAggregateVersion() != 1 ||
		md.GetActor().GetUserId() != a.ID.String() || md.GetCorrelationId() != "req-sign-in" ||
		md.GetTraceparent() != testTraceparent || md.GetCausationId() != "" || !md.GetOccurredAt().AsTime().Equal(a.CreatedAt) {
		t.Fatalf("metadata = %v", md)
	}
	if len(snap.GetRoles()) != 0 || snap.GetVersion() != 1 || !snap.GetCreatedAt().AsTime().Equal(a.CreatedAt) {
		t.Fatalf("snapshot = %v", snap)
	}
	for _, p := range []auth.Principal{{UserID: a.ID}, {Caller: "catalog"}, {Caller: "profile-social"}, {}} {
		_, err := h.svc.CreateAccount(ctx, p, uuid.NewString())
		wantErr(t, err, codes.PermissionDenied, commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED)
	}
}

func TestCreateAccountWithoutRequestIDGetsFreshCorrelationID(t *testing.T) {
	h := newHarness(t)
	p := h.newUser() // CreateAccount called without x-request-id
	envs := h.accountEvents(p.UserID)
	wantTypes(t, envs, events.AccountCreatedType)
	if id, err := uuid.Parse(envs[0].GetMetadata().GetCorrelationId()); err != nil || id.Version() != 7 {
		t.Fatalf("correlation id = %q", envs[0].GetMetadata().GetCorrelationId())
	}
}

func TestReadAuthorization(t *testing.T) {
	h := newHarness(t)
	owner, other := h.newUser(), h.newUser()
	admin := auth.Principal{UserID: other.UserID, Roles: []commonv1.Role{domain.RoleAdmin}}
	missing := uuid.NewString()

	tests := []struct {
		name   string
		p      auth.Principal
		userID string
		code   codes.Code
	}{
		{"self by default", owner, "", codes.OK},
		{"self explicitly", owner, owner.UserID.String(), codes.OK},
		{"anonymous, no user id", auth.Principal{}, "", codes.Unauthenticated},
		{"anonymous, other user", auth.Principal{}, owner.UserID.String(), codes.Unauthenticated},
		{"other user", other, owner.UserID.String(), codes.PermissionDenied},
		{"admin", admin, owner.UserID.String(), codes.OK},
		{"internal caller", auth.Principal{Caller: "auth-gateway"}, owner.UserID.String(), codes.OK},
		{"missing account", auth.Principal{Caller: "auth-gateway"}, missing, codes.NotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := h.svc.GetAccount(context.Background(), tt.p, tt.userID)
			if got := apperr.From(err); tt.code == codes.OK {
				mustNoErr(t, err)
				if a.ID != owner.UserID {
					t.Fatalf("read %s, want %s", a.ID, owner.UserID)
				}
			} else if got == nil || got.Code != tt.code {
				t.Fatalf("GetAccount: want %s, got %v", tt.code, err)
			}
		})
	}
}

func TestStateAndEventsCommitTogether(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.newUser()
	h.store.update(func(db *fakeDB) { db.outboxErr = errors.New("insert into outbox: connection reset") })

	_, err := h.svc.CreateAccount(ctx, auth.Principal{Caller: auth.CallerAuthGateway}, uuid.NewString())
	wantErr(t, err, codes.Internal, commonv1.ErrorReason_ERROR_REASON_INTERNAL)
	if n := len(h.store.snapshot().accounts); n != 1 {
		t.Fatalf("an account was committed without AccountCreated: %d accounts", n)
	}
}
