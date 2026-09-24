package service_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
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
	"github.com/tbilol/dxrl/services/identity/internal/eijara"
	"github.com/tbilol/dxrl/services/identity/internal/events"
	"github.com/tbilol/dxrl/services/identity/internal/service"
)

// 2026-09-22 15:00 in Asia/Tashkent (UTC+5).
var start = time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)

const testTraceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

type harness struct {
	t       *testing.T
	store   *fakeStore
	clock   *fakeClock
	limiter *fakeLimiter
	checker atomic.Value // checkerBox
	checks  atomic.Int32
	svc     *service.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, store: newFakeStore(), clock: &fakeClock{t: start}, limiter: &fakeLimiter{}}
	h.useChecker(eijara.Stub{Now: h.clock.Now})
	checker := checkerFunc(func(ctx context.Context, n string) (eijara.Result, error) {
		h.checks.Add(1)
		return h.checker.Load().(checkerBox).Check(ctx, n)
	})
	h.svc = service.New(h.store, checker, h.limiter, service.Config{
		EIjaraTimeout:   50 * time.Millisecond,
		RetryBackoff:    30 * time.Second,
		RetryBackoffMax: time.Hour,
		WorkerBatchSize: 10,
	}, service.WithClock(h.clock.Now))
	return h
}

// checkerBox gives atomic.Value one concrete type for any checker.
type checkerBox struct{ service.EIjaraChecker }

func (h *harness) useChecker(c service.EIjaraChecker) { h.checker.Store(checkerBox{c}) }

// newUser creates an account through CreateAccount and returns its principal.
func (h *harness) newUser() auth.Principal {
	h.t.Helper()
	a, err := h.svc.CreateAccount(context.Background(), auth.Principal{Caller: auth.CallerAuthGateway}, uuid.NewString())
	if err != nil {
		h.t.Fatalf("CreateAccount: %v", err)
	}
	return auth.Principal{UserID: a.ID, Roles: a.Roles, RequestID: uuid.NewString()}
}

func (h *harness) submit(p auth.Principal, contract string) (domain.Verification, error) {
	return h.svc.SubmitFarmerVerification(context.Background(), p, uuid.NewString(), contract)
}

func (h *harness) account(id uuid.UUID) domain.Account {
	h.t.Helper()
	a, err := h.svc.GetAccount(context.Background(), auth.Principal{Caller: "test"}, id.String())
	if err != nil {
		h.t.Fatalf("GetAccount: %v", err)
	}
	return a
}

// outbox returns the outbox and fails on a topic identity does not publish.
func (h *harness) outbox() []events.Message {
	h.t.Helper()
	msgs := h.store.snapshot().outbox
	for _, m := range msgs {
		if m.Topic != events.TopicAccount && m.Topic != events.TopicNotificationRequest {
			h.t.Fatalf("outbox message on unexpected topic %q", m.Topic)
		}
	}
	return msgs
}

// notifications decodes the notification requests in the outbox.
func (h *harness) notifications() []*eventsv1.NotificationRequestEvent {
	h.t.Helper()
	var out []*eventsv1.NotificationRequestEvent
	for _, m := range h.outbox() {
		if m.Topic != events.TopicNotificationRequest {
			continue
		}
		var env eventsv1.NotificationRequestEvent
		if err := proto.Unmarshal(m.Value, &env); err != nil {
			h.t.Fatalf("outbox payload: %v", err)
		}
		if m.Key != env.GetNotificationRequested().GetRecipientId() {
			h.t.Fatalf("outbox key = %s", m.Key)
		}
		out = append(out, &env)
	}
	return out
}

