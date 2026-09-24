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
	authgatewayv1 "github.com/tbilol/dxrl/gen/go/authgateway/v1"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpchealth "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/tbilol/dxrl/services/auth-gateway/internal/apperr"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/auth"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/domain"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/service"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/token"
)

// stubService records what the handler decoded and answers from its fields.
type stubService struct {
	got      auth.Principal
	calls    int
	err      error
	panic    bool
	signIn   service.SignInInput
	link     service.LinkInput
	setup    service.PhoneSetup
	scope    authgatewayv1.SignOutScope
	method   authgatewayv1.SignInMethod
	client   domain.ClientInfo
	result   service.SignInResult
	pair     service.TokenPair
	verified service.Verified
	creds    []domain.Credential
	keys     []token.JSONWebKey
}

func (s *stubService) answer(p auth.Principal) error {
	s.got, s.calls = p, s.calls+1
	if s.panic {
		panic("boom")
	}
	return s.err
}

func (s *stubService) SignIn(_ context.Context, p auth.Principal, in service.SignInInput) (service.SignInResult, error) {
	s.signIn = in
	return s.result, s.answer(p)
}

func (s *stubService) RequestPhoneCode(_ context.Context, p auth.Principal, phone string, purpose authgatewayv1.PhoneCodePurpose) (service.Challenge, error) {
	s.setup = service.PhoneSetup{Phone: phone}
	return service.Challenge{
		ID:                uuid.MustParse("0198e4d0-0000-7000-8000-00000000000c"),
		ExpiresAt:         time.Unix(1800000000, 0).UTC(),
		ResendAvailableAt: time.Unix(1799999940, 0).UTC(),
	}, s.answer(p)
}

func (s *stubService) SignUpWithPhone(_ context.Context, p auth.Principal, _ string, setup service.PhoneSetup, client domain.ClientInfo) (service.SignInResult, error) {
	s.setup, s.client = setup, client
	return s.result, s.answer(p)
}

func (s *stubService) ResetPassword(_ context.Context, p auth.Principal, setup service.PhoneSetup) error {
	s.setup = setup
	return s.answer(p)
}

func (s *stubService) LinkSignInMethod(_ context.Context, p auth.Principal, in service.LinkInput) ([]domain.Credential, error) {
	s.link = in
	return s.creds, s.answer(p)
}

func (s *stubService) UnlinkSignInMethod(_ context.Context, p auth.Principal, method authgatewayv1.SignInMethod) ([]domain.Credential, error) {
	s.method = method
	return s.creds, s.answer(p)
}

func (s *stubService) ListSignInMethods(_ context.Context, p auth.Principal) ([]domain.Credential, error) {
	return s.creds, s.answer(p)
}

func (s *stubService) RefreshToken(_ context.Context, p auth.Principal, _ string) (service.TokenPair, error) {
	return s.pair, s.answer(p)
}

func (s *stubService) SignOut(_ context.Context, p auth.Principal, scope authgatewayv1.SignOutScope) error {
	s.scope = scope
	return s.answer(p)
}

func (s *stubService) VerifyToken(_ context.Context, p auth.Principal, _ string) (service.Verified, error) {
	return s.verified, s.answer(p)
}

func (s *stubService) JWKS() []token.JSONWebKey { return s.keys }

func dial(t *testing.T, svc Service) (authgatewayv1.AuthGatewayServiceClient, healthpb.HealthClient) {
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
	return authgatewayv1.NewAuthGatewayServiceClient(conn), healthpb.NewHealthClient(conn)
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
	ctx := context.Background()

	// No credential at all.
	_, err := client.SignIn(ctx, &authgatewayv1.SignInRequest{})
	e := detail(t, err, codes.InvalidArgument)
	if e.GetReason() != commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED || len(e.GetFieldViolations()) != 1 {
		t.Fatalf("error = %v", e)
	}
	// A phone number that is not Uzbek.
	_, err = client.SignIn(ctx, &authgatewayv1.SignInRequest{
		Credential: &authgatewayv1.SignInRequest_PhonePassword{
			PhonePassword: &authgatewayv1.PhonePasswordCredential{PhoneNumber: "+79001234567", Password: "x"},
		},
	})
	if e := detail(t, err, codes.InvalidArgument); e.GetFieldViolations()[0].GetField() != "phone_password.phone_number" {
		t.Fatalf("error = %v", e)
	}
	// A password that is too short for a sign-up.
	_, err = client.SignUpWithPhone(ctx, &authgatewayv1.SignUpWithPhoneRequest{
		RequestId: uuid.NewString(),
		PhonePassword: &authgatewayv1.PhonePasswordSetup{
			PhoneNumber:  "+998901234567",
			Verification: &authgatewayv1.PhoneCodeVerification{ChallengeId: uuid.NewString(), Code: "123456"},
			Password:     "short",
		},
	})
	if e := detail(t, err, codes.InvalidArgument); e.GetFieldViolations()[0].GetField() != "phone_password.password" {
		t.Fatalf("error = %v", e)
	}
	// A code that is not digits.
	_, err = client.ResetPassword(ctx, &authgatewayv1.ResetPasswordRequest{
		PhonePassword: &authgatewayv1.PhonePasswordSetup{
			PhoneNumber:  "+998901234567",
			Verification: &authgatewayv1.PhoneCodeVerification{ChallengeId: uuid.NewString(), Code: "abcd"},
			Password:     "long-enough-password",
		},
	})
	detail(t, err, codes.InvalidArgument)

	if svc.calls != 0 {
		t.Fatalf("service called %d times, want 0 (invalid requests must not reach it)", svc.calls)
	}
}

