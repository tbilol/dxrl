package grpcserver

import (
	"context"

	authgatewayv1 "github.com/tbilol/dxrl/gen/go/authgateway/v1"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tbilol/dxrl/services/auth-gateway/internal/auth"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/domain"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/service"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/token"
)

// Service is the business API the handler exposes (*service.Service).
type Service interface {
	SignIn(ctx context.Context, p auth.Principal, in service.SignInInput) (service.SignInResult, error)
	RequestPhoneCode(ctx context.Context, p auth.Principal, phone string, purpose authgatewayv1.PhoneCodePurpose) (service.Challenge, error)
	SignUpWithPhone(ctx context.Context, p auth.Principal, requestID string, setup service.PhoneSetup, client domain.ClientInfo) (service.SignInResult, error)
	ResetPassword(ctx context.Context, p auth.Principal, setup service.PhoneSetup) error
	LinkSignInMethod(ctx context.Context, p auth.Principal, in service.LinkInput) ([]domain.Credential, error)
	UnlinkSignInMethod(ctx context.Context, p auth.Principal, method authgatewayv1.SignInMethod) ([]domain.Credential, error)
	ListSignInMethods(ctx context.Context, p auth.Principal) ([]domain.Credential, error)
	RefreshToken(ctx context.Context, p auth.Principal, refreshToken string) (service.TokenPair, error)
	SignOut(ctx context.Context, p auth.Principal, scope authgatewayv1.SignOutScope) error
	VerifyToken(ctx context.Context, p auth.Principal, accessToken string) (service.Verified, error)
	JWKS() []token.JSONWebKey
}

// handler maps agrogram.authgateway.v1.AuthGatewayService onto Service.
// Requests arrive validated and with a Principal in the context (see
// interceptors); the decoding here is only about oneofs and enums.
type handler struct {
	authgatewayv1.UnimplementedAuthGatewayServiceServer
	svc Service
}

func principal(ctx context.Context) auth.Principal {
	p, _ := auth.FromContext(ctx)
	return p
}

func (h *handler) SignIn(ctx context.Context, req *authgatewayv1.SignInRequest) (*authgatewayv1.SignInResponse, error) {
	in := service.SignInInput{Client: clientInfo(req.GetClient())}
	switch c := req.GetCredential().(type) {
	case *authgatewayv1.SignInRequest_Google:
		in.Method, in.Token, in.Nonce = domain.MethodGoogle, c.Google.GetIdToken(), c.Google.GetNonce()
	case *authgatewayv1.SignInRequest_Apple:
		in.Method, in.Token, in.Nonce = domain.MethodApple, c.Apple.GetIdentityToken(), c.Apple.GetNonce()
	case *authgatewayv1.SignInRequest_PhonePassword:
		in.Method = domain.MethodPhonePassword
		in.Phone, in.Password = c.PhonePassword.GetPhoneNumber(), c.PhonePassword.GetPassword()
	}
	res, err := h.svc.SignIn(ctx, principal(ctx), in)
	if err != nil {
		return nil, err
	}
	return &authgatewayv1.SignInResponse{
		UserId:       res.UserID.String(),
		SessionId:    res.SessionID.String(),
		Tokens:       tokensProto(res.Tokens),
		IsNewAccount: res.IsNewAccount,
	}, nil
}

func (h *handler) RequestPhoneCode(ctx context.Context, req *authgatewayv1.RequestPhoneCodeRequest) (*authgatewayv1.RequestPhoneCodeResponse, error) {
	ch, err := h.svc.RequestPhoneCode(ctx, principal(ctx), req.GetPhoneNumber(), req.GetPurpose())
	if err != nil {
		return nil, err
	}
	return &authgatewayv1.RequestPhoneCodeResponse{
		ChallengeId:       ch.ID.String(),
		ExpiresAt:         timestamppb.New(ch.ExpiresAt),
		ResendAvailableAt: timestamppb.New(ch.ResendAvailableAt),
	}, nil
}

func (h *handler) SignUpWithPhone(ctx context.Context, req *authgatewayv1.SignUpWithPhoneRequest) (*authgatewayv1.SignUpWithPhoneResponse, error) {
	res, err := h.svc.SignUpWithPhone(ctx, principal(ctx), req.GetRequestId(),
		phoneSetup(req.GetPhonePassword()), clientInfo(req.GetClient()))
	if err != nil {
		return nil, err
	}
	return &authgatewayv1.SignUpWithPhoneResponse{
		UserId:    res.UserID.String(),
		SessionId: res.SessionID.String(),
		Tokens:    tokensProto(res.Tokens),
	}, nil
}

func (h *handler) ResetPassword(ctx context.Context, req *authgatewayv1.ResetPasswordRequest) (*authgatewayv1.ResetPasswordResponse, error) {
	if err := h.svc.ResetPassword(ctx, principal(ctx), phoneSetup(req.GetPhonePassword())); err != nil {
		return nil, err
	}
	return &authgatewayv1.ResetPasswordResponse{}, nil
}

