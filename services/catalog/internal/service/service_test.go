package service_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/tbilol/dxrl/services/catalog/internal/apperr"
	"github.com/tbilol/dxrl/services/catalog/internal/auth"
	"github.com/tbilol/dxrl/services/catalog/internal/domain"
	"github.com/tbilol/dxrl/services/catalog/internal/events"
	"github.com/tbilol/dxrl/services/catalog/internal/service"
)

var ctx = context.Background()

// testClock advances one second per reading, so listings get distinct,
// ordered created_at values.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Second)
	return c.now
}

type env struct {
	svc     *service.Service
	store   *fakeStore
	views   *fakeViews
	limiter *fakeLimiter
}

func newEnv(opts ...service.Option) env {
	e := env{store: newFakeStore(), views: &fakeViews{}, limiter: &fakeLimiter{allowed: true}}
	clk := &testClock{now: time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC)}
	e.svc = service.New(e.store, e.limiter, e.views, append([]service.Option{service.WithClock(clk.Now)}, opts...)...)
	return e
}

func user(roles ...commonv1.Role) auth.Principal {
	return auth.Principal{UserID: uuid.Must(uuid.NewV7()), Roles: roles, RequestID: uuid.NewString()}
}

func farmer() auth.Principal   { return user(domain.RoleReseller, domain.RoleFarmer) }
func reseller() auth.Principal { return user(domain.RoleReseller) }
func admin() auth.Principal    { return user(domain.RoleReseller, domain.RoleAdmin) }

func premium(p auth.Principal) auth.Principal {
	p.Premium = true
	return p
}

func internalCaller(name string) auth.Principal {
	return auth.Principal{Caller: name, RequestID: uuid.NewString()}
}

func fields() domain.ListingFields {
	return domain.ListingFields{
		Product:  commonv1.ProductType_PRODUCT_TYPE_KARTOSHKA,
		Quantity: domain.Mass{Grams: 20_000_000, Unit: domain.UnitTonne},
		Price:    &domain.Price{AmountMinor: 450_000, Currency: domain.CurrencyUZS, Unit: domain.UnitKilogram},
		Phone:    "+998901234567",
		Address:  domain.Address{Region: commonv1.Region_REGION_SAMARQAND, District: "Urgut", Line: "Mahalla 5"},
		Notes:    "Sifatli, qoplarda",
		MediaIDs: []uuid.UUID{uuid.New()},
	}
}

func (e env) create(t *testing.T, p auth.Principal, kind commonv1.ListingKind) domain.Listing {
	t.Helper()
	l, err := e.svc.CreateListing(ctx, p, uuid.NewString(), kind, fields())
	if err != nil {
		t.Fatalf("CreateListing: %v", err)
	}
	return l
}

// setStatus changes a stored listing's status directly (states catalog has
// no RPC for yet: CLOSED, SOLD_OUT, BLOCKED).
func (e env) setStatus(id uuid.UUID, st eventsv1.ListingStatus) {
	e.store.update(func(d *fakeDB) {
		l := d.listings[id]
		l.Status = st
		d.listings[id] = l
	})
}

func wantReason(t *testing.T, err error, reason commonv1.ErrorReason) *apperr.Error {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Reason != reason {
		t.Fatalf("err = %v, want %s", err, reason)
	}
	if ae.Code != apperr.CodeOf(reason) {
		t.Fatalf("code = %s, want %s", ae.Code, apperr.CodeOf(reason))
	}
	return ae
}

