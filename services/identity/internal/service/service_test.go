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

// notifications decodes the outbox.
func (h *harness) notifications() []*eventsv1.NotificationRequestEvent {
	h.t.Helper()
	var out []*eventsv1.NotificationRequestEvent
	for _, m := range h.store.snapshot().outbox {
		var env eventsv1.NotificationRequestEvent
		if err := proto.Unmarshal(m.Value, &env); err != nil {
			h.t.Fatalf("outbox payload: %v", err)
		}
		if m.Topic != events.TopicNotificationRequest || m.Key != env.GetNotificationRequested().GetRecipientId() {
			h.t.Fatalf("outbox topic/key = %s/%s", m.Topic, m.Key)
		}
		out = append(out, &env)
	}
	return out
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
	gw := auth.Principal{Caller: auth.CallerAuthGateway}
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

	for _, p := range []auth.Principal{{UserID: a.ID}, {Caller: "catalog"}, {}} {
		_, err := h.svc.CreateAccount(ctx, p, uuid.NewString())
		wantErr(t, err, codes.PermissionDenied, commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED)
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
	p.Traceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

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
		t.Fatalf("outbox has %d events, want 1", len(envs))
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
		t.Fatalf("outbox has %d events, want 1", len(envs))
	}
	n := envs[0].GetNotificationRequested()
	if n.GetTemplateKey() != events.TemplateFarmerVerificationRejected ||
		n.GetParams()["rejection_reason"] != "ERROR_REASON_LEASE_CONTRACT_INVALID" {
		t.Fatalf("notification = %v", n)
	}
	// A rejection does not block another attempt.
	v2, err := h.submit(p, "AB-7")
	mustNoErr(t, err)
	if v2.Status != domain.VerificationVerified {
		t.Fatalf("second attempt = %+v", v2)
	}
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
	if n := len(h.store.snapshot().outbox); n != 1 {
		t.Fatalf("outbox has %d events, want 1", n)
	}
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
	if len(h.store.snapshot().outbox) != 0 || h.account(p.UserID).HasRole(domain.RoleFarmer) {
		t.Fatal("a PENDING verification must not notify or grant")
	}

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
		t.Fatalf("outbox has %d events, want 1", len(envs))
	}
	md := envs[0].GetMetadata()
	if md.GetActor().GetService() != auth.ServiceName || md.GetCorrelationId() == p.RequestID ||
		uuid.Validate(md.GetCorrelationId()) != nil {
		t.Fatalf("worker event metadata = %v", md)
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
	h.clock.Set(time.Date(2027, 9, 22, 19, 30, 0, 0, time.UTC))
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
}