// accountEvents decodes the account-stream events of user, in outbox order,
// and checks that key, metadata and snapshot agree.
func (h *harness) accountEvents(user uuid.UUID) []*eventsv1.AccountEvent {
	h.t.Helper()
	var out []*eventsv1.AccountEvent
	for _, m := range h.outbox() {
		if m.Topic != events.TopicAccount || m.Key != user.String() {
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
	if a.ID.Version() != 7 || a.Status != domain.AccountActive || a.Plan != domain.PlanDefault ||
		a.Version != 1 || !slices.Equal(a.Roles, []commonv1.Role{domain.RoleReseller}) || a.IsPremium {
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
	if !slices.Equal(snap.GetRoles(), []commonv1.Role{domain.RoleReseller}) || snap.GetIsVerifiedFarmer() ||
		snap.GetIsPremium() || snap.GetPremiumUntil() != nil || snap.GetVersion() != 1 ||
		!snap.GetCreatedAt().AsTime().Equal(a.CreatedAt) {
		t.Fatalf("snapshot = %v", snap)
	}
	if n := len(h.notifications()); n != 0 {
		t.Fatalf("CreateAccount requested %d notifications", n)
	}

	for _, p := range []auth.Principal{{UserID: a.ID}, {Caller: "catalog"}, {Caller: auth.CallerOrderEscrow}, {}} {
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
	admin := auth.Principal{UserID: other.UserID, Roles: []commonv1.Role{domain.RoleReseller, domain.RoleAdmin}}
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
		{"internal caller", auth.Principal{Caller: "order-escrow"}, owner.UserID.String(), codes.OK},
		{"missing account", auth.Principal{Caller: "order-escrow"}, missing, codes.NotFound},
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
			// GetFarmerVerification applies the same rules; with nothing
			// submitted an allowed read is NOT_FOUND.
			_, err = h.svc.GetFarmerVerification(context.Background(), tt.p, tt.userID)
			want := tt.code
			if want == codes.OK {
				want = codes.NotFound
			}
			if got := apperr.From(err); got == nil || got.Code != want {
				t.Fatalf("GetFarmerVerification: want %s, got %v", want, err)
			}
		})
	}
}

func TestSubmitVerifiedGrantsFarmer(t *testing.T) {
	h := newHarness(t)
	p := h.newUser()
	p.Traceparent = testTraceparent

	v, err := h.submit(p, "  ab-123/45 ")
	mustNoErr(t, err)
	if v.Status != domain.VerificationVerified || v.ContractNumber != "AB-123/45" || v.DecidedAt == nil || v.Lease == nil {
		t.Fatalf("verification = %+v", v)
	}
	wantFrom, wantUntil := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC), time.Date(2027, 9, 22, 0, 0, 0, 0, time.UTC)
	if !v.Lease.ValidFrom.Equal(wantFrom) || !v.Lease.ValidUntil.Equal(wantUntil) {
		t.Fatalf("lease = %s .. %s", v.Lease.ValidFrom, v.Lease.ValidUntil)
	}

	a := h.account(p.UserID)
	if !a.HasRole(domain.RoleFarmer) || !a.HasRole(domain.RoleReseller) || a.Version != 2 ||
		a.FarmerVerificationStatus != domain.VerificationVerified {
		t.Fatalf("account = %+v", a)
	}

	got, err := h.svc.GetFarmerVerification(context.Background(), p, "")
	mustNoErr(t, err)
	if got.ID != v.ID || got.Status != domain.VerificationVerified {
		t.Fatalf("GetFarmerVerification = %+v", got)
	}

	envs := h.notifications()
	if len(envs) != 1 {
		t.Fatalf("outbox has %d notification requests, want 1", len(envs))
	}
	md, n := envs[0].GetMetadata(), envs[0].GetNotificationRequested()
	if n.GetTemplateKey() != events.TemplateFarmerVerified || n.GetParams()["lease_contract_number"] != "AB-123/45" ||
		n.GetCategory() != eventsv1.NotificationCategory_NOTIFICATION_CATEGORY_ACCOUNT ||
		n.GetSubject().GetId() != v.ID.String() || n.GetRecipientId() != p.UserID.String() {
		t.Fatalf("notification = %v", n)
	}
	if md.GetActor().GetUserId() != p.UserID.String() || md.GetCorrelationId() != p.RequestID ||
		md.GetTraceparent() != p.Traceparent || !md.GetOccurredAt().AsTime().Equal(*v.DecidedAt) {
		t.Fatalf("metadata = %v", md)
	}

	// Account stream: the verification fact, then the role change it caused.
	acctEnvs := h.accountEvents(p.UserID)
	wantTypes(t, acctEnvs, events.AccountCreatedType, events.FarmerVerifiedType, events.AccountRoleChangedType)
	verified, roleChanged := acctEnvs[1], acctEnvs[2]
	fv := verified.GetFarmerVerified()
	if fv.GetVerificationId() != v.ID.String() || fv.GetLeaseValidFrom().GetYear() != 2026 ||
		fv.GetLeaseValidFrom().GetMonth() != 9 || fv.GetLeaseValidFrom().GetDay() != 22 ||
		fv.GetLeaseValidUntil().GetYear() != 2027 || fv.GetLeaseValidUntil().GetMonth() != 9 ||
		fv.GetLeaseValidUntil().GetDay() != 22 {
		t.Fatalf("FarmerVerified = %v", fv)
	}
	// The verification alone does not change the account.
	if s := verified.GetAccount(); s.GetVersion() != 1 || s.GetIsVerifiedFarmer() ||
		!slices.Equal(s.GetRoles(), []commonv1.Role{domain.RoleReseller}) {
		t.Fatalf("FarmerVerified snapshot = %v", s)
	}
	rc := roleChanged.GetAccountRoleChanged()
	if !slices.Equal(rc.GetPreviousRoles(), []commonv1.Role{domain.RoleReseller}) ||
		!slices.Equal(rc.GetRoles(), []commonv1.Role{domain.RoleReseller, domain.RoleFarmer}) ||
		!slices.Equal(rc.GetGrantedRoles(), []commonv1.Role{domain.RoleFarmer}) || len(rc.GetRevokedRoles()) != 0 ||
		rc.GetFarmerVerificationId() != v.ID.String() {
		t.Fatalf("AccountRoleChanged = %v", rc)
	}
	if s := roleChanged.GetAccount(); s.GetVersion() != 2 || !s.GetIsVerifiedFarmer() || !slices.Equal(s.GetRoles(), rc.GetRoles()) {
		t.Fatalf("AccountRoleChanged snapshot = %v", s)
	}
	for _, e := range acctEnvs[1:] {
		md := e.GetMetadata()
		if md.GetActor().GetUserId() != p.UserID.String() || md.GetCorrelationId() != p.RequestID ||
			md.GetTraceparent() != p.Traceparent || !md.GetOccurredAt().AsTime().Equal(*v.DecidedAt) {
			t.Fatalf("account event metadata = %v", md)
		}
	}
}

