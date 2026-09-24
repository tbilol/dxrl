package grpcserver

import (
	"context"
	"time"

	"github.com/google/uuid"
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
	SubmitFarmerVerification(ctx context.Context, p auth.Principal, requestID, contractNumber string) (domain.Verification, error)
	GetFarmerVerification(ctx context.Context, p auth.Principal, userID string) (domain.Verification, error)
	SetAccountPremium(ctx context.Context, p auth.Principal, userID string, premium bool) (domain.Account, error)
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

func (h *handler) SubmitFarmerVerification(ctx context.Context, req *identityv1.SubmitFarmerVerificationRequest) (*identityv1.SubmitFarmerVerificationResponse, error) {
	v, err := h.svc.SubmitFarmerVerification(ctx, principal(ctx), req.GetRequestId(), req.GetLeaseContractNumber())
	if err != nil {
		return nil, err
	}
	return &identityv1.SubmitFarmerVerificationResponse{Verification: verificationProto(v)}, nil
}

func (h *handler) GetFarmerVerification(ctx context.Context, req *identityv1.GetFarmerVerificationRequest) (*identityv1.GetFarmerVerificationResponse, error) {
	v, err := h.svc.GetFarmerVerification(ctx, principal(ctx), req.GetUserId())
	if err != nil {
		return nil, err
	}
	return &identityv1.GetFarmerVerificationResponse{Verification: verificationProto(v)}, nil
}

func (h *handler) SetAccountPremium(ctx context.Context, req *identityv1.SetAccountPremiumRequest) (*identityv1.SetAccountPremiumResponse, error) {
	a, err := h.svc.SetAccountPremium(ctx, principal(ctx), req.GetUserId(), req.GetIsPremium())
	if err != nil {
		return nil, err
	}
	return &identityv1.SetAccountPremiumResponse{Account: accountProto(a)}, nil
}

func accountProto(a domain.Account) *identityv1.Account {
	pb := &identityv1.Account{
		Id:                       a.ID.String(),
		Roles:                    append([]commonv1.Role(nil), a.Roles...),
		Status:                   a.Status,
		Plan:                     a.Plan,
		IsPremium:                a.IsPremium,
		IsVerifiedFarmer:         a.HasRole(domain.RoleFarmer),
		FarmerVerificationStatus: a.FarmerVerificationStatus,
		Version:                  a.Version,
		CreatedAt:                timestamppb.New(a.CreatedAt),
		UpdatedAt:                timestamppb.New(a.UpdatedAt),
	}
	if a.PremiumUntil != nil {
		pb.PremiumUntil = timestamppb.New(*a.PremiumUntil)
	}
	return pb
}

func verificationProto(v domain.Verification) *identityv1.FarmerVerification {
	pb := &identityv1.FarmerVerification{
		Id:                  idString(v.ID),
		UserId:              idString(v.UserID),
		LeaseContractNumber: v.ContractNumber,
		Status:              v.Status,
		RejectionReason:     v.RejectionReason,
		SubmittedAt:         timestamppb.New(v.SubmittedAt),
	}
	if v.DecidedAt != nil {
		pb.DecidedAt = timestamppb.New(*v.DecidedAt)
	}
	if v.Lease != nil {
		// TODO(e-ijara): location and area once e-ijara returns them.
		pb.Lease = &identityv1.LandLease{
			ContractNumber: v.Lease.ContractNumber,
			ValidFrom:      dateProto(v.Lease.ValidFrom),
			ValidUntil:     dateProto(v.Lease.ValidUntil),
		}
	}
	return pb
}

func dateProto(t time.Time) *commonv1.Date {
	y, m, d := t.Date()
	return &commonv1.Date{Year: int32(y), Month: int32(m), Day: int32(d)}
}

func idString(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}
