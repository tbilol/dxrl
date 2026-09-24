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
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	profilesocialv1 "github.com/tbilol/dxrl/gen/go/profilesocial/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpchealth "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/tbilol/dxrl/services/profile-social/internal/apperr"
	"github.com/tbilol/dxrl/services/profile-social/internal/auth"
	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
)

// stubService records what it was called with and answers from its fields.
type stubService struct {
	got      auth.Principal
	calls    int
	err      error
	panic    bool
	prof     domain.Profile
	fields   domain.ProfileFields
	mask     []string
	version  int64
	userID   string
	page     domain.PageRequest
	statuses []eventsv1.ListingStatus
	kind     commonv1.ListingKind
	ratings  domain.RatingPage
	listings domain.ListingPage
}

func (s *stubService) answer(p auth.Principal) error {
	s.got, s.calls = p, s.calls+1
	if s.panic {
		panic("boom")
	}
	return s.err
}

func (s *stubService) CreateProfile(_ context.Context, p auth.Principal, _ string, f domain.ProfileFields) (domain.Profile, error) {
	s.fields = f
	return s.prof, s.answer(p)
}

func (s *stubService) GetProfile(_ context.Context, p auth.Principal, userID string) (domain.Profile, error) {
	s.userID = userID
	return s.prof, s.answer(p)
}

func (s *stubService) UpdateProfile(_ context.Context, p auth.Principal, id string, f domain.ProfileFields, mask []string, v int64) (domain.Profile, error) {
	s.userID, s.fields, s.mask, s.version = id, f, mask, v
	return s.prof, s.answer(p)
}

func (s *stubService) DeleteProfile(_ context.Context, p auth.Principal, userID string) error {
	s.userID = userID
	return s.answer(p)
}

func (s *stubService) FollowUser(_ context.Context, p auth.Principal, userID string) (domain.Profile, error) {
	s.userID = userID
	return s.prof, s.answer(p)
}

func (s *stubService) UnfollowUser(_ context.Context, p auth.Principal, userID string) (domain.Profile, error) {
	s.userID = userID
	return s.prof, s.answer(p)
}

func (s *stubService) ListFollowers(_ context.Context, p auth.Principal, userID string, page domain.PageRequest) (domain.ProfilePage, error) {
	s.userID, s.page = userID, page
	return domain.ProfilePage{Profiles: []domain.Profile{s.prof}, NextPageToken: "next", TotalSize: 9}, s.answer(p)
}

func (s *stubService) ListFollowing(_ context.Context, p auth.Principal, userID string, page domain.PageRequest) (domain.ProfilePage, error) {
	s.userID, s.page = userID, page
	return domain.ProfilePage{}, s.answer(p)
}

func (s *stubService) RateUser(_ context.Context, p auth.Principal, _, _ string, _ int32, _ string) (domain.Rating, error) {
	return domain.Rating{ID: uuid.New(), Score: 5, CreatedAt: time.Now()}, s.answer(p)
}

func (s *stubService) ListRatings(_ context.Context, p auth.Principal, userID string, page domain.PageRequest) (domain.RatingPage, error) {
	s.userID, s.page = userID, page
	return s.ratings, s.answer(p)
}

func (s *stubService) ListUserListings(_ context.Context, p auth.Principal, userID string, kind commonv1.ListingKind,
	statuses []eventsv1.ListingStatus, page domain.PageRequest,
) (domain.ListingPage, error) {
	s.userID, s.kind, s.statuses, s.page = userID, kind, statuses, page
	return s.listings, s.answer(p)
}