func TestCreateOffer(t *testing.T) {
	e := newEnv()
	p := farmer()
	p.Traceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	reqID := uuid.NewString()
	l, err := e.svc.CreateListing(ctx, p, reqID, domain.KindOffer, fields())
	if err != nil {
		t.Fatal(err)
	}
	if l.OwnerID != p.UserID || l.Kind != domain.KindOffer || l.Status != domain.StatusActive || l.Version != 1 ||
		l.RemainingGrams != l.Quantity.Grams || l.CreateRequestID.String() != reqID || l.CreatedAt.IsZero() ||
		!l.CreatedAt.Equal(l.UpdatedAt) || l.DeletedAt != nil || l.ViewsCount != 0 || l.ID.Version() != 7 {
		t.Fatalf("listing = %+v", l)
	}
	db := e.store.state()
	if len(db.lockedOwners) != 1 || db.lockedOwners[0] != p.UserID {
		t.Fatalf("owner locks = %v", db.lockedOwners)
	}
	doc := db.docs[l.ID]
	if doc.Product != "kartoshka картофель kartoshka kartoshka" || doc.Place != "urgut" || doc.Notes != "sifatli qoplarda" {
		t.Fatalf("search document = %+v", doc)
	}
	if len(db.outbox) != 1 {
		t.Fatalf("outbox = %d messages, want 1", len(db.outbox))
	}
	m := db.outbox[0]
	var ev eventsv1.ListingEvent
	if err := proto.Unmarshal(m.Value, &ev); err != nil {
		t.Fatal(err)
	}
	if m.Topic != events.TopicListing || m.Key != l.ID.String() || ev.GetListingCreated() == nil ||
		ev.GetListing().GetId() != l.ID.String() || ev.GetMetadata().GetCorrelationId() != p.RequestID ||
		ev.GetMetadata().GetActor().GetUserId() != p.UserID.String() ||
		ev.GetMetadata().GetTraceparent() != p.Traceparent || !ev.GetMetadata().GetOccurredAt().AsTime().Equal(l.CreatedAt) {
		t.Fatalf("event = %v (message %+v)", &ev, m)
	}
}

func TestCreateListingRoles(t *testing.T) {
	cases := map[string]struct {
		p      auth.Principal
		kind   commonv1.ListingKind
		reason commonv1.ErrorReason
	}{
		"anonymous":                  {auth.Principal{RequestID: "r"}, domain.KindBuyRequest, commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED},
		"offer by a reseller":        {reseller(), domain.KindOffer, commonv1.ErrorReason_ERROR_REASON_FARMER_VERIFICATION_REQUIRED},
		"offer by premium staff":     {premium(admin()), domain.KindOffer, commonv1.ErrorReason_ERROR_REASON_FARMER_VERIFICATION_REQUIRED},
		"buy-request without role":   {user(domain.RoleFarmer), domain.KindBuyRequest, commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED},
		"no roles at all":            {user(), domain.KindBuyRequest, commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED},
		"kind missing (direct call)": {farmer(), commonv1.ListingKind_LISTING_KIND_UNSPECIFIED, commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv()
			_, err := e.svc.CreateListing(ctx, tc.p, uuid.NewString(), tc.kind, fields())
			ae := wantReason(t, err, tc.reason)
			if tc.reason == commonv1.ErrorReason_ERROR_REASON_FARMER_VERIFICATION_REQUIRED &&
				(ae.Code != codes.FailedPrecondition || ae.Metadata["listing_kind"] != "OFFER") {
				t.Fatalf("error = %+v", ae)
			}
			if db := e.store.state(); len(db.listings) != 0 || len(db.outbox) != 0 || len(e.limiter.keys) != 0 {
				t.Fatalf("rejected call left state: %d listings, %d events, %d rate-limit hits",
					len(db.listings), len(db.outbox), len(e.limiter.keys))
			}
		})
	}
	// Both roles: both boards.
	e := newEnv()
	p := farmer()
	e.create(t, p, domain.KindOffer)
	e.create(t, p, domain.KindBuyRequest)
}

func TestCreateListingQuotas(t *testing.T) {
	cases := []struct {
		kind    commonv1.ListingKind
		premium bool
		limit   int
		plan    string
	}{
		{domain.KindOffer, false, 5, "default"},
		{domain.KindOffer, true, 20, "premium"},
		{domain.KindBuyRequest, false, 5, "default"},
		{domain.KindBuyRequest, true, 25, "premium"},
	}
	for _, tc := range cases {
		name := strings.TrimPrefix(tc.kind.String(), "LISTING_KIND_") + "/" + tc.plan
		t.Run(name, func(t *testing.T) {
			e := newEnv()
			p := farmer()
			p.Premium = tc.premium
			for range tc.limit {
				e.create(t, p, tc.kind)
			}
			_, err := e.svc.CreateListing(ctx, p, uuid.NewString(), tc.kind, fields())
			ae := wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_PLAN_LIMIT_REACHED)
			if ae.Code != codes.ResourceExhausted || ae.Metadata["plan"] != tc.plan ||
				ae.Metadata["listing_kind"] != strings.TrimPrefix(tc.kind.String(), "LISTING_KIND_") ||
				ae.Metadata["limit"] != strconv.Itoa(tc.limit) {
				t.Fatalf("error = %+v", ae)
			}
			if db := e.store.state(); len(db.listings) != tc.limit || len(db.outbox) != tc.limit {
				t.Fatalf("stored %d listings, %d events; want %d", len(db.listings), len(db.outbox), tc.limit)
			}
		})
	}
}

