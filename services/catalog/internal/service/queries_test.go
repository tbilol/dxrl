package service_test

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	catalogv1 "github.com/tbilol/dxrl/gen/go/catalog/v1"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"

	"github.com/tbilol/dxrl/services/catalog/internal/auth"
	"github.com/tbilol/dxrl/services/catalog/internal/domain"
	"github.com/tbilol/dxrl/services/catalog/internal/service"
)

func (e env) lastQuery(t *testing.T) domain.ListingQuery {
	t.Helper()
	qs := e.store.state().queries
	if len(qs) == 0 {
		t.Fatal("no listing query ran")
	}
	return qs[len(qs)-1]
}

func TestListListingsStatusesAndVisibility(t *testing.T) {
	e := newEnv()
	owner := farmer()
	active := e.create(t, owner, domain.KindOffer)
	closed := e.create(t, owner, domain.KindOffer)
	e.setStatus(closed.ID, domain.StatusClosed)
	nonActive := []eventsv1.ListingStatus{domain.StatusClosed, domain.StatusActive}

	// Default: ACTIVE only, for everyone.
	page, err := e.svc.ListListings(ctx, auth.Principal{}, domain.ListingFilter{OwnerID: owner.UserID.String()}, domain.PageRequest{})
	if err != nil || len(page.Listings) != 1 || page.Listings[0].ID != active.ID || page.NextPageToken != "" {
		t.Fatalf("anonymous owner listing = %+v, %v", page, err)
	}
	if q := e.lastQuery(t); !slices.Equal(q.Criteria.Statuses, []eventsv1.ListingStatus{domain.StatusActive}) ||
		q.Criteria.OwnerID != owner.UserID || q.Sort != domain.SortCreatedAt || !q.Desc || q.Limit != 21 {
		t.Fatalf("query = %+v", q)
	}

	// Other statuses: the owner (profile-social forwards the owner's headers) and staff only.
	for name, p := range map[string]auth.Principal{"owner": owner, "staff": admin()} {
		page, err := e.svc.ListListings(ctx, p, domain.ListingFilter{OwnerID: owner.UserID.String(), Statuses: nonActive},
			domain.PageRequest{})
		if err != nil || len(page.Listings) != 2 {
			t.Fatalf("%s: page = %+v, %v", name, page, err)
		}
	}
	for name, tc := range map[string]struct {
		p auth.Principal
		f domain.ListingFilter
	}{
		"other user":             {reseller(), domain.ListingFilter{OwnerID: owner.UserID.String(), Statuses: nonActive}},
		"anonymous":              {auth.Principal{}, domain.ListingFilter{OwnerID: owner.UserID.String(), Statuses: nonActive}},
		"owner without owner_id": {owner, domain.ListingFilter{Statuses: nonActive}},
		"order-escrow":           {internalCaller("order-escrow"), domain.ListingFilter{Statuses: nonActive}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.svc.ListListings(ctx, tc.p, tc.f, domain.PageRequest{})
			if ae := wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED); ae.Metadata["status"] != "CLOSED" {
				t.Fatalf("metadata = %v", ae.Metadata)
			}
		})
	}
	_, err = e.svc.ListListings(ctx, owner, domain.ListingFilter{OwnerID: "x"}, domain.PageRequest{})
	wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED)
}

