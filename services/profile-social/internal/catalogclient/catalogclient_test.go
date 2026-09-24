package catalogclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	catalogv1 "github.com/tbilol/dxrl/gen/go/catalog/v1"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tbilol/dxrl/services/profile-social/internal/apperr"
	"github.com/tbilol/dxrl/services/profile-social/internal/auth"
	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
)

// stubCatalog is an in-process CatalogService client: it records the
// outgoing metadata and request and answers from its fields.
type stubCatalog struct {
	catalogv1.CatalogServiceClient
	md    metadata.MD
	req   *catalogv1.ListListingsRequest
	resp  *catalogv1.ListListingsResponse
	err   error
	delay time.Duration
}

func (s *stubCatalog) ListListings(ctx context.Context, req *catalogv1.ListListingsRequest, _ ...grpc.CallOption) (*catalogv1.ListListingsResponse, error) {
	s.md, _ = metadata.FromOutgoingContext(ctx)
	s.req = req
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	return s.resp, s.err
}

func TestListUserListingsForwardsTheCallerAndMapsCards(t *testing.T) {
	owner := uuid.New()
	created := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	video := &commonv1.MediaRef{MediaId: uuid.NewString(), Type: commonv1.MediaType_MEDIA_TYPE_VIDEO, ThumbnailUrl: "poster"}
	photo := &commonv1.MediaRef{MediaId: uuid.NewString(), Type: commonv1.MediaType_MEDIA_TYPE_IMAGE}
	listing := &catalogv1.Listing{
		Id: uuid.NewString(), Kind: commonv1.ListingKind_LISTING_KIND_OFFER, OwnerId: owner.String(),
		Product:           commonv1.ProductType_PRODUCT_TYPE_BUGDOY,
		Quantity:          &commonv1.Mass{Grams: 5_000_000, DisplayUnit: commonv1.MassUnit_MASS_UNIT_TONNE},
		RemainingQuantity: &commonv1.Mass{Grams: 3_000_000, DisplayUnit: commonv1.MassUnit_MASS_UNIT_TONNE},
		Price:             &commonv1.UnitPrice{Amount: &commonv1.Money{AmountMinor: 450_000, CurrencyCode: "UZS"}, Unit: commonv1.MassUnit_MASS_UNIT_KILOGRAM},
		Phone:             "+998901234567",
		Address:           &commonv1.Address{Region: commonv1.Region_REGION_JIZZAX, District: "Zomin"},
		Media:             []*commonv1.MediaRef{video, photo},
		Status:            eventsv1.ListingStatus_LISTING_STATUS_ACTIVE,
		CreatedAt:         timestamppb.New(created),
	}
	stub := &stubCatalog{resp: &catalogv1.ListListingsResponse{
		Listings: []*catalogv1.Listing{listing, {Id: uuid.NewString(), Media: []*commonv1.MediaRef{video}}},
		Page:     &commonv1.PageResponse{NextPageToken: "catalog-next"},
	}}
	client := New(stub, time.Second)

	viewer, session := uuid.New(), uuid.New()
	p := auth.Principal{UserID: viewer, Roles: []commonv1.Role{commonv1.Role_ROLE_RESELLER}, SessionID: session.String(),
		RequestID: "req-5", Traceparent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", AcceptLanguage: "ru"}
	page, err := client.ListUserListings(context.Background(), p, domain.ListingQuery{
		OwnerID: owner, Kind: commonv1.ListingKind_LISTING_KIND_OFFER,
		Statuses: []eventsv1.ListingStatus{eventsv1.ListingStatus_LISTING_STATUS_ACTIVE},
		Page:     domain.PageRequest{Size: 10, Token: "catalog-prev"},
	})
	if err != nil {
		t.Fatal(err)
	}

	for k, v := range map[string]string{
		auth.HeaderCaller: auth.ServiceName, auth.HeaderRequestID: "req-5", auth.HeaderUserID: viewer.String(),
		auth.HeaderRoles: "RESELLER", auth.HeaderPremium: "false", auth.HeaderSessionID: session.String(),
		auth.HeaderAcceptLanguage: "ru", auth.HeaderTraceparent: p.Traceparent,
	} {
		if got := stub.md.Get(k); len(got) != 1 || got[0] != v {
			t.Fatalf("metadata %s = %v, want %q", k, got, v)
		}
	}
	f := stub.req.GetFilter()
	if f.GetOwnerId() != owner.String() || f.GetKind() != commonv1.ListingKind_LISTING_KIND_OFFER || len(f.GetStatuses()) != 1 ||
		stub.req.GetPage().GetPageSize() != 10 || stub.req.GetPage().GetPageToken() != "catalog-prev" {
		t.Fatalf("request = %v", stub.req)
	}

	if page.NextPageToken != "catalog-next" || len(page.Listings) != 2 {
		t.Fatalf("page = %+v", page)
	}
	c := page.Listings[0]
	if c.GetListingId() != listing.GetId() || c.GetKind() != listing.GetKind() || c.GetProduct() != listing.GetProduct() ||
		c.GetQuantity().GetGrams() != 5_000_000 || c.GetRemainingQuantity().GetGrams() != 3_000_000 ||
		c.GetPrice().GetAmount().GetAmountMinor() != 450_000 || c.GetRegion() != commonv1.Region_REGION_JIZZAX ||
		c.GetDistrict() != "Zomin" || c.GetStatus() != eventsv1.ListingStatus_LISTING_STATUS_ACTIVE ||
		!c.GetCreatedAt().AsTime().Equal(created) || c.GetCover() != photo {
		t.Fatalf("card = %v (the cover is the first photo)", c)
	}
	if page.Listings[1].GetCover() != video {
		t.Fatalf("without photos the cover is the first media (a video poster): %v", page.Listings[1])
	}
}

func TestUnavailableCatalogIsDegradable(t *testing.T) {
	q := domain.ListingQuery{OwnerID: uuid.New()}
	for name, stub := range map[string]*stubCatalog{
		"unavailable":   {err: status.Error(codes.Unavailable, "connection refused")},
		"unimplemented": {err: status.Error(codes.Unimplemented, "not yet")},
		"timeout":       {delay: time.Second},
	} {
		_, err := New(stub, 20*time.Millisecond).ListUserListings(context.Background(), auth.Principal{RequestID: "r"}, q)
		if !errors.Is(err, domain.ErrDependencyUnavailable) {
			t.Fatalf("%s: err = %v, want ErrDependencyUnavailable", name, err)
		}
		if ae := apperr.From(err); ae.Code != codes.Unavailable || ae.Reason != commonv1.ErrorReason_ERROR_REASON_DEPENDENCY_UNAVAILABLE {
			t.Fatalf("%s: error = %v", name, ae)
		}
	}

	stub := &stubCatalog{err: status.Error(codes.InvalidArgument, "bad page token")}
	_, err := New(stub, time.Second).ListUserListings(context.Background(), auth.Principal{RequestID: "r"}, q)
	if errors.Is(err, domain.ErrDependencyUnavailable) {
		t.Fatal("catalog's own errors must not be degraded")
	}
	if ae := apperr.From(err); ae.Code != codes.InvalidArgument {
		t.Fatalf("error = %v", ae)
	}
}

func TestDial(t *testing.T) {
	if _, err := Dial("", time.Second); err == nil {
		t.Fatal("an empty address must be refused")
	}
	// Nothing listens on port 1: the lazy connection fails on the call.
	client, err := Dial("127.0.0.1:1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = client.ListUserListings(context.Background(), auth.Principal{RequestID: "r"}, domain.ListingQuery{OwnerID: uuid.New()})
	if !errors.Is(err, domain.ErrDependencyUnavailable) {
		t.Fatalf("err = %v, want ErrDependencyUnavailable", err)
	}
}