func TestSubmitRejectedByStubHook(t *testing.T) {
	h := newHarness(t)
	p := h.newUser()

	v, err := h.submit(p, "invalid-42")
	mustNoErr(t, err)
	if v.Status != domain.VerificationRejected || v.RejectionReason != domain.ReasonContractInvalid || v.Lease != nil {
		t.Fatalf("verification = %+v", v)
	}
	a := h.account(p.UserID)
	if a.HasRole(domain.RoleFarmer) || a.Version != 1 || a.FarmerVerificationStatus != domain.VerificationRejected {
		t.Fatalf("account = %+v", a)
	}
	envs := h.notifications()
	if len(envs) != 1 {
		t.Fatalf("outbox has %d notification requests, want 1", len(envs))
	}
	n := envs[0].GetNotificationRequested()
	if n.GetTemplateKey() != events.TemplateFarmerVerificationRejected ||
		n.GetParams()["rejection_reason"] != "ERROR_REASON_LEASE_CONTRACT_INVALID" {
		t.Fatalf("notification = %v", n)
	}
	// A rejection is published, but roles do not change.
	acctEnvs := h.accountEvents(p.UserID)
	wantTypes(t, acctEnvs, events.AccountCreatedType, events.FarmerVerificationRejectedType)
	if r := acctEnvs[1].GetFarmerVerificationRejected(); r.GetVerificationId() != v.ID.String() ||
		r.GetRejectionReason() != domain.ReasonContractInvalid || acctEnvs[1].GetAccount().GetVersion() != 1 {
		t.Fatalf("FarmerVerificationRejected = %v", acctEnvs[1])
	}

	// A rejection does not block another attempt.
	v2, err := h.submit(p, "AB-7")
	mustNoErr(t, err)
	if v2.Status != domain.VerificationVerified {
		t.Fatalf("second attempt = %+v", v2)
	}
	wantTypes(t, h.accountEvents(p.UserID), events.AccountCreatedType, events.FarmerVerificationRejectedType,
		events.FarmerVerifiedType, events.AccountRoleChangedType)
}

func TestSubmitAlreadyVerified(t *testing.T) {
	h := newHarness(t)
	p := h.newUser()
	_, err := h.submit(p, "AB-1")
	mustNoErr(t, err)
	_, err = h.submit(p, "AB-2")
	wantErr(t, err, codes.AlreadyExists, commonv1.ErrorReason_ERROR_REASON_ALREADY_EXISTS)
}

func TestSubmitContractClaimedByAnotherAccount(t *testing.T) {
	h := newHarness(t)
	a, b := h.newUser(), h.newUser()
	_, err := h.submit(a, "AB-1")
	mustNoErr(t, err)

	_, err = h.submit(b, "ab-1")
	wantErr(t, err, codes.AlreadyExists, commonv1.ErrorReason_ERROR_REASON_LEASE_CONTRACT_ALREADY_CLAIMED)
	_, err = h.svc.GetFarmerVerification(context.Background(), b, "")
	wantErr(t, err, codes.NotFound, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND) // nothing stored
	wantTypes(t, h.accountEvents(b.UserID), events.AccountCreatedType)           // nothing published
}