func TestListListingsPages(t *testing.T) {
	e := newEnv()
	var created []domain.Listing
	for range 5 {
		created = append(created, e.create(t, farmer(), domain.KindOffer))
	}
	e.create(t, farmer(), domain.KindBuyRequest) // another board
	f := domain.ListingFilter{Kind: domain.KindOffer}

	var got []uuid.UUID
	token := ""
	for i := 0; ; i++ {
		page, err := e.svc.ListListings(ctx, reseller(), f, domain.PageRequest{Size: 2, Token: token})
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		for _, l := range page.Listings {
			got = append(got, l.ID)
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
		if i > 5 {
			t.Fatal("paging does not end")
		}
	}
	want := []uuid.UUID{created[4].ID, created[3].ID, created[2].ID, created[1].ID, created[0].ID}
	if !slices.Equal(got, want) {
		t.Fatalf("pages = %v, want newest first %v", got, want)
	}

	// A token is valid only for the query that produced it.
	first, err := e.svc.ListListings(ctx, reseller(), f, domain.PageRequest{Size: 2})
	if err != nil || first.NextPageToken == "" {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	for name, call := range map[string]func() error{
		"other filter": func() error {
			_, err := e.svc.ListListings(ctx, reseller(), domain.ListingFilter{Kind: domain.KindBuyRequest},
				domain.PageRequest{Token: first.NextPageToken})
			return err
		},
		"search with a list token": func() error {
			_, err := e.svc.SearchListings(ctx, reseller(), "", f, 0, 0, domain.PageRequest{Token: first.NextPageToken})
			return err
		},
		"garbage": func() error {
			_, err := e.svc.ListListings(ctx, reseller(), f, domain.PageRequest{Token: "not-a-token"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			ae := wantReason(t, call(), commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED)
			if ae.Violations[0].GetField() != "page.page_token" {
				t.Fatalf("violations = %v", ae.Violations)
			}
		})
	}
	// The page size may change between pages; 0 means 20 and 100 is the cap.
	if _, err := e.svc.ListListings(ctx, reseller(), f, domain.PageRequest{Size: 500, Token: first.NextPageToken}); err != nil {
		t.Fatal(err)
	}
	if q := e.lastQuery(t); q.Limit != 101 || q.After == nil || q.After.ID != created[3].ID {
		t.Fatalf("query = %+v", q)
	}
}

func TestSearchListingsCriteria(t *testing.T) {
	e := newEnv()
	owner := farmer()
	f := domain.ListingFilter{
		Kind:        domain.KindOffer,
		Products:    []commonv1.ProductType{commonv1.ProductType_PRODUCT_TYPE_SABZI, commonv1.ProductType_PRODUCT_TYPE_BUGDOY},
		Regions:     []commonv1.Region{commonv1.Region_REGION_XORAZM, commonv1.Region_REGION_ANDIJON},
		District:    "  Urgut ",
		OwnerID:     owner.UserID.String(),
		MinQuantity: &domain.Mass{Grams: 1_000_000, Unit: domain.UnitTonne},
		// 3 000 so'm per kg and 5 000 000 so'm per tonne (= 5 000 so'm per kg).
		MinPrice: &domain.Price{AmountMinor: 300_000, Currency: "UZS", Unit: domain.UnitKilogram},
		MaxPrice: &domain.Price{AmountMinor: 500_000_000, Currency: "UZS", Unit: domain.UnitTonne},
	}
	if _, err := e.svc.SearchListings(ctx, owner, "  Bug‘doy, URGUT bug'doy!", f, 0, 0, domain.PageRequest{Size: 5}); err != nil {
		t.Fatal(err)
	}
	q := e.lastQuery(t)
	c := q.Criteria
	if c.Kind != domain.KindOffer || c.OwnerID != owner.UserID || c.District != "Urgut" || c.MinRemainingGrams != 1_000_000 ||
		!slices.Equal(c.Products, []commonv1.ProductType{commonv1.ProductType_PRODUCT_TYPE_BUGDOY, commonv1.ProductType_PRODUCT_TYPE_SABZI}) ||
		!slices.Equal(c.Regions, []commonv1.Region{commonv1.Region_REGION_ANDIJON, commonv1.Region_REGION_XORAZM}) ||
		*c.MinPricePerTonne != 300_000_000 || *c.MaxPricePerTonne != 500_000_000 ||
		!slices.Equal(c.Terms, []string{"bugdoy", "urgut"}) || q.Sort != domain.SortRelevance || !q.Desc || q.Limit != 6 {
		t.Fatalf("query = %+v, criteria = %+v", q, c)
	}

	for name, tc := range map[string]struct {
		f     domain.ListingFilter
		field string
	}{
		"min above max across units": {domain.ListingFilter{
			MinPrice: &domain.Price{AmountMinor: 600_000, Currency: "UZS", Unit: domain.UnitKilogram},
			MaxPrice: &domain.Price{AmountMinor: 500_000_000, Currency: "UZS", Unit: domain.UnitTonne},
		}, "filter"},
		"price in USD": {domain.ListingFilter{
			MaxPrice: &domain.Price{AmountMinor: 100, Currency: "USD", Unit: domain.UnitKilogram},
		}, "filter.max_price.amount.currency_code"},
		"price without unit": {domain.ListingFilter{
			MinPrice: &domain.Price{AmountMinor: 100, Currency: "UZS"},
		}, "filter.min_price"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.svc.SearchListings(ctx, owner, "", tc.f, 0, 0, domain.PageRequest{})
			ae := wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED)
			if ae.Violations[0].GetField() != tc.field {
				t.Fatalf("violations = %v, want %s", ae.Violations, tc.field)
			}
		})
	}
	// min_price 0 is a valid filter: it only excludes negotiable listings.
	if _, err := e.svc.SearchListings(ctx, owner, "", domain.ListingFilter{
		MinPrice: &domain.Price{Currency: "UZS", Unit: domain.UnitKilogram},
	}, 0, 0, domain.PageRequest{}); err != nil || *e.lastQuery(t).Criteria.MinPricePerTonne != 0 {
		t.Fatalf("min_price 0: %v", err)
	}
}

func TestSearchListingsSortResolution(t *testing.T) {
	e := newEnv()
	const (
		unspecified = catalogv1.ListingSortField_LISTING_SORT_FIELD_UNSPECIFIED
		relevance   = catalogv1.ListingSortField_LISTING_SORT_FIELD_RELEVANCE
		createdAt   = catalogv1.ListingSortField_LISTING_SORT_FIELD_CREATED_AT
		price       = catalogv1.ListingSortField_LISTING_SORT_FIELD_PRICE
		quantity    = catalogv1.ListingSortField_LISTING_SORT_FIELD_QUANTITY
		asc         = commonv1.SortDirection_SORT_DIRECTION_ASC
		desc        = commonv1.SortDirection_SORT_DIRECTION_DESC
	)
	cases := []struct {
		text  string
		field catalogv1.ListingSortField
		dir   commonv1.SortDirection
		sort  domain.SortField
		desc  bool
	}{
		{"kartoshka", unspecified, 0, domain.SortRelevance, true},
		{"", unspecified, 0, domain.SortCreatedAt, true},
		{"", unspecified, asc, domain.SortCreatedAt, false},
		{"", relevance, 0, domain.SortCreatedAt, true}, // no text: no relevance
		{"kartoshka", relevance, desc, domain.SortRelevance, true},
		{"kartoshka", createdAt, asc, domain.SortCreatedAt, false},
		{"", price, asc, domain.SortPrice, false},
		{"", price, 0, domain.SortPrice, true},
		{"piyoz", quantity, desc, domain.SortQuantity, true},
		{"!!! ...", unspecified, 0, domain.SortCreatedAt, true}, // no words: filters only
	}
	for _, tc := range cases {
		if _, err := e.svc.SearchListings(ctx, reseller(), tc.text, domain.ListingFilter{}, tc.field, tc.dir, domain.PageRequest{}); err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
		if q := e.lastQuery(t); q.Sort != tc.sort || q.Desc != tc.desc {
			t.Fatalf("%+v: sort = %s desc=%t", tc, q.Sort, q.Desc)
		}
	}
	_, err := e.svc.SearchListings(ctx, reseller(), "kartoshka", domain.ListingFilter{}, relevance, asc, domain.PageRequest{})
	if ae := wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED); ae.Violations[0].GetField() != "sort_direction" {
		t.Fatalf("violations = %v", ae.Violations)
	}
}

