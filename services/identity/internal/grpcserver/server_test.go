package grpcserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	identityv1 "github.com/tbilol/dxrl/gen/go/identity/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpchealth "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/tbilol/dxrl/services/identity/internal/apperr"
	"github.com/tbilol/dxrl/services/identity/internal/auth"
	"github.com/tbilol/dxrl/services/identity/internal/domain"
)

// stubService records the principal and answers from its fields.
type stubService struct {
	got   auth.Principal
	calls int
	err   error
	panic bool
	acct  domain.Account
	verif domain.Verification
}

func (s *stubService) answer(p auth.Principal) error {
	s.got, s.calls = p, s.calls+1
	if s.panic {
		panic("boom")
	}
	return s.err
}

func (s *stubService) CreateAccount(_ context.Context, p auth.Principal, _ string) (domain.Account, error) {
	return s.acct, s.answer(p)
}

func (s *stubService) GetAccount(_ context.Context, p auth.Principal, _ string) (domain.Account, error) {
	return s.acct, s.answer(p)
}

func (s *stubService) SubmitFarmerVerification(_ context.Context, p auth.Principal, _, _ string) (domain.Verification, error) {
	return s.verif, s.answer(p)
}

func (s *stubService) GetFarmerVerification(_ context.Context, p auth.Principal, _ string) (domain.Verification, error) {
	return s.verif, s.answer(p)
}

func dial(t *testing.T, svc Service) (identityv1.IdentityServiceClient, healthpb.HealthClient) {
	t.Helper()
	validator, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	srv := New(svc, grpchealth.NewServer(), Options{Logger: slog.New(slog.DiscardHandler), Validator: validator, Reflection: true})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return identityv1.NewIdentityServiceClient(conn), healthpb.NewHealthClient(conn)
}

func detail(t *testing.T, err error, code codes.Code) *commonv1.Error {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok || st.Code() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
	if len(st.Details()) != 1 {
		t.Fatalf("want exactly one detail, got %v", st.Details())
	}
	e, ok := st.Details()[0].(*commonv1.Error)
	if !ok || e.GetDomain() != apperr.Domain {
		t.Fatalf("detail = %v", st.Details()[0])
	}
	return e
}

func TestValidationInterceptor(t *testing.T) {
	svc := &stubService{}
	client, _ := dial(t, svc)
	_, err := client.CreateAccount(context.Background(), &identityv1.CreateAccountRequest{RequestId: "not-a-uuid"})
	e := detail(t, err, codes.InvalidArgument)
	if e.GetReason() != commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED || len(e.GetFieldViolations()) != 1 ||
		e.GetFieldViolations()[0].GetField() != "request_id" || e.GetFieldViolations()[0].GetRule() != "string.uuid" {
		t.Fatalf("error = %v", e)
	}
	_, err = client.SubmitFarmerVerification(context.Background(), &identityv1.SubmitFarmerVerificationRequest{
		RequestId: uuid.NewString(), LeaseContractNumber: "ab",
	})
	if e := detail(t, err, codes.InvalidArgument); e.GetFieldViolations()[0].GetField() != "lease_contract_number" {
		t.Fatalf("error = %v", e)
	}
	// Empty user_id is allowed (IGNORE_IF_ZERO_VALUE).
	if _, err := client.GetAccount(context.Background(), &identityv1.GetAccountRequest{}); err != nil {
		t.Fatal(err)
	}
	if svc.calls != 1 {
		t.Fatalf("service called %d times, want 1 (invalid requests must not reach it)", svc.calls)
	}
}