func TestSubmitIdempotentRequestID(t *testing.T) {
	h := newHarness(t)
	p := h.newUser()
	reqID := uuid.NewString()

	first, err := h.svc.SubmitFarmerVerification(context.Background(), p, reqID, "AB-1")
	mustNoErr(t, err)
	// The retry returns the original (now VERIFIED) verification instead of
	// ALREADY_EXISTS, and neither calls e-ijara nor counts against the limit.
	second, err := h.svc.SubmitFarmerVerification(context.Background(), p, reqID, "AB-1")
	mustNoErr(t, err)
	if second.ID != first.ID || second.Status != domain.VerificationVerified {
		t.Fatalf("retry = %+v, want %s", second, first.ID)
	}
	if n := h.checks.Load(); n != 1 {
		t.Fatalf("e-ijara called %d times, want 1", n)
	}
	if h.limiter.calls != 1 {
		t.Fatalf("rate limiter called %d times, want 1", h.limiter.calls)
	}
	if n := len(h.notifications()); n != 1 {
		t.Fatalf("outbox has %d notification requests, want 1", n)
	}
	wantTypes(t, h.accountEvents(p.UserID), events.AccountCreatedType, events.FarmerVerifiedType, events.AccountRoleChangedType)
}

func TestSubmitRateLimit(t *testing.T) {
	h := newHarness(t)
	p := h.newUser()

	h.limiter.deny, h.limiter.retryAfter = true, 89500*time.Millisecond
	_, err := h.submit(p, "AB-1")
	ae := wantErr(t, err, codes.ResourceExhausted, commonv1.ErrorReason_ERROR_REASON_RATE_LIMITED)
	if ae.Metadata["retry_after_seconds"] != "90" {
		t.Fatalf("metadata = %v", ae.Metadata)
	}

	// Redis down: fail open.
	h.limiter.deny, h.limiter.err = false, errors.New("redis: connection refused")
	v, err := h.submit(p, "AB-1")
	mustNoErr(t, err)
	if v.Status != domain.VerificationVerified {
		t.Fatalf("verification = %+v", v)
	}
}

func TestSubmitPreconditions(t *testing.T) {
	h := newHarness(t)
	p := h.newUser()

	_, err := h.submit(auth.Principal{}, "AB-1")
	wantErr(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED)

	_, err = h.submit(auth.Principal{UserID: uuid.New()}, "AB-1")
	wantErr(t, err, codes.NotFound, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)

	for _, bad := range []string{"!!!", "a b c", "-AB1", "AB", "AB_12"} {
		_, err = h.submit(p, bad)
		ae := wantErr(t, err, codes.InvalidArgument, commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED)
		if len(ae.Violations) != 1 || ae.Violations[0].GetField() != "lease_contract_number" {
			t.Fatalf("%q: violations = %v", bad, ae.Violations)
		}
	}

	h.store.update(func(db *fakeDB) {
		a := db.accounts[p.UserID]
		a.Status = domain.AccountBlocked
		db.accounts[p.UserID] = a
	})
	_, err = h.submit(p, "AB-1")
	wantErr(t, err, codes.PermissionDenied, commonv1.ErrorReason_ERROR_REASON_ACCOUNT_DISABLED)
}