func dial(t *testing.T, svc Service) (profilesocialv1.ProfileSocialServiceClient, healthpb.HealthClient) {
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
	return profilesocialv1.NewProfileSocialServiceClient(conn), healthpb.NewHealthClient(conn)
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
	id := uuid.NewString()

	cases := map[string]struct {
		call  func() error
		field string
	}{
		"create without display name": {func() error {
			_, err := client.CreateProfile(ctx, &profilesocialv1.CreateProfileRequest{RequestId: id, Profile: &profilesocialv1.Profile{}})
			return err
		}, "profile"},
		"get with a bad id": {func() error {
			_, err := client.GetProfile(ctx, &profilesocialv1.GetProfileRequest{UserId: "42"})
			return err
		}, "user_id"},
		"update with an unknown mask path": {func() error {
			_, err := client.UpdateProfile(ctx, &profilesocialv1.UpdateProfileRequest{
				Profile: &profilesocialv1.Profile{Id: id}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"is_premium"}}})
			return err
		}, "update_mask"},
		"page too large": {func() error {
			_, err := client.ListFollowers(ctx, &profilesocialv1.ListFollowersRequest{UserId: id, Page: &commonv1.PageRequest{PageSize: 101}})
			return err
		}, "page.page_size"},
		"score out of range": {func() error {
			_, err := client.RateUser(ctx, &profilesocialv1.RateUserRequest{RequestId: id, TransactionId: id, Score: 6})
			return err
		}, "score"},
		"unspecified listing status": {func() error {
			_, err := client.ListUserListings(ctx, &profilesocialv1.ListUserListingsRequest{UserId: id,
				Statuses: []eventsv1.ListingStatus{eventsv1.ListingStatus_LISTING_STATUS_UNSPECIFIED}})
			return err
		}, "statuses[0]"},
	}
	for name, tc := range cases {
		e := detail(t, tc.call(), codes.InvalidArgument)
		if e.GetReason() != commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED || len(e.GetFieldViolations()) == 0 ||
			e.GetFieldViolations()[0].GetField() != tc.field {
			t.Errorf("%s: error = %v", name, e)
		}
	}
	// Empty user_id is allowed on DeleteProfile (the caller's).
	if _, err := client.DeleteProfile(ctx, &profilesocialv1.DeleteProfileRequest{}); err != nil {
		t.Fatal(err)
	}
	if svc.calls != 1 {
		t.Fatalf("service called %d times, want 1 (invalid requests must not reach it)", svc.calls)
	}
}

func TestPrincipalAndResponseMapping(t *testing.T) {
	user, avatar := uuid.New(), uuid.New()
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	svc := &stubService{prof: domain.Profile{
		ID: user, Status: domain.ProfileActive, Version: 4, CreatedAt: now, UpdatedAt: now,
		ProfileFields: domain.ProfileFields{DisplayName: "Nodira", AvatarMediaID: avatar, Region: commonv1.Region_REGION_NAVOIY},
		RatingCount:   4, RatingSum: 18, FollowerCount: 3, IsFollowedByMe: true, IsPremium: true,
		Account: domain.Account{IsVerifiedFarmer: true},
	}}
	client, _ := dial(t, svc)
	ctx := metadata.AppendToOutgoingContext(context.Background(),
		auth.HeaderUserID, uuid.NewString(), auth.HeaderRoles, "RESELLER,ADMIN", auth.HeaderRequestID, "req-42")

	var header metadata.MD
	resp, err := client.GetProfile(ctx, &profilesocialv1.GetProfileRequest{UserId: user.String()}, grpc.Header(&header))
	if err != nil {
		t.Fatal(err)
	}
	if !svc.got.Staff() || svc.got.RequestID != "req-42" || svc.userID != user.String() {
		t.Fatalf("principal = %+v, user id %q", svc.got, svc.userID)
	}
	if got := header.Get(auth.HeaderRequestID); len(got) != 1 || got[0] != "req-42" {
		t.Fatalf("x-request-id header = %v", got)
	}
	p := resp.GetProfile()
	if p.GetId() != user.String() || p.GetDisplayName() != "Nodira" || p.GetAvatar().GetMediaId() != avatar.String() ||
		p.GetRating().GetAverage() != 4.5 || p.GetRating().GetCount() != 4 || p.GetFollowerCount() != 3 ||
		!p.GetIsFollowedByMe() || !p.GetIsPremium() || !p.GetIsVerifiedFarmer() || p.GetVersion() != 4 ||
		!p.GetCreatedAt().AsTime().Equal(now) || p.GetRegion() != commonv1.Region_REGION_NAVOIY {
		t.Fatalf("profile = %v", p)
	}

	// Request fields reach the service.
	_, err = client.CreateProfile(ctx, &profilesocialv1.CreateProfileRequest{RequestId: uuid.NewString(), Profile: &profilesocialv1.Profile{
		DisplayName: "Nodira", Avatar: &commonv1.MediaRef{MediaId: avatar.String()}, Bio: "bio", District: "Karmana",
	}})
	if err != nil || svc.fields.AvatarMediaID != avatar || svc.fields.District != "Karmana" || svc.fields.Bio != "bio" {
		t.Fatalf("create fields = %+v, %v", svc.fields, err)
	}
	_, err = client.UpdateProfile(ctx, &profilesocialv1.UpdateProfileRequest{
		Profile: &profilesocialv1.Profile{Id: user.String(), Bio: "new"}, ExpectedVersion: 4,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"bio", "avatar"}},
	})
	if err != nil || svc.userID != user.String() || len(svc.mask) != 2 || svc.version != 4 || svc.fields.AvatarMediaID != uuid.Nil {
		t.Fatalf("update = mask %v version %d fields %+v, %v", svc.mask, svc.version, svc.fields, err)
	}
	lf, err := client.ListFollowers(ctx, &profilesocialv1.ListFollowersRequest{UserId: user.String(),
		Page: &commonv1.PageRequest{PageSize: 7, PageToken: "tok"}})
	if err != nil || svc.page.Size != 7 || svc.page.Token != "tok" || lf.GetPage().GetNextPageToken() != "next" ||
		lf.GetPage().GetTotalSize() != 9 || len(lf.GetProfiles()) != 1 {
		t.Fatalf("list followers = %v (page %+v), %v", lf, svc.page, err)
	}
	svc.ratings = domain.RatingPage{Ratings: []domain.Rating{{ID: uuid.New(), Score: 4, CreatedAt: now}}, Average: 4, Count: 1}
	lr, err := client.ListRatings(ctx, &profilesocialv1.ListRatingsRequest{UserId: user.String()})
	if err != nil || lr.GetSummary().GetCount() != 1 || lr.GetSummary().GetAverage() != 4 || len(lr.GetRatings()) != 1 ||
		lr.GetPage().GetTotalSize() != 1 {
		t.Fatalf("list ratings = %v, %v", lr, err)
	}
	card := &profilesocialv1.ListingCard{ListingId: uuid.NewString(), Status: domain.ListingActive}
	svc.listings = domain.ListingPage{Listings: []*profilesocialv1.ListingCard{card}, NextPageToken: "catalog"}
	ll, err := client.ListUserListings(ctx, &profilesocialv1.ListUserListingsRequest{UserId: user.String(),
		Kind: commonv1.ListingKind_LISTING_KIND_BUY_REQUEST, Statuses: []eventsv1.ListingStatus{domain.ListingActive}})
	if err != nil || len(ll.GetListings()) != 1 || ll.GetPage().GetNextPageToken() != "catalog" ||
		svc.kind != commonv1.ListingKind_LISTING_KIND_BUY_REQUEST || len(svc.statuses) != 1 {
		t.Fatalf("list user listings = %v, %v", ll, err)
	}
}