func TestPrincipalAndResponseMapping(t *testing.T) {
	user := uuid.New()
	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	day := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	svc := &stubService{
		acct: domain.Account{ID: user, Roles: []commonv1.Role{domain.RoleReseller, domain.RoleFarmer}, Status: domain.AccountActive,
			Plan: domain.PlanDefault, Version: 2, CreatedAt: now, UpdatedAt: now, FarmerVerificationStatus: domain.VerificationVerified},
		verif: domain.Verification{ID: uuid.New(), UserID: user, ContractNumber: "AB-1", Status: domain.VerificationVerified,
			SubmittedAt: now, DecidedAt: &now, Lease: &domain.Lease{ContractNumber: "AB-1", ValidFrom: day, ValidUntil: day.AddDate(1, 0, 0)}},
	}
	client, _ := dial(t, svc)
	ctx := metadata.AppendToOutgoingContext(context.Background(),
		auth.HeaderUserID, user.String(), auth.HeaderRoles, "RESELLER,FARMER", auth.HeaderRequestID, "req-42")

	var header metadata.MD
	resp, err := client.GetAccount(ctx, &identityv1.GetAccountRequest{}, grpc.Header(&header))
	if err != nil {
		t.Fatal(err)
	}
	if svc.got.UserID != user || !svc.got.HasRole(domain.RoleFarmer) || svc.got.RequestID != "req-42" {
		t.Fatalf("principal = %+v", svc.got)
	}
	if got := header.Get(auth.HeaderRequestID); len(got) != 1 || got[0] != "req-42" {
		t.Fatalf("x-request-id header = %v", got)
	}
	a := resp.GetAccount()
	if a.GetId() != user.String() || !a.GetIsVerifiedFarmer() || a.GetVersion() != 2 ||
		a.GetFarmerVerificationStatus() != domain.VerificationVerified || a.GetPremiumUntil() != nil {
		t.Fatalf("account = %v", a)
	}

	vr, err := client.GetFarmerVerification(ctx, &identityv1.GetFarmerVerificationRequest{})
	if err != nil {
		t.Fatal(err)
	}
	lease := vr.GetVerification().GetLease()
	if lease.GetValidFrom().GetYear() != 2026 || lease.GetValidUntil().GetYear() != 2027 || lease.GetValidUntil().GetMonth() != 9 ||
		lease.GetValidUntil().GetDay() != 22 || lease.GetLocation() != nil || vr.GetVerification().GetDecidedAt() == nil {
		t.Fatalf("verification = %v", vr.GetVerification())
	}
}

func TestErrorsCarryOneCommonError(t *testing.T) {
	svc := &stubService{}
	client, _ := dial(t, svc)
	ctx := context.Background()

	svc.err = apperr.New(commonv1.ErrorReason_ERROR_REASON_LEASE_CONTRACT_ALREADY_CLAIMED, "taken")
	_, err := client.GetAccount(ctx, &identityv1.GetAccountRequest{})
	if e := detail(t, err, codes.AlreadyExists); e.GetReason() != commonv1.ErrorReason_ERROR_REASON_LEASE_CONTRACT_ALREADY_CLAIMED {
		t.Fatalf("error = %v", e)
	}

	svc.err = errors.New("postgres: password authentication failed")
	_, err = client.GetAccount(ctx, &identityv1.GetAccountRequest{})
	if e := detail(t, err, codes.Internal); e.GetReason() != commonv1.ErrorReason_ERROR_REASON_INTERNAL || e.GetMessage() != "internal error" {
		t.Fatalf("internal error leaked: %v", e)
	}

	svc.err, svc.panic = nil, true
	_, err = client.GetAccount(ctx, &identityv1.GetAccountRequest{})
	detail(t, err, codes.Internal)

	svc.panic = false
	_, err = client.GetAccount(metadata.AppendToOutgoingContext(ctx, auth.HeaderUserID, "42"), &identityv1.GetAccountRequest{})
	if e := detail(t, err, codes.Unauthenticated); e.GetReason() != commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED {
		t.Fatalf("error = %v", e)
	}
}

func TestHealthServiceRegistered(t *testing.T) {
	_, health := dial(t, &stubService{})
	resp, err := health.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health = %v, %v", resp, err)
	}
}