func TestTimeoutLeavesPendingThenWorkerDecides(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.newUser()

	h.useChecker(blockingChecker)
	v, err := h.submit(p, "AB-1")
	mustNoErr(t, err)
	if v.Status != domain.VerificationPending || v.DecidedAt != nil {
		t.Fatalf("verification = %+v", v)
	}
	if len(h.notifications()) != 0 || h.account(p.UserID).HasRole(domain.RoleFarmer) {
		t.Fatal("a PENDING verification must not notify or grant")
	}
	wantTypes(t, h.accountEvents(p.UserID), events.AccountCreatedType) // nothing decided yet

	// Not due yet: the inline call keeps the worker away for RetryBackoff.
	n, err := h.svc.RetryPending(ctx)
	mustNoErr(t, err)
	if n != 0 {
		t.Fatalf("decided %d, want 0", n)
	}

	// Due, e-ijara still down: an attempt is counted and backed off.
	h.clock.Advance(31 * time.Second)
	n, err = h.svc.RetryPending(ctx)
	mustNoErr(t, err)
	db := h.store.snapshot()
	if got := db.verifications[v.ID]; n != 0 || got.Attempts != 1 || got.Status != domain.VerificationPending ||
		!got.NextAttemptAt.Equal(h.clock.Now().Add(30*time.Second)) || db.lastErrors[v.ID] == "" {
		t.Fatalf("after a failed retry: n=%d verification=%+v last_error=%q", n, got, db.lastErrors[v.ID])
	}

	// e-ijara is back: the worker decides.
	h.useChecker(eijara.Stub{Now: h.clock.Now})
	h.clock.Advance(31 * time.Second)
	n, err = h.svc.RetryPending(ctx)
	mustNoErr(t, err)
	if n != 1 {
		t.Fatalf("decided %d, want 1", n)
	}
	got, err := h.svc.GetFarmerVerification(ctx, p, "")
	mustNoErr(t, err)
	if got.Status != domain.VerificationVerified || !h.account(p.UserID).HasRole(domain.RoleFarmer) {
		t.Fatalf("after worker: %+v", got)
	}
	envs := h.notifications()
	if len(envs) != 1 {
		t.Fatalf("outbox has %d notification requests, want 1", len(envs))
	}
	md := envs[0].GetMetadata()
	if md.GetActor().GetService() != auth.ServiceName || md.GetCorrelationId() == p.RequestID ||
		uuid.Validate(md.GetCorrelationId()) != nil {
		t.Fatalf("worker event metadata = %v", md)
	}
	// The worker's account events share the notification's correlation id.
	acctEnvs := h.accountEvents(p.UserID)
	wantTypes(t, acctEnvs, events.AccountCreatedType, events.FarmerVerifiedType, events.AccountRoleChangedType)
	for _, e := range acctEnvs[1:] {
		if am := e.GetMetadata(); am.GetActor().GetService() != auth.ServiceName || am.GetCorrelationId() != md.GetCorrelationId() {
			t.Fatalf("worker account event metadata = %v", am)
		}
	}
}

func TestSecondVerifiedLeaseDoesNotRepeatRoleChange(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.newUser()

	h.useChecker(blockingChecker)
	first, err := h.submit(p, "AB-1") // no answer: PENDING for the worker
	mustNoErr(t, err)
	h.useChecker(eijara.Stub{Now: h.clock.Now})
	_, err = h.submit(p, "AB-2") // VERIFIED: ROLE_FARMER granted
	mustNoErr(t, err)

	h.clock.Advance(time.Minute)
	n, err := h.svc.RetryPending(ctx)
	mustNoErr(t, err)
	if n != 1 {
		t.Fatalf("decided %d, want 1", n)
	}
	// The first lease is VERIFIED too, but the account already held the role:
	// FarmerVerified without another AccountRoleChanged, version unchanged.
	envs := h.accountEvents(p.UserID)
	wantTypes(t, envs, events.AccountCreatedType, events.FarmerVerifiedType, events.AccountRoleChangedType,
		events.FarmerVerifiedType)
	if last := envs[3]; last.GetFarmerVerified().GetVerificationId() != first.ID.String() || last.GetAccount().GetVersion() != 2 {
		t.Fatalf("second FarmerVerified = %v", last)
	}
	if a := h.account(p.UserID); a.Version != 2 {
		t.Fatalf("account = %+v", a)
	}
}

func TestContractClaimedAtDecisionTimeRejects(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := h.newUser(), h.newUser()

	h.useChecker(blockingChecker)
	va, err := h.submit(a, "LEASE-7")
	mustNoErr(t, err)
	h.useChecker(eijara.Stub{Now: h.clock.Now})
	vb, err := h.submit(b, "LEASE-7") // a's is only PENDING, so b may claim it
	mustNoErr(t, err)
	if vb.Status != domain.VerificationVerified {
		t.Fatalf("b = %+v", vb)
	}

	h.clock.Advance(time.Minute)
	n, err := h.svc.RetryPending(ctx)
	mustNoErr(t, err)
	if n != 1 {
		t.Fatalf("decided %d, want 1", n)
	}
	got := h.store.snapshot().verifications[va.ID]
	if got.Status != domain.VerificationRejected || got.RejectionReason != domain.ReasonContractClaimed {
		t.Fatalf("a = %+v", got)
	}
	if h.account(a.UserID).HasRole(domain.RoleFarmer) {
		t.Fatal("a must not be a farmer")
	}
	envs := h.notifications()
	last := envs[len(envs)-1].GetNotificationRequested()
	if last.GetRecipientId() != a.UserID.String() ||
		last.GetParams()["rejection_reason"] != "ERROR_REASON_LEASE_CONTRACT_ALREADY_CLAIMED" {
		t.Fatalf("notification = %v", last)
	}
	// Only the committed outcome is published: no FarmerVerified for a.
	acctEnvs := h.accountEvents(a.UserID)
	wantTypes(t, acctEnvs, events.AccountCreatedType, events.FarmerVerificationRejectedType)
	if r := acctEnvs[1].GetFarmerVerificationRejected(); r.GetVerificationId() != va.ID.String() ||
		r.GetRejectionReason() != domain.ReasonContractClaimed {
		t.Fatalf("FarmerVerificationRejected = %v", r)
	}
}

