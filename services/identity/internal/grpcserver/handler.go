package grpcserver

import (
	"context"

	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	identityv1 "github.com/tbilol/dxrl/gen/go/identity/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tbilol/dxrl/services/identity/internal/auth"
	"github.com/tbilol/dxrl/services/identity/internal/domain"
)

// Service is the business API the handler exposes (*service.Service).
type Service interface {
	CreateAccount(ctx context.Context, p auth.Principal, requestID string) (domain.Account, error)
	GetAccount(ctx context.Context, p auth.Principal, userID string) (domain.Account, error)
}

// handler maps agrogram.identity.v1.IdentityService onto Service. Requests
// arrive validated and with a Principal in the context (see interceptors).
type handler struct {
	identityv1.UnimplementedIdentityServiceServer
	svc Service
}

func principal(ctx context.Context) auth.Principal {
	p, _ := auth.FromContext(ctx)
	return p
}

func (h *handler) CreateAccount(ctx context.Context, req *identityv1.CreateAccountRequest) (*identityv1.CreateAccountResponse, error) {
	a, err := h.svc.CreateAccount(ctx, principal(ctx), req.GetRequestId())
	if err != nil {
		return nil, err
	}
	return &identityv1.CreateAccountResponse{Account: accountProto(a)}, nil
}

func (h *handler) GetAccount(ctx context.Context, req *identityv1.GetAccountRequest) (*identityv1.GetAccountResponse, error) {
	a, err := h.svc.GetAccount(ctx, principal(ctx), req.GetUserId())
	if err != nil {
		return nil, err
	}
	return &identityv1.GetAccountResponse{Account: accountProto(a)}, nil
}

func accountProto(a domain.Account) *identityv1.Account {
	return &identityv1.Account{
		Id:        a.ID.String(),
		Roles:     append([]commonv1.Role(nil), a.Roles...),
		Status:    a.Status,
		Version:   a.Version,
		CreatedAt: timestamppb.New(a.CreatedAt),
		UpdatedAt: timestamppb.New(a.UpdatedAt),
	}
}