// TestSearchPageTokens: the next-page token carries the last hit's sort key
// (price per tonne, with the negotiable key for listings without a price,
// remaining grams, or text rank) and is bound to the query.
func TestSearchPageTokens(t *testing.T) {
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	hit := func(price *domain.Price, grams int64, rank float32, age time.Duration) domain.Hit {
		f := fields()
		f.Price = price
		return domain.Hit{Listing: domain.Listing{ID: uuid.Must(uuid.NewV7()), Kind: domain.KindOffer, ListingFields: f,
			RemainingGrams: grams, Status: domain.StatusActive, CreatedAt: at.Add(-age)}, Rank: rank}
	}
	perKg := &domain.Price{AmountMinor: 450_000, Currency: "UZS", Unit: domain.UnitKilogram}
	cases := map[string]struct {
		text    string
		field   catalogv1.ListingSortField
		dir     commonv1.SortDirection
		hits    []domain.Hit
		wantKey int64
		rank    float32
	}{
		"price asc, negotiable last": {"", catalogv1.ListingSortField_LISTING_SORT_FIELD_PRICE, commonv1.SortDirection_SORT_DIRECTION_ASC,
			[]domain.Hit{hit(perKg, 1, 0, 0), hit(nil, 1, 0, time.Minute), hit(nil, 1, 0, time.Hour)},
			domain.NegotiableSortKey(false), 0},
		"price desc": {"", catalogv1.ListingSortField_LISTING_SORT_FIELD_PRICE, 0,
			[]domain.Hit{hit(perKg, 1, 0, 0), hit(perKg, 1, 0, time.Minute), hit(nil, 1, 0, 0)}, 450_000_000, 0},
		"price desc, negotiable": {"", catalogv1.ListingSortField_LISTING_SORT_FIELD_PRICE, 0,
			[]domain.Hit{hit(nil, 1, 0, 0), hit(nil, 1, 0, time.Minute), hit(nil, 1, 0, time.Hour)}, -1, 0},
		"quantity": {"", catalogv1.ListingSortField_LISTING_SORT_FIELD_QUANTITY, 0,
			[]domain.Hit{hit(nil, 9, 0, 0), hit(nil, 7, 0, 0), hit(nil, 5, 0, 0)}, 7, 0},
		"relevance": {"sabzi", 0, 0,
			[]domain.Hit{hit(nil, 1, 0.75, 0), hit(nil, 1, 0.0607927, 0), hit(nil, 1, 0.01, 0)}, 0, 0.0607927},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv()
			e.store.update(func(d *fakeDB) { d.hits = tc.hits })
			page, err := e.svc.SearchListings(ctx, reseller(), tc.text, domain.ListingFilter{}, tc.field, tc.dir, domain.PageRequest{Size: 2})
			if err != nil || len(page.Listings) != 2 || page.NextPageToken == "" {
				t.Fatalf("page = %+v, %v", page, err)
			}
			if _, err := e.svc.SearchListings(ctx, reseller(), tc.text, domain.ListingFilter{}, tc.field, tc.dir,
				domain.PageRequest{Size: 2, Token: page.NextPageToken}); err != nil {
				t.Fatal(err)
			}
			after := e.lastQuery(t).After
			last := tc.hits[1]
			if after == nil || after.Key != tc.wantKey || after.Rank != tc.rank || after.ID != last.ID || !after.CreatedAt.Equal(last.CreatedAt) {
				t.Fatalf("cursor = %+v, want key %d rank %v of %s", after, tc.wantKey, tc.rank, last.ID)
			}
			// Another text, direction or filter: the token does not fit.
			_, err = e.svc.SearchListings(ctx, reseller(), tc.text+" x", domain.ListingFilter{}, tc.field, tc.dir,
				domain.PageRequest{Token: page.NextPageToken})
			wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED)
			_, err = e.svc.SearchListings(ctx, reseller(), tc.text, domain.ListingFilter{Kind: domain.KindOffer}, tc.field, tc.dir,
				domain.PageRequest{Token: page.NextPageToken})
			wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED)
		})
	}
}

