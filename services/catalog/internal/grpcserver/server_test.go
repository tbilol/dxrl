package grpcserver

import (
	"context"
	"log/slog"
	"net"
	"slices"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	catalogv1 "github.com/tbilol/dxrl/gen/go/catalog/v1"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpchealth "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/tbilol/dxrl/services/catalog/internal/apperr"
	"github.com/tbilol/dxrl/services/catalog/internal/auth"
	"github.com/tbilol/dxrl/services/catalog/internal/domain"
)

// stubService records what it was called with and answers from its fields.
type stubService struct {
	got      auth.Principal
	calls    int
	err      error
	panic    bool
	listing  domain.Listing
	fields   domain.ListingFields
	kind     commonv1.ListingKind
	id       string
	mask     []string
	version  int64
	filter   domain.ListingFilter
	query    string
	sortBy   catalogv1.ListingSortField
	dir      commonv1.SortDirection
	page     domain.PageRequest
	category commonv1.ProductCategory
	types    []domain.ProductType
}

func (s *stubService) answer(p auth.Principal) error {
	s.got, s.calls = p, s.calls+1
	if s.panic {
		panic("boom")
	}
	return s.err
}

func (s *stubService) CreateListing(_ context.Context, p auth.Principal, requestID string, kind commonv1.ListingKind,
	f domain.ListingFields,
) (domain.Listing, error) {
	s.id, s.kind, s.fields = requestID, kind, f
	return s.listing, s.answer(p)
}

func (s *stubService) GetListing(_ context.Context, p auth.Principal, id string) (domain.Listing, error) {
	s.id = id
	return s.listing, s.answer(p)
}

func (s *stubService) UpdateListing(_ context.Context, p auth.Principal, id string, f domain.ListingFields, mask []string,
	v int64,
) (domain.Listing, error) {
	s.id, s.fields, s.mask, s.version = id, f, mask, v
	return s.listing, s.answer(p)
}

func (s *stubService) DeleteListing(_ context.Context, p auth.Principal, id string, v int64) error {
	s.id, s.version = id, v
	return s.answer(p)
}

func (s *stubService) ListListings(_ context.Context, p auth.Principal, f domain.ListingFilter, page domain.PageRequest) (domain.ListingPage, error) {
	s.filter, s.page = f, page
	return domain.ListingPage{Listings: []domain.Listing{s.listing}, NextPageToken: "next"}, s.answer(p)
}

func (s *stubService) SearchListings(_ context.Context, p auth.Principal, query string, f domain.ListingFilter,
	sortBy catalogv1.ListingSortField, dir commonv1.SortDirection, page domain.PageRequest,
) (domain.ListingPage, error) {
	s.query, s.filter, s.sortBy, s.dir, s.page = query, f, sortBy, dir, page
	return domain.ListingPage{Listings: []domain.Listing{s.listing}}, s.answer(p)
}

func (s *stubService) ListProductTypes(_ context.Context, p auth.Principal, category commonv1.ProductCategory) ([]domain.ProductType, error) {
	s.category = category
	return s.types, s.answer(p)
}

