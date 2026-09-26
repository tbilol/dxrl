package grpcserver

import (
	"context"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	profilesocialv1 "github.com/tbilol/dxrl/gen/go/profilesocial/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tbilol/dxrl/services/profile-social/internal/apperr"
	"github.com/tbilol/dxrl/services/profile-social/internal/auth"
	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
)

// Service is the business API the handler exposes (*service.Service).
type Service interface {
	CreateProfile(ctx context.Context, p auth.Principal, requestID string, f domain.ProfileFields) (domain.Profile, error)
	GetProfile(ctx context.Context, p auth.Principal, userID string) (domain.Profile, error)
	UpdateProfile(ctx context.Context, p auth.Principal, profileID string, f domain.ProfileFields, mask []string, expectedVersion int64) (domain.Profile, error)
	DeleteProfile(ctx context.Context, p auth.Principal, userID string) error
	FollowUser(ctx context.Context, p auth.Principal, userID string) (domain.Profile, error)
	UnfollowUser(ctx context.Context, p auth.Principal, userID string) (domain.Profile, error)
	ListFollowers(ctx context.Context, p auth.Principal, userID string, page domain.PageRequest) (domain.ProfilePage, error)
	ListFollowing(ctx context.Context, p auth.Principal, userID string, page domain.PageRequest) (domain.ProfilePage, error)
	RateUser(ctx context.Context, p auth.Principal, requestID, transactionID string, score int32, comment string) (domain.Rating, error)
	ListRatings(ctx context.Context, p auth.Principal, userID string, page domain.PageRequest) (domain.RatingPage, error)
	ListUserListings(ctx context.Context, p auth.Principal, userID string, kind commonv1.ListingKind,
		statuses []eventsv1.ListingStatus, page domain.PageRequest) (domain.ListingPage, error)
}

// handler maps agrogram.profilesocial.v1.ProfileSocialService onto Service.
// Requests arrive validated and with a Principal in the context (see the
// interceptors).
type handler struct {
	profilesocialv1.UnimplementedProfileSocialServiceServer
	svc Service
}

func principal(ctx context.Context) auth.Principal {
	p, _ := auth.FromContext(ctx)
	return p
}

func (h *handler) CreateProfile(ctx context.Context, req *profilesocialv1.CreateProfileRequest) (*profilesocialv1.CreateProfileResponse, error) {
	f, err := fields(req.GetProfile())
	if err != nil {
		return nil, err
	}
	prof, err := h.svc.CreateProfile(ctx, principal(ctx), req.GetRequestId(), f)
	if err != nil {
		return nil, err
	}
	return &profilesocialv1.CreateProfileResponse{Profile: profileProto(prof)}, nil
}

func (h *handler) GetProfile(ctx context.Context, req *profilesocialv1.GetProfileRequest) (*profilesocialv1.GetProfileResponse, error) {
	prof, err := h.svc.GetProfile(ctx, principal(ctx), req.GetUserId())
	if err != nil {
		return nil, err
	}
	return &profilesocialv1.GetProfileResponse{Profile: profileProto(prof)}, nil
}

func (h *handler) UpdateProfile(ctx context.Context, req *profilesocialv1.UpdateProfileRequest) (*profilesocialv1.UpdateProfileResponse, error) {
	f, err := fields(req.GetProfile())
	if err != nil {
		return nil, err
	}
	prof, err := h.svc.UpdateProfile(ctx, principal(ctx), req.GetProfile().GetId(), f,
		req.GetUpdateMask().GetPaths(), req.GetExpectedVersion())
	if err != nil {
		return nil, err
	}
	return &profilesocialv1.UpdateProfileResponse{Profile: profileProto(prof)}, nil
}

func (h *handler) DeleteProfile(ctx context.Context, req *profilesocialv1.DeleteProfileRequest) (*profilesocialv1.DeleteProfileResponse, error) {
	if err := h.svc.DeleteProfile(ctx, principal(ctx), req.GetUserId()); err != nil {
		return nil, err
	}
	return &profilesocialv1.DeleteProfileResponse{}, nil
}

func (h *handler) FollowUser(ctx context.Context, req *profilesocialv1.FollowUserRequest) (*profilesocialv1.FollowUserResponse, error) {
	prof, err := h.svc.FollowUser(ctx, principal(ctx), req.GetUserId())
	if err != nil {
		return nil, err
	}
	return &profilesocialv1.FollowUserResponse{Profile: profileProto(prof)}, nil
}

func (h *handler) UnfollowUser(ctx context.Context, req *profilesocialv1.UnfollowUserRequest) (*profilesocialv1.UnfollowUserResponse, error) {
	prof, err := h.svc.UnfollowUser(ctx, principal(ctx), req.GetUserId())
	if err != nil {
		return nil, err
	}
	return &profilesocialv1.UnfollowUserResponse{Profile: profileProto(prof)}, nil
}

func (h *handler) ListFollowers(ctx context.Context, req *profilesocialv1.ListFollowersRequest) (*profilesocialv1.ListFollowersResponse, error) {
	page, err := h.svc.ListFollowers(ctx, principal(ctx), req.GetUserId(), pageRequest(req.GetPage()))
	if err != nil {
		return nil, err
	}
	return &profilesocialv1.ListFollowersResponse{Profiles: profilesProto(page.Profiles), Page: pageResponse(page)}, nil
}