func TestExpiryRevokesFarmerRole(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.newUser()
	v, err := h.submit(p, "AB-1") // valid until 2027-09-22
	mustNoErr(t, err)

	// 2027-09-22 23:00 in Tashkent: the last day of the lease.
	h.clock.Set(time.Date(2027, 9, 22, 18, 0, 0, 0, time.UTC))
	n, err := h.svc.ExpireVerifications(ctx)
	mustNoErr(t, err)
	if n != 0 {
		t.Fatalf("expired %d on the last day", n)
	}

	// 2027-09-23 00:30 in Tashkent (still the 22nd in UTC).
	expiredAt := time.Date(2027, 9, 22, 19, 30, 0, 0, time.UTC)
	h.clock.Set(expiredAt)
	n, err = h.svc.ExpireVerifications(ctx)
	mustNoErr(t, err)
	if n != 1 {
		t.Fatalf("expired %d, want 1", n)
	}
	if got := h.store.snapshot().verifications[v.ID]; got.Status != domain.VerificationExpired {
		t.Fatalf("verification = %+v", got)
	}
	a := h.account(p.UserID)
	if a.HasRole(domain.RoleFarmer) || a.Version != 3 || a.FarmerVerificationStatus != domain.VerificationExpired {
		t.Fatalf("account = %+v", a)
	}
	envs := h.notifications()
	last := envs[len(envs)-1]
	if last.GetNotificationRequested().GetTemplateKey() != events.TemplateFarmerVerificationExpired ||
		last.GetMetadata().GetActor().GetService() != auth.ServiceName {
		t.Fatalf("notification = %v", last)
	}

	// Account stream: the expiry fact, then the revocation it caused.
	acctEnvs := h.accountEvents(p.UserID)
	wantTypes(t, acctEnvs, events.AccountCreatedType, events.FarmerVerifiedType, events.AccountRoleChangedType,
		events.FarmerVerificationExpiredType, events.AccountRoleChangedType)
	expired, revoked := acctEnvs[3], acctEnvs[4]
	if e := expired.GetFarmerVerificationExpired(); e.GetVerificationId() != v.ID.String() ||
		e.GetLeaseValidUntil().GetYear() != 2027 || e.GetLeaseValidUntil().GetMonth() != 9 || e.GetLeaseValidUntil().GetDay() != 22 ||
		expired.GetAccount().GetVersion() != 2 || !expired.GetAccount().GetIsVerifiedFarmer() {
		t.Fatalf("FarmerVerificationExpired = %v", expired)
	}
	rc := revoked.GetAccountRoleChanged()
	if !slices.Equal(rc.GetPreviousRoles(), []commonv1.Role{domain.RoleReseller, domain.RoleFarmer}) ||
		!slices.Equal(rc.GetRoles(), []commonv1.Role{domain.RoleReseller}) || len(rc.GetGrantedRoles()) != 0 ||
		!slices.Equal(rc.GetRevokedRoles(), []commonv1.Role{domain.RoleFarmer}) || rc.GetFarmerVerificationId() != v.ID.String() ||
		revoked.GetAccount().GetVersion() != 3 || revoked.GetAccount().GetIsVerifiedFarmer() {
		t.Fatalf("AccountRoleChanged = %v", revoked)
	}
	for _, e := range []*eventsv1.AccountEvent{expired, revoked} {
		md := e.GetMetadata()
		if md.GetActor().GetService() != auth.ServiceName || uuid.Validate(md.GetCorrelationId()) != nil ||
			md.GetCorrelationId() != last.GetMetadata().GetCorrelationId() || !md.GetOccurredAt().AsTime().Equal(expiredAt) {
			t.Fatalf("expiry metadata = %v", md)
		}
	}

	// Nothing left to expire, and the user may verify a new lease.
	if n, _ := h.svc.ExpireVerifications(ctx); n != 0 {
		t.Fatalf("expired %d again", n)
	}
	v2, err := h.submit(p, "AB-2")
	mustNoErr(t, err)
	if v2.Status != domain.VerificationVerified {
		t.Fatalf("new verification = %+v", v2)
	}
}