func dial(t *testing.T, svc Service) (catalogv1.CatalogServiceClient, healthpb.HealthClient) {
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
	return catalogv1.NewCatalogServiceClient(conn), healthpb.NewHealthClient(conn)
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

func requestListing() *catalogv1.Listing {
	return &catalogv1.Listing{
		Kind:     commonv1.ListingKind_LISTING_KIND_OFFER,
		Product:  commonv1.ProductType_PRODUCT_TYPE_POMIDOR,
		Quantity: &commonv1.Mass{Grams: 3_500_000, DisplayUnit: commonv1.MassUnit_MASS_UNIT_TONNE},
		Price: &commonv1.UnitPrice{Amount: &commonv1.Money{AmountMinor: 700_000, CurrencyCode: "UZS"},
			Unit: commonv1.MassUnit_MASS_UNIT_KILOGRAM},
		Phone: "+998911112233",
		Address: &commonv1.Address{Region: commonv1.Region_REGION_FARGONA, District: "Quva", AddressLine: "Bozor yonida",
			Location: &commonv1.GeoPoint{Latitude: 40.5, Longitude: 72.1}},
		Notes: "Issiqxona pomidori",
		Media: []*commonv1.MediaRef{{MediaId: uuid.NewString(), Type: commonv1.MediaType_MEDIA_TYPE_VIDEO, Url: "https://evil"}},
		// Output only: ignored.
		Id: uuid.NewString(), OwnerId: uuid.NewString(), Status: eventsv1.ListingStatus_LISTING_STATUS_BLOCKED,
		ViewsCount: 99, Version: 42,
	}
}

func sampleListing() domain.Listing {
	now := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	return domain.Listing{
		ID: uuid.New(), Kind: domain.KindOffer, OwnerID: uuid.New(),
		ListingFields: domain.ListingFields{
			Product:  commonv1.ProductType_PRODUCT_TYPE_POMIDOR,
			Quantity: domain.Mass{Grams: 3_500_000, Unit: domain.UnitTonne},
			Price:    &domain.Price{AmountMinor: 700_000, Currency: "UZS", Unit: domain.UnitKilogram},
			Phone:    "+998911112233",
			Address:  domain.Address{Region: commonv1.Region_REGION_FARGONA, District: "Quva"},
			MediaIDs: []uuid.UUID{uuid.New()},
		},
		Status: domain.StatusActive, ViewsCount: 12, Version: 3, CreatedAt: now, UpdatedAt: now,
	}
}

func TestCreateListingMapsRequestAndResponse(t *testing.T) {
	svc := &stubService{listing: sampleListing()}
	client, _ := dial(t, svc)
	user, reqID := uuid.New(), uuid.NewString()
	ctx := metadata.AppendToOutgoingContext(context.Background(),
		auth.HeaderUserID, user.String(), auth.HeaderRoles, "ADMIN",
		auth.HeaderRequestID, "req-42")
	var header metadata.MD
	in := requestListing()
	resp, err := client.CreateListing(ctx, &catalogv1.CreateListingRequest{RequestId: reqID, Listing: in}, grpc.Header(&header))
	if err != nil {
		t.Fatal(err)
	}
	if svc.got.UserID != user || !svc.got.HasRole(domain.RoleAdmin) || svc.got.RequestID != "req-42" ||
		svc.id != reqID || svc.kind != domain.KindOffer {
		t.Fatalf("principal = %+v, request id %s, kind %s", svc.got, svc.id, svc.kind)
	}
	if got := header.Get(auth.HeaderRequestID); len(got) != 1 || got[0] != "req-42" {
		t.Fatalf("x-request-id header = %v", got)
	}
	f := svc.fields
	if f.Product != commonv1.ProductType_PRODUCT_TYPE_POMIDOR || f.Quantity != (domain.Mass{Grams: 3_500_000, Unit: domain.UnitTonne}) ||
		*f.Price != (domain.Price{AmountMinor: 700_000, Currency: "UZS", Unit: domain.UnitKilogram}) || f.Phone != in.Phone ||
		f.Address.District != "Quva" || f.Address.Line != "Bozor yonida" || f.Address.Location.Latitude != 40.5 ||
		f.Notes != in.Notes || len(f.MediaIDs) != 1 || f.MediaIDs[0].String() != in.Media[0].MediaId {
		t.Fatalf("fields = %+v", f)
	}

	l := svc.listing
	out := resp.GetListing()
	if out.GetId() != l.ID.String() || out.GetOwnerId() != l.OwnerID.String() || out.GetOwner().GetUserId() != l.OwnerID.String() ||
		out.GetOwner().GetDisplayName() != "" || out.GetPhone() != l.Phone || out.GetStatus() != domain.StatusActive ||
		out.GetViewsCount() != 12 || out.GetVersion() != 3 || out.GetPrice().GetAmount().GetCurrencyCode() != "UZS" ||
		len(out.GetMedia()) != 1 || out.GetMedia()[0].GetType() != commonv1.MediaType_MEDIA_TYPE_UNSPECIFIED ||
		!out.GetCreatedAt().AsTime().Equal(l.CreatedAt) {
		t.Fatalf("response = %v", out)
	}
}

func TestValidationInterceptor(t *testing.T) {
	svc := &stubService{listing: sampleListing()}
	client, _ := dial(t, svc)
	ctx := context.Background()
	id := uuid.NewString()
	without := func(change func(l *catalogv1.Listing)) *catalogv1.Listing {
		l := requestListing()
		l.Id, l.OwnerId = "", ""
		change(l)
		return l
	}

	cases := map[string]struct {
		call  func() error
		field string
	}{
		"create without phone": {func() error {
			_, err := client.CreateListing(ctx, &catalogv1.CreateListingRequest{RequestId: id,
				Listing: without(func(l *catalogv1.Listing) { l.Phone = "" })})
			return err
		}, "listing"},
		"create with zero quantity": {func() error {
			_, err := client.CreateListing(ctx, &catalogv1.CreateListingRequest{RequestId: id,
				Listing: without(func(l *catalogv1.Listing) { l.Quantity.Grams = 0 })})
			return err
		}, "listing.quantity"},
		"create with a USD price": {func() error {
			_, err := client.CreateListing(ctx, &catalogv1.CreateListingRequest{RequestId: id,
				Listing: without(func(l *catalogv1.Listing) { l.Price.Amount.CurrencyCode = "USD" })})
			return err
		}, "listing.price"},
		"update with status in the mask": {func() error {
			_, err := client.UpdateListing(ctx, &catalogv1.UpdateListingRequest{Listing: &catalogv1.Listing{Id: id},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"status"}}})
			return err
		}, "update_mask"},
		"get with a bad id": {func() error {
			_, err := client.GetListing(ctx, &catalogv1.GetListingRequest{ListingId: "42"})
			return err
		}, "listing_id"},
		"page too large": {func() error {
			_, err := client.ListListings(ctx, &catalogv1.ListListingsRequest{Page: &commonv1.PageRequest{PageSize: 101}})
			return err
		}, "page.page_size"},
		"unspecified status": {func() error {
			_, err := client.ListListings(ctx, &catalogv1.ListListingsRequest{Filter: &catalogv1.ListingFilter{
				Statuses: []eventsv1.ListingStatus{eventsv1.ListingStatus_LISTING_STATUS_UNSPECIFIED}}})
			return err
		}, "filter.statuses[0]"},
		"query too long": {func() error {
			_, err := client.SearchListings(ctx, &catalogv1.SearchListingsRequest{Query: string(make([]byte, 201))})
			return err
		}, "query"},
	}
	for name, tc := range cases {
		e := detail(t, tc.call(), codes.InvalidArgument)
		if e.GetReason() != commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED || len(e.GetFieldViolations()) == 0 ||
			e.GetFieldViolations()[0].GetField() != tc.field {
			t.Errorf("%s: error = %v", name, e)
		}
	}
	if svc.calls != 0 {
		t.Fatalf("service called %d times; invalid requests must not reach it", svc.calls)
	}
}