func (h *handler) LinkSignInMethod(ctx context.Context, req *authgatewayv1.LinkSignInMethodRequest) (*authgatewayv1.LinkSignInMethodResponse, error) {
	var in service.LinkInput
	switch m := req.GetMethod().(type) {
	case *authgatewayv1.LinkSignInMethodRequest_Google:
		in.Method, in.Token, in.Nonce = domain.MethodGoogle, m.Google.GetIdToken(), m.Google.GetNonce()
	case *authgatewayv1.LinkSignInMethodRequest_Apple:
		in.Method, in.Token, in.Nonce = domain.MethodApple, m.Apple.GetIdentityToken(), m.Apple.GetNonce()
	case *authgatewayv1.LinkSignInMethodRequest_PhonePassword:
		in.Method, in.Phone = domain.MethodPhonePassword, phoneSetup(m.PhonePassword)
	}
	creds, err := h.svc.LinkSignInMethod(ctx, principal(ctx), in)
	if err != nil {
		return nil, err
	}
	return &authgatewayv1.LinkSignInMethodResponse{Methods: methodsProto(creds)}, nil
}

func (h *handler) UnlinkSignInMethod(ctx context.Context, req *authgatewayv1.UnlinkSignInMethodRequest) (*authgatewayv1.UnlinkSignInMethodResponse, error) {
	creds, err := h.svc.UnlinkSignInMethod(ctx, principal(ctx), req.GetMethod())
	if err != nil {
		return nil, err
	}
	return &authgatewayv1.UnlinkSignInMethodResponse{Methods: methodsProto(creds)}, nil
}

func (h *handler) ListSignInMethods(ctx context.Context, _ *authgatewayv1.ListSignInMethodsRequest) (*authgatewayv1.ListSignInMethodsResponse, error) {
	creds, err := h.svc.ListSignInMethods(ctx, principal(ctx))
	if err != nil {
		return nil, err
	}
	return &authgatewayv1.ListSignInMethodsResponse{Methods: methodsProto(creds)}, nil
}

func (h *handler) RefreshToken(ctx context.Context, req *authgatewayv1.RefreshTokenRequest) (*authgatewayv1.RefreshTokenResponse, error) {
	pair, err := h.svc.RefreshToken(ctx, principal(ctx), req.GetRefreshToken())
	if err != nil {
		return nil, err
	}
	return &authgatewayv1.RefreshTokenResponse{Tokens: tokensProto(pair)}, nil
}

func (h *handler) SignOut(ctx context.Context, req *authgatewayv1.SignOutRequest) (*authgatewayv1.SignOutResponse, error) {
	if err := h.svc.SignOut(ctx, principal(ctx), req.GetScope()); err != nil {
		return nil, err
	}
	return &authgatewayv1.SignOutResponse{}, nil
}

func (h *handler) VerifyToken(ctx context.Context, req *authgatewayv1.VerifyTokenRequest) (*authgatewayv1.VerifyTokenResponse, error) {
	v, err := h.svc.VerifyToken(ctx, principal(ctx), req.GetAccessToken())
	if err != nil {
		return nil, err
	}
	return &authgatewayv1.VerifyTokenResponse{
		Principal: &commonv1.Principal{
			UserId:    v.UserID.String(),
			Roles:     v.Roles,
			SessionId: v.SessionID.String(),
		},
		ExpiresAt: timestamppb.New(v.ExpiresAt),
	}, nil
}

func (h *handler) GetJwks(_ context.Context, _ *authgatewayv1.GetJwksRequest) (*authgatewayv1.GetJwksResponse, error) {
	keys := h.svc.JWKS()
	out := make([]*authgatewayv1.JsonWebKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, &authgatewayv1.JsonWebKey{
			Kid: k.Kid, Kty: k.Kty, Alg: k.Alg, Use: k.Use,
			Crv: k.Crv, X: k.X, Y: k.Y, N: k.N, E: k.E,
		})
	}
	return &authgatewayv1.GetJwksResponse{Keys: out}, nil
}

func clientInfo(pb *authgatewayv1.ClientInfo) domain.ClientInfo {
	return domain.ClientInfo{
		Platform:   pb.GetPlatform(),
		DeviceID:   pb.GetDeviceId(),
		AppVersion: pb.GetAppVersion(),
		DeviceName: pb.GetDeviceName(),
	}
}

func phoneSetup(pb *authgatewayv1.PhonePasswordSetup) service.PhoneSetup {
	return service.PhoneSetup{
		Phone:       pb.GetPhoneNumber(),
		ChallengeID: pb.GetVerification().GetChallengeId(),
		Code:        pb.GetVerification().GetCode(),
		Password:    pb.GetPassword(),
	}
}

func tokensProto(p service.TokenPair) *authgatewayv1.TokenPair {
	return &authgatewayv1.TokenPair{
		AccessToken:           p.AccessToken,
		AccessTokenExpiresAt:  timestamppb.New(p.AccessTokenExpiresAt),
		RefreshToken:          p.RefreshToken,
		RefreshTokenExpiresAt: timestamppb.New(p.RefreshTokenExpiresAt),
	}
}

func methodsProto(creds []domain.Credential) []*authgatewayv1.LinkedSignInMethod {
	out := make([]*authgatewayv1.LinkedSignInMethod, 0, len(creds))
	for _, c := range creds {
		m := &authgatewayv1.LinkedSignInMethod{
			Method:      c.Method,
			DisplayHint: c.DisplayHint,
			LinkedAt:    timestamppb.New(c.LinkedAt),
		}
		if c.LastUsedAt != nil {
			m.LastUsedAt = timestamppb.New(*c.LastUsedAt)
		}
		out = append(out, m)
	}
	return out
}