func TestExpiryKeepsRoleWhileAnotherLeaseIsVerified(t *testing.T) {
	h := newHarness(t)
	p := h.newUser()
	v, err := h.submit(p, "AB-1")
	mustNoErr(t, err)
	// A second VERIFIED lease that runs longer (e.g. two plots).
	h.store.update(func(db *fakeDB) {
		longer := v
		longer.ID, longer.RequestID, longer.ContractNumber = uuid.New(), uuid.New(), "AB-2"
		longer.Lease = &domain.Lease{ContractNumber: "AB-2", ValidFrom: v.Lease.ValidFrom, ValidUntil: v.Lease.ValidUntil.AddDate(1, 0, 0)}
		db.verifications[longer.ID] = longer
	})
	h.clock.Set(time.Date(2027, 10, 1, 0, 0, 0, 0, time.UTC))
	n, err := h.svc.ExpireVerifications(context.Background())
	mustNoErr(t, err)
	if a := h.account(p.UserID); n != 1 || !a.HasRole(domain.RoleFarmer) || a.Version != 2 {
		t.Fatalf("n=%d account=%+v", n, a)
	}
	// The expiry is published; the role set did not change, so no AccountRoleChanged.
	wantTypes(t, h.accountEvents(p.UserID), events.AccountCreatedType, events.FarmerVerifiedType,
		events.AccountRoleChangedType, events.FarmerVerificationExpiredType)
}

func TestSetAccountPremium(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	user := h.newUser().UserID
	escrow := auth.Principal{Caller: auth.CallerOrderEscrow, RequestID: "req-premium", Traceparent: testTraceparent}

	a, err := h.svc.SetAccountPremium(ctx, escrow, user.String(), true)
	mustNoErr(t, err)
	if !a.IsPremium || a.Plan != domain.PlanPremium || a.PremiumUntil != nil || a.Version != 2 || !a.UpdatedAt.Equal(start) {
		t.Fatalf("after on: %+v", a)
	}
	envs := h.accountEvents(user)
	wantTypes(t, envs, events.AccountCreatedType, events.AccountPremiumChangedType)
	on, md := envs[1], envs[1].GetMetadata()
	if !on.GetAccountPremiumChanged().GetIsPremium() || !on.GetAccount().GetIsPremium() || on.GetAccount().GetPremiumUntil() != nil ||
		on.GetAccount().GetVersion() != 2 {
		t.Fatalf("AccountPremiumChanged = %v", on)
	}
	if md.GetActor().GetService() != auth.CallerOrderEscrow || md.GetCorrelationId() != "req-premium" ||
		md.GetTraceparent() != testTraceparent || !md.GetOccurredAt().AsTime().Equal(start) {
		t.Fatalf("metadata = %v", md)
	}

	// The value it already has: no version bump, no event.
	a, err = h.svc.SetAccountPremium(ctx, escrow, user.String(), true)
	mustNoErr(t, err)
	if !a.IsPremium || a.Version != 2 {
		t.Fatalf("repeated on: %+v", a)
	}
	wantTypes(t, h.accountEvents(user), events.AccountCreatedType, events.AccountPremiumChangedType)

	a, err = h.svc.SetAccountPremium(ctx, escrow, user.String(), false)
	mustNoErr(t, err)
	if a.IsPremium || a.Plan != domain.PlanDefault || a.Version != 3 {
		t.Fatalf("after off: %+v", a)
	}
	envs = h.accountEvents(user)
	wantTypes(t, envs, events.AccountCreatedType, events.AccountPremiumChangedType, events.AccountPremiumChangedType)
	if off := envs[2]; off.GetAccountPremiumChanged().GetIsPremium() || off.GetAccount().GetIsPremium() || off.GetAccount().GetVersion() != 3 {
		t.Fatalf("AccountPremiumChanged = %v", off)
	}

	a, err = h.svc.SetAccountPremium(ctx, escrow, user.String(), false)
	mustNoErr(t, err)
	if a.IsPremium || a.Version != 3 || len(h.accountEvents(user)) != 3 {
		t.Fatalf("repeated off: %+v", a)
	}

	// On a user's behalf (user headers forwarded), the user is the actor.
	onBehalf := escrow
	onBehalf.UserID = user
	_, err = h.svc.SetAccountPremium(ctx, onBehalf, user.String(), true)
	mustNoErr(t, err)
	envs = h.accountEvents(user)
	if md := envs[len(envs)-1].GetMetadata(); md.GetActor().GetUserId() != user.String() {
		t.Fatalf("actor = %v", md.GetActor())
	}
}