func TestQuotasArePerKindAndCountOnlyActive(t *testing.T) {
	e := newEnv()
	p := farmer() // both roles: both quotas apply, independently
	var offers []domain.Listing
	for range 5 {
		offers = append(offers, e.create(t, p, domain.KindOffer))
	}
	for range 5 {
		e.create(t, p, domain.KindBuyRequest)
	}
	for _, kind := range []commonv1.ListingKind{domain.KindOffer, domain.KindBuyRequest} {
		_, err := e.svc.CreateListing(ctx, p, uuid.NewString(), kind, fields())
		wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_PLAN_LIMIT_REACHED)
	}
	// Another owner is not affected.
	e.create(t, farmer(), domain.KindOffer)

	// Listings off the board free their slot: deleted, closed, sold out, blocked.
	if err := e.svc.DeleteListing(ctx, p, offers[0].ID.String(), 0); err != nil {
		t.Fatal(err)
	}
	e.setStatus(offers[1].ID, domain.StatusClosed)
	e.setStatus(offers[2].ID, domain.StatusSoldOut)
	e.setStatus(offers[3].ID, domain.StatusBlocked)
	for range 4 {
		e.create(t, p, domain.KindOffer)
	}
	_, err := e.svc.CreateListing(ctx, p, uuid.NewString(), domain.KindOffer, fields())
	wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_PLAN_LIMIT_REACHED)

	// Premium raises the limit for the same user.
	e.create(t, premium(p), domain.KindOffer)
}

func TestQuotasAreConfigurable(t *testing.T) {
	e := newEnv(service.WithQuotas(domain.Quotas{OfferDefault: 1, OfferPremium: 2, BuyRequestDefault: 1, BuyRequestPremium: 3}))
	p := farmer()
	e.create(t, p, domain.KindOffer)
	_, err := e.svc.CreateListing(ctx, p, uuid.NewString(), domain.KindOffer, fields())
	if ae := wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_PLAN_LIMIT_REACHED); ae.Metadata["limit"] != "1" {
		t.Fatalf("metadata = %v", ae.Metadata)
	}
	e.create(t, premium(p), domain.KindOffer)
}

func TestCreateListingIsIdempotent(t *testing.T) {
	e := newEnv()
	p := farmer()
	reqID := uuid.NewString()
	for range 4 {
		e.create(t, p, domain.KindOffer)
	}
	first, err := e.svc.CreateListing(ctx, p, reqID, domain.KindOffer, fields())
	if err != nil {
		t.Fatal(err)
	}
	// The quota is used up now, but a retry returns the original result.
	again, err := e.svc.CreateListing(ctx, p, reqID, domain.KindOffer, fields())
	if err != nil || again.ID != first.ID || again.Version != first.Version {
		t.Fatalf("retry = %+v, %v; want listing %s", again, err, first.ID)
	}
	if db := e.store.state(); len(db.listings) != 5 || len(db.outbox) != 5 {
		t.Fatalf("stored %d listings, %d events after a retry", len(db.listings), len(db.outbox))
	}
	// The same request id of another user is another listing.
	other, err := e.svc.CreateListing(ctx, farmer(), reqID, domain.KindOffer, fields())
	if err != nil || other.ID == first.ID {
		t.Fatalf("other user's create = %+v, %v", other, err)
	}
}

func TestCreateListingProducts(t *testing.T) {
	e := newEnv()
	p := farmer()
	f := fields()
	f.Product = commonv1.ProductType_PRODUCT_TYPE_BEDA // retired in the test reference data
	_, err := e.svc.CreateListing(ctx, p, uuid.NewString(), domain.KindOffer, f)
	if ae := wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED); ae.Violations[0].GetRule() != "product.not_retired" {
		t.Fatalf("violations = %v", ae.Violations)
	}
	f.Product = commonv1.ProductType_PRODUCT_TYPE_PISTA // defined but not in the test reference data
	_, err = e.svc.CreateListing(ctx, p, uuid.NewString(), domain.KindOffer, f)
	if ae := wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED); ae.Violations[0].GetRule() != "product.known" {
		t.Fatalf("violations = %v", ae.Violations)
	}
	if db := e.store.state(); len(db.listings) != 0 || len(db.outbox) != 0 {
		t.Fatal("a rejected create was stored")
	}
}