func TestHandlerPassesQueriesThrough(t *testing.T) {
	svc := &stubService{listing: sampleListing(), types: []domain.ProductType{{
		Type: commonv1.ProductType_PRODUCT_TYPE_BUGDOY, Key: "bugdoy", NameUZ: "Bug'doy", NameRU: "Пшеница",
		Category: commonv1.ProductCategory_PRODUCT_CATEGORY_GRAIN_INDUSTRIAL, SortOrder: 10, Retired: true,
	}}}
	client, _ := dial(t, svc)
	ctx := context.Background()
	owner := uuid.NewString()

	list, err := client.ListListings(ctx, &catalogv1.ListListingsRequest{
		Filter: &catalogv1.ListingFilter{OwnerId: owner, Kind: domain.KindBuyRequest,
			Statuses:    []eventsv1.ListingStatus{domain.StatusClosed},
			MinQuantity: &commonv1.Mass{Grams: 1000},
			MinPrice: &commonv1.UnitPrice{Amount: &commonv1.Money{AmountMinor: 1, CurrencyCode: "UZS"},
				Unit: commonv1.MassUnit_MASS_UNIT_KILOGRAM}},
		Page: &commonv1.PageRequest{PageSize: 7, PageToken: "tok"},
	})
	if err != nil || len(list.GetListings()) != 1 || list.GetPage().GetNextPageToken() != "next" || list.GetPage().TotalSize != nil {
		t.Fatalf("list = %v, %v", list, err)
	}
	if svc.filter.OwnerID != owner || svc.filter.Kind != domain.KindBuyRequest || svc.filter.MinQuantity.Grams != 1000 ||
		svc.filter.MinPrice.AmountMinor != 1 || svc.filter.MaxPrice != nil ||
		!slices.Equal(svc.filter.Statuses, []eventsv1.ListingStatus{domain.StatusClosed}) || svc.page != (domain.PageRequest{Size: 7, Token: "tok"}) {
		t.Fatalf("filter = %+v, page = %+v", svc.filter, svc.page)
	}

	if _, err := client.SearchListings(ctx, &catalogv1.SearchListingsRequest{Query: "piyoz",
		SortBy: catalogv1.ListingSortField_LISTING_SORT_FIELD_PRICE, SortDirection: commonv1.SortDirection_SORT_DIRECTION_ASC,
	}); err != nil || svc.query != "piyoz" || svc.sortBy != catalogv1.ListingSortField_LISTING_SORT_FIELD_PRICE ||
		svc.dir != commonv1.SortDirection_SORT_DIRECTION_ASC {
		t.Fatalf("search: %v (%+v)", err, svc)
	}

	id := uuid.NewString()
	if _, err := client.UpdateListing(ctx, &catalogv1.UpdateListingRequest{
		Listing:    &catalogv1.Listing{Id: id, Notes: "yangi", Phone: "+998900000000"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"notes", "phone"}}, ExpectedVersion: 4,
	}); err != nil || svc.id != id || !slices.Equal(svc.mask, []string{"notes", "phone"}) || svc.version != 4 || svc.fields.Notes != "yangi" {
		t.Fatalf("update: %v (%+v)", err, svc)
	}
	if _, err := client.DeleteListing(ctx, &catalogv1.DeleteListingRequest{ListingId: id, ExpectedVersion: 5}); err != nil ||
		svc.version != 5 {
		t.Fatalf("delete: %v", err)
	}

	types, err := client.ListProductTypes(ctx, &catalogv1.ListProductTypesRequest{Category: commonv1.ProductCategory_PRODUCT_CATEGORY_NUT})
	if err != nil || svc.category != commonv1.ProductCategory_PRODUCT_CATEGORY_NUT || len(types.GetProductTypes()) != 1 {
		t.Fatalf("product types = %v, %v", types, err)
	}
	pt := types.GetProductTypes()[0]
	if pt.GetKey() != "bugdoy" || pt.GetName().GetUz() != "Bug'doy" || pt.GetName().GetRu() != "Пшеница" || !pt.GetIsRetired() ||
		pt.GetSortOrder() != 10 || pt.GetCategory() != commonv1.ProductCategory_PRODUCT_CATEGORY_GRAIN_INDUSTRIAL {
		t.Fatalf("product type = %v", pt)
	}
}