func TestSetAccountPremiumComparesTheActiveValue(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	user := h.newUser().UserID
	escrow := auth.Principal{Caller: auth.CallerOrderEscrow}

	// A Premium period set by hand in SQL that has already ended: plan
	// PREMIUM, but Premium is not active.
	ended := start.Add(-time.Hour)
	h.store.update(func(db *fakeDB) {
		a := db.accounts[user]
		a.Plan, a.PremiumUntil = domain.PlanPremium, &ended
		db.accounts[user] = a
	})
	a, err := h.svc.SetAccountPremium(ctx, escrow, user.String(), false)
	mustNoErr(t, err)
	if a.IsPremium || a.Version != 1 {
		t.Fatalf("off while already inactive: %+v", a)
	}
	wantTypes(t, h.accountEvents(user), events.AccountCreatedType)

	a, err = h.svc.SetAccountPremium(ctx, escrow, user.String(), true)
	mustNoErr(t, err)
	if !a.IsPremium || a.PremiumUntil != nil || a.Version != 2 {
		t.Fatalf("on: %+v", a)
	}
	wantTypes(t, h.accountEvents(user), events.AccountCreatedType, events.AccountPremiumChangedType)
}

func TestSetAccountPremiumAuthorization(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	owner := h.newUser()
	admin := auth.Principal{UserID: owner.UserID, Roles: []commonv1.Role{domain.RoleReseller, domain.RoleAdmin}}

	tests := []struct {
		name string
		p    auth.Principal
	}{
		{"the user", owner},
		{"admin", admin},
		{"auth-gateway", auth.Principal{Caller: auth.CallerAuthGateway}},
		{"another service", auth.Principal{Caller: "catalog"}},
		{"anonymous", auth.Principal{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.svc.SetAccountPremium(ctx, tt.p, owner.UserID.String(), true)
			wantErr(t, err, codes.PermissionDenied, commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED)
		})
	}

	escrow := auth.Principal{Caller: auth.CallerOrderEscrow}
	_, err := h.svc.SetAccountPremium(ctx, escrow, "not-a-uuid", true)
	ae := wantErr(t, err, codes.InvalidArgument, commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED)
	if len(ae.Violations) != 1 || ae.Violations[0].GetField() != "user_id" {
		t.Fatalf("violations = %v", ae.Violations)
	}
	_, err = h.svc.SetAccountPremium(ctx, escrow, uuid.NewString(), true)
	wantErr(t, err, codes.NotFound, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)

	if a := h.account(owner.UserID); a.IsPremium || a.Version != 1 {
		t.Fatalf("account changed: %+v", a)
	}
	wantTypes(t, h.accountEvents(owner.UserID), events.AccountCreatedType)
}

// TestStateAndEventsCommitTogether: when the outbox write fails, the state
// change rolls back with it, so no change is ever committed without its event.
func TestStateAndEventsCommitTogether(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.newUser()
	h.store.update(func(db *fakeDB) { db.outboxErr = errors.New("insert into outbox: connection reset") })

	_, err := h.svc.CreateAccount(ctx, auth.Principal{Caller: auth.CallerAuthGateway}, uuid.NewString())
	wantErr(t, err, codes.Internal, commonv1.ErrorReason_ERROR_REASON_INTERNAL)
	if n := len(h.store.snapshot().accounts); n != 1 {
		t.Fatalf("an account was committed without AccountCreated: %d accounts", n)
	}

	_, err = h.svc.SetAccountPremium(ctx, auth.Principal{Caller: auth.CallerOrderEscrow}, p.UserID.String(), true)
	wantErr(t, err, codes.Internal, commonv1.ErrorReason_ERROR_REASON_INTERNAL)
	if a := h.account(p.UserID); a.IsPremium || a.Plan != domain.PlanDefault || a.Version != 1 {
		t.Fatalf("Premium was committed without its event: %+v", a)
	}

	// The decision rolls back; the verification stays PENDING for the worker.
	_, err = h.submit(p, "AB-1")
	wantErr(t, err, codes.Internal, commonv1.ErrorReason_ERROR_REASON_INTERNAL)
	latest, err := h.svc.GetFarmerVerification(ctx, p, "")
	mustNoErr(t, err)
	if a := h.account(p.UserID); latest.Status != domain.VerificationPending || a.HasRole(domain.RoleFarmer) || a.Version != 1 {
		t.Fatalf("decision committed without its events: verification=%+v account=%+v", latest, a)
	}
	wantTypes(t, h.accountEvents(p.UserID), events.AccountCreatedType)

	// Once the outbox works again, the worker decides it and publishes.
	h.store.update(func(db *fakeDB) { db.outboxErr = nil })
	h.clock.Advance(time.Minute)
	n, err := h.svc.RetryPending(ctx)
	mustNoErr(t, err)
	if n != 1 {
		t.Fatalf("decided %d, want 1", n)
	}
	wantTypes(t, h.accountEvents(p.UserID), events.AccountCreatedType, events.FarmerVerifiedType, events.AccountRoleChangedType)
}