func (h *handler) ListFollowing(ctx context.Context, req *profilesocialv1.ListFollowingRequest) (*profilesocialv1.ListFollowingResponse, error) {
	page, err := h.svc.ListFollowing(ctx, principal(ctx), req.GetUserId(), pageRequest(req.GetPage()))
	if err != nil {
		return nil, err
	}
	return &profilesocialv1.ListFollowingResponse{Profiles: profilesProto(page.Profiles), Page: pageResponse(page)}, nil
}

func (h *handler) RateUser(ctx context.Context, req *profilesocialv1.RateUserRequest) (*profilesocialv1.RateUserResponse, error) {
	r, err := h.svc.RateUser(ctx, principal(ctx), req.GetRequestId(), req.GetTransactionId(), req.GetScore(), req.GetComment())
	if err != nil {
		return nil, err
	}
	return &profilesocialv1.RateUserResponse{Rating: ratingProto(r)}, nil
}

func (h *handler) ListRatings(ctx context.Context, req *profilesocialv1.ListRatingsRequest) (*profilesocialv1.ListRatingsResponse, error) {
	page, err := h.svc.ListRatings(ctx, principal(ctx), req.GetUserId(), pageRequest(req.GetPage()))
	if err != nil {
		return nil, err
	}
	resp := &profilesocialv1.ListRatingsResponse{
		Ratings: make([]*profilesocialv1.Rating, len(page.Ratings)),
		Summary: &eventsv1.RatingSummary{Average: page.Average, Count: page.Count},
		Page:    &commonv1.PageResponse{NextPageToken: page.NextPageToken, TotalSize: &page.Count},
	}
	for i, r := range page.Ratings {
		resp.Ratings[i] = ratingProto(r)
	}
	return resp, nil
}

func (h *handler) ListUserListings(ctx context.Context, req *profilesocialv1.ListUserListingsRequest) (*profilesocialv1.ListUserListingsResponse, error) {
	page, err := h.svc.ListUserListings(ctx, principal(ctx), req.GetUserId(), req.GetKind(), req.GetStatuses(),
		pageRequest(req.GetPage()))
	if err != nil {
		return nil, err
	}
	return &profilesocialv1.ListUserListingsResponse{
		Listings: page.Listings,
		Page:     &commonv1.PageResponse{NextPageToken: page.NextPageToken},
	}, nil
}

// fields reads the user-owned fields of a request profile. Output-only
// fields are ignored; of the avatar only media_id is read.
func fields(pb *profilesocialv1.Profile) (domain.ProfileFields, error) {
	f := domain.ProfileFields{
		DisplayName: pb.GetDisplayName(),
		Bio:         pb.GetBio(),
		Region:      pb.GetRegion(),
		District:    pb.GetDistrict(),
	}
	if raw := pb.GetAvatar().GetMediaId(); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil { // protovalidate checked it; this guards direct callers
			return f, apperr.Validation(&commonv1.FieldViolation{
				Field: "profile.avatar.media_id", Description: "value must be a valid UUID", Rule: "string.uuid",
			})
		}
		f.AvatarMediaID = id
	}
	return f, nil
}

func pageRequest(pb *commonv1.PageRequest) domain.PageRequest {
	return domain.PageRequest{Size: pb.GetPageSize(), Token: pb.GetPageToken()}
}

func pageResponse(page domain.ProfilePage) *commonv1.PageResponse {
	total := page.TotalSize
	return &commonv1.PageResponse{NextPageToken: page.NextPageToken, TotalSize: &total}
}

func profilesProto(ps []domain.Profile) []*profilesocialv1.Profile {
	out := make([]*profilesocialv1.Profile, len(ps))
	for i, p := range ps {
		out[i] = profileProto(p)
	}
	return out
}

func profileProto(p domain.Profile) *profilesocialv1.Profile {
	pb := &profilesocialv1.Profile{
		Id:               p.ID.String(),
		DisplayName:      p.DisplayName,
		Bio:              p.Bio,
		Region:           p.Region,
		District:         p.District,
		Rating:           &eventsv1.RatingSummary{Average: p.RatingAverage(), Count: p.RatingCount},
		DealsCount:       p.DealsCount,
		FollowerCount:    p.FollowerCount,
		FollowingCount:   p.FollowingCount,
		IsVerifiedFarmer: p.Account.IsVerifiedFarmer,
		IsPremium:        p.IsPremium,
		IsFollowedByMe:   p.IsFollowedByMe,
		Version:          p.Version,
		UpdatedAt:        timestamppb.New(p.UpdatedAt),
	}
	if !p.CreatedAt.IsZero() {
		pb.CreatedAt = timestamppb.New(p.CreatedAt)
	}
	if p.AvatarMediaID != uuid.Nil {
		// TODO(media): type, status and URLs come from media-pipeline, which
		// is not built yet; only the id is known here.
		pb.Avatar = &commonv1.MediaRef{MediaId: p.AvatarMediaID.String()}
	}
	return pb
}

func ratingProto(r domain.Rating) *profilesocialv1.Rating {
	return &profilesocialv1.Rating{
		Id:            r.ID.String(),
		TransactionId: r.TransactionID.String(),
		RaterId:       r.RaterID.String(),
		RatedUserId:   r.RatedUserID.String(),
		Score:         r.Score,
		Comment:       r.Comment,
		CreatedAt:     timestamppb.New(r.CreatedAt),
	}
}