func TestErrorsCarryOneCommonError(t *testing.T) {
	svc := &stubService{}
	client, _ := dial(t, svc)
	ctx := context.Background()
	req := &profilesocialv1.FollowUserRequest{UserId: uuid.NewString()}

	svc.err = apperr.New(commonv1.ErrorReason_ERROR_REASON_SELF_ACTION_NOT_ALLOWED, "users cannot follow themselves")
	_, err := client.FollowUser(ctx, req)
	if e := detail(t, err, codes.InvalidArgument); e.GetReason() != commonv1.ErrorReason_ERROR_REASON_SELF_ACTION_NOT_ALLOWED {
		t.Fatalf("error = %v", e)
	}
	svc.err = apperr.New(commonv1.ErrorReason_ERROR_REASON_ALREADY_RATED, "rated")
	_, err = client.FollowUser(ctx, req)
	if e := detail(t, err, codes.AlreadyExists); e.GetReason() != commonv1.ErrorReason_ERROR_REASON_ALREADY_RATED {
		t.Fatalf("error = %v", e)
	}
	svc.err = errors.New("postgres: password authentication failed")
	_, err = client.FollowUser(ctx, req)
	if e := detail(t, err, codes.Internal); e.GetMessage() != "internal error" {
		t.Fatalf("internal error leaked: %v", e)
	}
	svc.err, svc.panic = nil, true
	_, err = client.FollowUser(ctx, req)
	detail(t, err, codes.Internal)
	svc.panic = false
	_, err = client.FollowUser(metadata.AppendToOutgoingContext(ctx, auth.HeaderUserID, "42"), req)
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
	if ServiceName != "agrogram.profilesocial.v1.ProfileSocialService" {
		t.Fatalf("service name = %q", ServiceName)
	}
}