func TestSignInDecodingAndResponse(t *testing.T) {
	userID, sessionID := uuid.New(), uuid.New()
	expiry := time.Unix(1800000000, 0).UTC()
	svc := &stubService{result: service.SignInResult{
		UserID: userID, SessionID: sessionID, IsNewAccount: true,
		Tokens: service.TokenPair{
			AccessToken: "access", AccessTokenExpiresAt: expiry,
			RefreshToken: "refresh", RefreshTokenExpiresAt: expiry.Add(time.Hour),
		},
	}}
	client, _ := dial(t, svc)

	resp, err := client.SignIn(context.Background(), &authgatewayv1.SignInRequest{
		Credential: &authgatewayv1.SignInRequest_Google{
			Google: &authgatewayv1.GoogleCredential{IdToken: "dev-google:123", Nonce: "n-1"},
		},
		Client: &authgatewayv1.ClientInfo{
			Platform: authgatewayv1.ClientPlatform_CLIENT_PLATFORM_IOS, DeviceName: "iPhone 15", DeviceId: "d-1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if svc.signIn.Method != domain.MethodGoogle || svc.signIn.Token != "dev-google:123" || svc.signIn.Nonce != "n-1" ||
		svc.signIn.Client.Platform != authgatewayv1.ClientPlatform_CLIENT_PLATFORM_IOS ||
		svc.signIn.Client.DeviceName != "iPhone 15" {
		t.Fatalf("decoded input = %+v", svc.signIn)
	}
	if resp.GetUserId() != userID.String() || resp.GetSessionId() != sessionID.String() || !resp.GetIsNewAccount() ||
		resp.GetTokens().GetAccessToken() != "access" || resp.GetTokens().GetRefreshToken() != "refresh" ||
		!resp.GetTokens().GetAccessTokenExpiresAt().AsTime().Equal(expiry) {
		t.Fatalf("response = %v", resp)
	}

	// The Apple and phone cases decode too.
	if _, err := client.SignIn(context.Background(), &authgatewayv1.SignInRequest{
		Credential: &authgatewayv1.SignInRequest_Apple{
			Apple: &authgatewayv1.AppleCredential{IdentityToken: "dev-apple:9"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if svc.signIn.Method != domain.MethodApple || svc.signIn.Token != "dev-apple:9" {
		t.Fatalf("decoded input = %+v", svc.signIn)
	}
	if _, err := client.SignIn(context.Background(), &authgatewayv1.SignInRequest{
		Credential: &authgatewayv1.SignInRequest_PhonePassword{
			PhonePassword: &authgatewayv1.PhonePasswordCredential{PhoneNumber: "+998901234567", Password: "pw"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if svc.signIn.Method != domain.MethodPhonePassword || svc.signIn.Phone != "+998901234567" || svc.signIn.Password != "pw" {
		t.Fatalf("decoded input = %+v", svc.signIn)
	}
}

func TestPrincipalFromMetadata(t *testing.T) {
	userID, sessionID := uuid.New(), uuid.New()
	svc := &stubService{}
	client, _ := dial(t, svc)
	ctx := metadata.AppendToOutgoingContext(context.Background(),
		auth.HeaderUserID, userID.String(), auth.HeaderSessionID, sessionID.String(),
		auth.HeaderRoles, "RESELLER", auth.HeaderRequestID, "req-42")

	var header metadata.MD
	if _, err := client.SignOut(ctx, &authgatewayv1.SignOutRequest{
		Scope: authgatewayv1.SignOutScope_SIGN_OUT_SCOPE_ALL_SESSIONS,
	}, grpc.Header(&header)); err != nil {
		t.Fatal(err)
	}
	if svc.got.UserID != userID || svc.got.SessionID != sessionID || svc.got.RequestID != "req-42" {
		t.Fatalf("principal = %+v", svc.got)
	}
	if svc.scope != authgatewayv1.SignOutScope_SIGN_OUT_SCOPE_ALL_SESSIONS {
		t.Fatalf("scope = %v", svc.scope)
	}
	if got := header.Get(auth.HeaderRequestID); len(got) != 1 || got[0] != "req-42" {
		t.Fatalf("x-request-id header = %v", got)
	}
}

func TestVerifyTokenAndJwksMapping(t *testing.T) {
	userID, sessionID := uuid.New(), uuid.New()
	expiry := time.Unix(1800000000, 0).UTC()
	svc := &stubService{
		verified: service.Verified{
			UserID: userID, SessionID: sessionID, Premium: true,
			Roles: []commonv1.Role{commonv1.Role_ROLE_RESELLER, commonv1.Role_ROLE_FARMER}, ExpiresAt: expiry,
		},
		keys: []token.JSONWebKey{{Kid: "kid-1", Kty: "EC", Alg: "ES256", Use: "sig", Crv: "P-256", X: "x", Y: "y"}},
	}
	client, _ := dial(t, svc)
	ctx := metadata.AppendToOutgoingContext(context.Background(), auth.HeaderCaller, "api-gateway")

	resp, err := client.VerifyToken(ctx, &authgatewayv1.VerifyTokenRequest{AccessToken: "a.b.c"})
	if err != nil {
		t.Fatal(err)
	}
	p := resp.GetPrincipal()
	if p.GetUserId() != userID.String() || p.GetSessionId() != sessionID.String() || !p.GetIsPremium() ||
		len(p.GetRoles()) != 2 || !resp.GetExpiresAt().AsTime().Equal(expiry) {
		t.Fatalf("principal = %v", resp)
	}
	if svc.got.Caller != "api-gateway" {
		t.Fatalf("caller = %q", svc.got.Caller)
	}

	jwks, err := client.GetJwks(context.Background(), &authgatewayv1.GetJwksRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jwks.GetKeys()) != 1 {
		t.Fatalf("jwks = %v", jwks)
	}
	k := jwks.GetKeys()[0]
	if k.GetKid() != "kid-1" || k.GetKty() != "EC" || k.GetCrv() != "P-256" || k.GetAlg() != "ES256" ||
		k.GetUse() != "sig" || k.GetX() != "x" || k.GetY() != "y" || k.GetN() != "" {
		t.Fatalf("key = %v", k)
	}
}

func TestLinkedMethodsMapping(t *testing.T) {
	linked := time.Unix(1700000000, 0).UTC()
	used := linked.Add(time.Hour)
	svc := &stubService{creds: []domain.Credential{
		{Method: domain.MethodPhonePassword, DisplayHint: "+998 90 *** ** 67", LinkedAt: linked, LastUsedAt: &used},
		{Method: domain.MethodGoogle, DisplayHint: "a***@gmail.com", LinkedAt: linked},
	}}
	client, _ := dial(t, svc)
	ctx := metadata.AppendToOutgoingContext(context.Background(), auth.HeaderUserID, uuid.NewString())

	resp, err := client.ListSignInMethods(ctx, &authgatewayv1.ListSignInMethodsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetMethods()) != 2 {
		t.Fatalf("methods = %v", resp.GetMethods())
	}
	first, second := resp.GetMethods()[0], resp.GetMethods()[1]
	if first.GetMethod() != domain.MethodPhonePassword || first.GetDisplayHint() != "+998 90 *** ** 67" ||
		!first.GetLastUsedAt().AsTime().Equal(used) {
		t.Fatalf("first method = %v", first)
	}
	if second.GetLastUsedAt() != nil {
		t.Fatalf("a method that was never used must have no last_used_at: %v", second)
	}

	// Unlink decodes the enum.
	if _, err := client.UnlinkSignInMethod(ctx, &authgatewayv1.UnlinkSignInMethodRequest{
		Method: authgatewayv1.SignInMethod_SIGN_IN_METHOD_GOOGLE,
	}); err != nil {
		t.Fatal(err)
	}
	if svc.method != domain.MethodGoogle {
		t.Fatalf("method = %v", svc.method)
	}
}

func TestErrorsCarryOneCommonError(t *testing.T) {
	svc := &stubService{}
	client, _ := dial(t, svc)
	ctx := context.Background()
	req := &authgatewayv1.RefreshTokenRequest{RefreshToken: "opaque"}

	svc.err = apperr.New(commonv1.ErrorReason_ERROR_REASON_TOKEN_REVOKED, "the session has ended")
	_, err := client.RefreshToken(ctx, req)
	if e := detail(t, err, codes.Unauthenticated); e.GetReason() != commonv1.ErrorReason_ERROR_REASON_TOKEN_REVOKED {
		t.Fatalf("error = %v", e)
	}

	svc.err = errors.New("postgres: password authentication failed")
	_, err = client.RefreshToken(ctx, req)
	if e := detail(t, err, codes.Internal); e.GetReason() != commonv1.ErrorReason_ERROR_REASON_INTERNAL ||
		e.GetMessage() != "internal error" {
		t.Fatalf("internal error leaked: %v", e)
	}

	svc.err, svc.panic = nil, true
	_, err = client.RefreshToken(ctx, req)
	detail(t, err, codes.Internal)

	svc.panic = false
	_, err = client.RefreshToken(metadata.AppendToOutgoingContext(ctx, auth.HeaderUserID, "42"), req)
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