func TestErrorsCarryOneDetail(t *testing.T) {
	svc := &stubService{err: apperr.New(commonv1.ErrorReason_ERROR_REASON_PLAN_LIMIT_REACHED, "quota",
		"plan", "default", "listing_kind", "OFFER", "limit", "5")}
	client, _ := dial(t, svc)
	_, err := client.GetListing(context.Background(), &catalogv1.GetListingRequest{ListingId: uuid.NewString()})
	e := detail(t, err, codes.ResourceExhausted)
	if e.GetReason() != commonv1.ErrorReason_ERROR_REASON_PLAN_LIMIT_REACHED || e.GetMetadata()["limit"] != "5" {
		t.Fatalf("error = %v", e)
	}

	// A malformed user id never reaches the service.
	svc.err = nil
	ctx := metadata.AppendToOutgoingContext(context.Background(), auth.HeaderUserID, "not-a-uuid")
	_, err = client.GetListing(ctx, &catalogv1.GetListingRequest{ListingId: uuid.NewString()})
	detail(t, err, codes.Unauthenticated)
	if svc.calls != 1 {
		t.Fatalf("calls = %d", svc.calls)
	}

	// A panic is INTERNAL with a generic message.
	svc.panic = true
	_, err = client.GetListing(context.Background(), &catalogv1.GetListingRequest{ListingId: uuid.NewString()})
	if e := detail(t, err, codes.Internal); e.GetMessage() != "internal error" {
		t.Fatalf("panic error = %v", e)
	}
}

func TestHealthIsRegistered(t *testing.T) {
	_, health := dial(t, &stubService{})
	// Nothing set the status: the health server knows the overall server only.
	resp, err := health.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health = %v, %v", resp, err)
	}
	if ServiceName != "agrogram.catalog.v1.CatalogService" {
		t.Fatalf("ServiceName = %q", ServiceName)
	}
}