// TestSearchHidesNonActiveFromOthers: defence in depth if the repository
// ever returned a listing outside the criteria.
func TestSearchHidesNonActiveFromOthers(t *testing.T) {
	e := newEnv()
	hidden := domain.Hit{Listing: domain.Listing{ID: uuid.New(), OwnerID: uuid.New(), Status: domain.StatusBlocked}}
	visible := domain.Hit{Listing: domain.Listing{ID: uuid.New(), OwnerID: uuid.New(), Status: domain.StatusActive}}
	e.store.update(func(d *fakeDB) { d.hits = []domain.Hit{hidden, visible} })
	page, err := e.svc.SearchListings(ctx, reseller(), "", domain.ListingFilter{}, 0, 0, domain.PageRequest{})
	if err != nil || len(page.Listings) != 1 || page.Listings[0].ID != visible.ID {
		t.Fatalf("page = %+v, %v", page, err)
	}
}

func TestTerms(t *testing.T) {
	cases := map[string][]string{
		"Bug'doy":               {"bugdoy"},
		"Bugʻdoy":               {"bugdoy"}, // U+02BB, the official oʻ / gʻ letter
		"O‘rik va Olxoʼri":      {"orik", "va", "olxori"},
		"Свёкла, КАРТОФЕЛЬ!":    {"свекла", "картофель"},
		"achchiq-qalampir 5.5t": {"achchiq", "qalampir", "5", "5t"},
		"  ":                    nil,
		"'&|!():*\\":            nil,
	}
	for in, want := range cases {
		if got := service.Terms(in); !slices.Equal(got, want) {
			t.Errorf("Terms(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearchQueryTermsAreDistinctAndBounded(t *testing.T) {
	e := newEnv()
	if _, err := e.svc.SearchListings(ctx, reseller(), "a b a c d e f g h i j k l", domain.ListingFilter{}, 0, 0,
		domain.PageRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := e.lastQuery(t).Criteria.Terms; !slices.Equal(got, []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}) {
		t.Fatalf("terms = %q", got)
	}
}