func TestCreateListingValidation(t *testing.T) {
	cases := map[string]struct {
		change func(f *domain.ListingFields)
		field  string
	}{
		"no product":          {func(f *domain.ListingFields) { f.Product = 0 }, "listing.product"},
		"zero quantity":       {func(f *domain.ListingFields) { f.Quantity.Grams = 0 }, "listing.quantity.grams"},
		"bad phone":           {func(f *domain.ListingFields) { f.Phone = "+99890123" }, "listing.phone"},
		"price in USD":        {func(f *domain.ListingFields) { f.Price.Currency = "USD" }, "listing.price"},
		"zero price":          {func(f *domain.ListingFields) { f.Price.AmountMinor = 0 }, "listing.price"},
		"price without unit":  {func(f *domain.ListingFields) { f.Price.Unit = 0 }, "listing.price"},
		"price overflows":     {func(f *domain.ListingFields) { f.Price.AmountMinor = 1 << 62 }, "listing.price"},
		"no region":           {func(f *domain.ListingFields) { f.Address.Region = 0 }, "listing.address.region"},
		"long notes":          {func(f *domain.ListingFields) { f.Notes = strings.Repeat("я", 1001) }, "listing.notes"},
		"duplicate media":     {func(f *domain.ListingFields) { f.MediaIDs = append(f.MediaIDs, f.MediaIDs[0]) }, "listing.media"},
		"too many media":      {func(f *domain.ListingFields) { f.MediaIDs = make([]uuid.UUID, 11) }, "listing.media"},
		"latitude off planet": {func(f *domain.ListingFields) { f.Address.Location = &domain.GeoPoint{Latitude: 91} }, "listing.address.location"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv()
			f := fields()
			tc.change(&f)
			_, err := e.svc.CreateListing(ctx, farmer(), uuid.NewString(), domain.KindOffer, f)
			ae := wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED)
			if len(ae.Violations) == 0 || ae.Violations[0].GetField() != tc.field {
				t.Fatalf("violations = %v, want field %s", ae.Violations, tc.field)
			}
		})
	}

	// Text is trimmed; 1000 characters after trimming are fine; negotiable
	// (no price) is fine.
	e := newEnv()
	f := fields()
	f.Notes = "  " + strings.Repeat("я", 1000) + "\n"
	f.Address.District = "  Urgut tumani "
	f.Price = nil
	l, err := e.svc.CreateListing(ctx, farmer(), uuid.NewString(), domain.KindOffer, f)
	if err != nil || len([]rune(l.Notes)) != 1000 || l.Address.District != "Urgut tumani" || l.Price != nil {
		t.Fatalf("listing = %+v, %v", l, err)
	}
	if _, err := e.svc.CreateListing(ctx, farmer(), "not-a-uuid", domain.KindOffer, fields()); err == nil {
		t.Fatal("bad request_id: want an error")
	}
}

func TestCreateListingRateLimit(t *testing.T) {
	e := newEnv()
	e.limiter.allowed, e.limiter.retryAfter = false, 90*time.Second
	p := farmer()
	_, err := e.svc.CreateListing(ctx, p, uuid.NewString(), domain.KindOffer, fields())
	ae := wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_RATE_LIMITED)
	if ae.Metadata["retry_after_seconds"] != "90" || e.limiter.keys[0] != "create-listing:"+p.UserID.String() {
		t.Fatalf("error = %+v, keys = %v", ae, e.limiter.keys)
	}
	// Redis down: fail open.
	e.limiter.allowed, e.limiter.err = false, errors.New("redis: connection refused")
	if _, err := e.svc.CreateListing(ctx, p, uuid.NewString(), domain.KindOffer, fields()); err != nil {
		t.Fatalf("limiter failure should fail open: %v", err)
	}
	// No limiter at all.
	svc := service.New(newFakeStore(), nil, nil)
	if _, err := svc.CreateListing(ctx, p, uuid.NewString(), domain.KindOffer, fields()); err != nil {
		t.Fatal(err)
	}
}

func TestCreateListingRollsBackOnFailure(t *testing.T) {
	e := newEnv()
	e.store.update(func(d *fakeDB) { d.failWith = errors.New("connection reset") })
	_, err := e.svc.CreateListing(ctx, farmer(), uuid.NewString(), domain.KindOffer, fields())
	wantReason(t, apperr.From(err), commonv1.ErrorReason_ERROR_REASON_INTERNAL)
}
