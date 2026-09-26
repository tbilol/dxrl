//go:build integration

// Integration tests against a real PostgreSQL. They run only with
// `-tags=integration` and CATALOG_TEST_DATABASE_URL set (make
// test-integration loads it from .env). Each run migrates a fresh temporary
// schema (selected through search_path) and drops it afterwards, so the
// target database is left as it was. No extension is needed.
package postgres_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	catalogv1 "github.com/tbilol/dxrl/gen/go/catalog/v1"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tbilol/dxrl/services/catalog/internal/apperr"
	"github.com/tbilol/dxrl/services/catalog/internal/auth"
	"github.com/tbilol/dxrl/services/catalog/internal/domain"
	"github.com/tbilol/dxrl/services/catalog/internal/events"
	"github.com/tbilol/dxrl/services/catalog/internal/repository/postgres"
	"github.com/tbilol/dxrl/services/catalog/internal/service"
	"github.com/tbilol/dxrl/services/catalog/migrations"
)

var ctx = context.Background()

// setup creates a temporary schema, migrates it and returns a pool whose
// search_path points at it.
func setup(t *testing.T) (*pgxpool.Pool, *goose.Provider) {
	t.Helper()
	url := os.Getenv("CATALOG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CATALOG_TEST_DATABASE_URL is not set")
	}
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	schema := "catalog_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+ident+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
		_ = admin.Close(context.Background())
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 20
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	provider, err := migrations.NewProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return pool, provider
}

func user(roles ...commonv1.Role) auth.Principal {
	return auth.Principal{UserID: uuid.Must(uuid.NewV7()), Roles: roles, RequestID: uuid.NewString()}
}

func farmer() auth.Principal { return user(domain.RoleReseller, domain.RoleFarmer) }

func fields(product commonv1.ProductType, district, notes string, price *domain.Price, tonnes int64) domain.ListingFields {
	return domain.ListingFields{
		Product:  product,
		Quantity: domain.Mass{Grams: tonnes * 1_000_000, Unit: domain.UnitTonne},
		Price:    price,
		Phone:    "+998901234567",
		Address: domain.Address{Region: commonv1.Region_REGION_SAMARQAND, District: district, Line: "Mahalla 5",
			Location: &domain.GeoPoint{Latitude: 39.65, Longitude: 66.96}},
		Notes:    notes,
		MediaIDs: []uuid.UUID{uuid.New(), uuid.New()},
	}
}

func perKg(soum int64) *domain.Price {
	return &domain.Price{AmountMinor: soum * 100, Currency: "UZS", Unit: domain.UnitKilogram}
}

func perTonne(soum int64) *domain.Price {
	return &domain.Price{AmountMinor: soum * 100, Currency: "UZS", Unit: domain.UnitTonne}
}

func wantReason(t *testing.T, err error, reason commonv1.ErrorReason) *apperr.Error {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Reason != reason {
		t.Fatalf("err = %v, want %s", err, reason)
	}
	return ae
}

func ids(ls []domain.Listing) []uuid.UUID {
	out := make([]uuid.UUID, len(ls))
	for i, l := range ls {
		out[i] = l.ID
	}
	return out
}

// TestIntegrationProductSeed: after `migrate up` the table holds exactly one
// row per defined ProductType, in sort order, with the product.proto key.
func TestIntegrationProductSeed(t *testing.T) {
	pool, _ := setup(t)
	svc := service.New(postgres.New(pool), nil, nil)
	all, err := svc.ListProductTypes(ctx, auth.Principal{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(commonv1.ProductType_name)-1 {
		t.Fatalf("%d product types, want %d", len(all), len(commonv1.ProductType_name)-1)
	}
	for i, pt := range all {
		name := commonv1.ProductType_name[int32(pt.Type)]
		if want := strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(name, "PRODUCT_TYPE_")), "_", "-"); pt.Key != want ||
			pt.NameUZ == "" || pt.NameRU == "" || pt.Category == 0 || pt.Retired {
			t.Errorf("row %d = %+v", i, pt)
		}
		if i > 0 && all[i-1].SortOrder >= pt.SortOrder {
			t.Errorf("not in sort order at %d", i)
		}
	}
	nuts, err := svc.ListProductTypes(ctx, auth.Principal{}, commonv1.ProductCategory_PRODUCT_CATEGORY_NUT)
	if err != nil || len(nuts) != 3 || nuts[0].Key != "yongoq" {
		t.Fatalf("nuts = %+v, %v", nuts, err)
	}
}

func TestIntegrationListings(t *testing.T) {
	pool, provider := setup(t)
	store := postgres.New(pool)
	svc := service.New(store, nil, nil)

	t.Run("create, read, outbox, idempotency", func(t *testing.T) {
		p := farmer()
		reqID := uuid.NewString()
		f := fields(commonv1.ProductType_PRODUCT_TYPE_KARTOSHKA, "Urgut", "Sifatli kartoshka", perKg(4500), 20)
		l, err := svc.CreateListing(ctx, p, reqID, domain.KindOffer, f)
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.GetListing(ctx, p, l.ID.String())
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != l.ID || got.OwnerID != p.UserID || got.Price == nil || *got.Price != *f.Price ||
			got.Quantity != f.Quantity || got.RemainingGrams != 20_000_000 || !slices.Equal(got.MediaIDs, f.MediaIDs) ||
			got.Address.Location == nil || *got.Address.Location != *f.Address.Location || got.Version != 1 ||
			!got.CreatedAt.Equal(l.CreatedAt) || got.Status != domain.StatusActive {
			t.Fatalf("read back %+v\nwant %+v", got, l)
		}
		var (
			payload   []byte
			key, typ  string
			perTonneV int64
		)
		if err := pool.QueryRow(ctx, `SELECT payload, record_key, event_type FROM outbox WHERE record_key = $1`,
			l.ID.String()).Scan(&payload, &key, &typ); err != nil {
			t.Fatal(err)
		}
		var ev eventsv1.ListingEvent
		if err := proto.Unmarshal(payload, &ev); err != nil || typ != events.ListingCreatedType || ev.GetListingCreated() == nil ||
			ev.GetListing().GetId() != l.ID.String() {
			t.Fatalf("outbox event = %v (%s), %v", &ev, typ, err)
		}
		if err := pool.QueryRow(ctx, `SELECT price_per_tonne_minor FROM listings WHERE id = $1`, l.ID).Scan(&perTonneV); err != nil ||
			perTonneV != 450_000*1000 {
			t.Fatalf("price_per_tonne_minor = %d, %v", perTonneV, err)
		}
		again, err := svc.CreateListing(ctx, p, reqID, domain.KindOffer, f)
		if err != nil || again.ID != l.ID {
			t.Fatalf("retry = %+v, %v", again, err)
		}
	})

	t.Run("quota holds under concurrent creations", func(t *testing.T) {
		p := farmer()
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			ok, lim int
		)
		for range 12 {
			wg.Go(func() {
				_, err := svc.CreateListing(ctx, p, uuid.NewString(), domain.KindOffer,
					fields(commonv1.ProductType_PRODUCT_TYPE_PIYOZ, "Quva", "", nil, 1))
				mu.Lock()
				defer mu.Unlock()
				var ae *apperr.Error
				switch {
				case err == nil:
					ok++
				case errors.As(err, &ae) && ae.Reason == commonv1.ErrorReason_ERROR_REASON_PLAN_LIMIT_REACHED:
					lim++
				default:
					t.Errorf("create: %v", err)
				}
			})
		}
		wg.Wait()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM listings WHERE owner_id = $1 AND status = 'ACTIVE'`, p.UserID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if ok != 5 || lim != 7 || n != 5 {
			t.Fatalf("created %d, limited %d, stored %d; want 5, 7, 5", ok, lim, n)
		}
		// Buy-requests have their own quota.
		if _, err := svc.CreateListing(ctx, p, uuid.NewString(), domain.KindBuyRequest,
			fields(commonv1.ProductType_PRODUCT_TYPE_PIYOZ, "Quva", "", nil, 1)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("update, version guard, delete, views", func(t *testing.T) {
		p := farmer()
		l, err := svc.CreateListing(ctx, p, uuid.NewString(), domain.KindOffer,
			fields(commonv1.ProductType_PRODUCT_TYPE_OLMA, "Namangan", "", perTonne(3_000_000), 5))
		if err != nil {
			t.Fatal(err)
		}
		up, err := svc.UpdateListing(ctx, p, l.ID.String(), domain.ListingFields{Notes: "Qizil olma, sovutgichda"},
			[]string{"notes", "price"}, 1)
		if err != nil || up.Version != 2 || up.Price != nil || up.Notes != "Qizil olma, sovutgichda" {
			t.Fatalf("update = %+v, %v", up, err)
		}
		_, err = svc.UpdateListing(ctx, p, l.ID.String(), domain.ListingFields{Notes: "x"}, []string{"notes"}, 1)
		if ae := wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_CONFLICT); ae.Metadata["current_version"] != "2" {
			t.Fatalf("metadata = %v", ae.Metadata)
		}
		// The new notes are searchable.
		page, err := svc.SearchListings(ctx, user(domain.RoleReseller), "sovutgich", domain.ListingFilter{}, 0, 0, domain.PageRequest{})
		if err != nil || !slices.Contains(ids(page.Listings), l.ID) {
			t.Fatalf("search after update = %v, %v", ids(page.Listings), err)
		}
		// Views do not bump the version.
		if err := store.AddViews(ctx, map[uuid.UUID]int64{l.ID: 3}); err != nil {
			t.Fatal(err)
		}
		if got, _ := svc.GetListing(ctx, p, l.ID.String()); got.ViewsCount != 3 || got.Version != 2 {
			t.Fatalf("after views = %+v", got)
		}
		if err := svc.DeleteListing(ctx, p, l.ID.String(), 2); err != nil {
			t.Fatal(err)
		}
		if err := svc.DeleteListing(ctx, p, l.ID.String(), 2); err != nil {
			t.Fatalf("repeated delete: %v", err)
		}
		got, err := svc.GetListing(ctx, p, l.ID.String())
		if err != nil || got.Status != domain.StatusDeleted || got.DeletedAt == nil || got.Version != 3 {
			t.Fatalf("deleted = %+v, %v", got, err)
		}
		_, err = svc.GetListing(ctx, user(domain.RoleReseller), l.ID.String())
		wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)
	})

	t.Run("migrations roll back and forward", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `TRUNCATE listing_transactions, listings, outbox`); err != nil {
			t.Fatal(err)
		}
		if _, err := provider.DownTo(ctx, 0); err != nil {
			t.Fatalf("migrate down: %v", err)
		}
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("migrate up again: %v", err)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM product_types`).Scan(&n); err != nil || n != len(commonv1.ProductType_name)-1 {
			t.Fatalf("product types after down/up = %d, %v", n, err)
		}
	})
}

func TestIntegrationSearch(t *testing.T) {
	pool, _ := setup(t)
	svc := service.New(postgres.New(pool), nil, nil, service.WithQuotas(domain.Quotas{
		OfferDefault: 50, OfferPremium: 50, BuyRequestDefault: 50, BuyRequestPremium: 50,
	}))
	seller, buyer := farmer(), user(domain.RoleReseller)
	create := func(p auth.Principal, kind commonv1.ListingKind, f domain.ListingFields) domain.Listing {
		t.Helper()
		l, err := svc.CreateListing(ctx, p, uuid.NewString(), kind, f)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // distinct created_at
		return l
	}
	potato := create(seller, domain.KindOffer, fields(commonv1.ProductType_PRODUCT_TYPE_KARTOSHKA, "Urgut tumani", "Qoplarda", perKg(4500), 20))
	wheat := create(seller, domain.KindOffer, fields(commonv1.ProductType_PRODUCT_TYPE_BUGDOY, "Jomboy", "Oliy nav", perTonne(3_900_000), 100))
	carrotNotes := create(seller, domain.KindOffer, fields(commonv1.ProductType_PRODUCT_TYPE_PIYOZ, "Urgut", "Sabzi ham bor", nil, 3))
	carrot := create(seller, domain.KindOffer, fields(commonv1.ProductType_PRODUCT_TYPE_SABZI, "Pastdargom", "", perKg(3000), 8))
	wanted := create(buyer, domain.KindBuyRequest, fields(commonv1.ProductType_PRODUCT_TYPE_KARTOSHKA, "Toshkent", "", perKg(4000), 50))
	closed := create(seller, domain.KindOffer, fields(commonv1.ProductType_PRODUCT_TYPE_KARTOSHKA, "Urgut", "", perKg(1000), 1))
	if _, err := pool.Exec(ctx, `UPDATE listings SET status = 'CLOSED' WHERE id = $1`, closed.ID); err != nil {
		t.Fatal(err)
	}

	search := func(query string, f domain.ListingFilter, sortBy catalogv1.ListingSortField, dir commonv1.SortDirection) []uuid.UUID {
		t.Helper()
		page, err := svc.SearchListings(ctx, buyer, query, f, sortBy, dir, domain.PageRequest{Size: 100})
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		return ids(page.Listings)
	}
	same := func(name string, got, want []uuid.UUID) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}

	t.Run("text", func(t *testing.T) {
		newestFirst := []uuid.UUID{wanted.ID, potato.ID}
		for _, q := range []string{"kartoshka", "kart", "КАРТОФ", "Kartoshka urgut"} {
			got := search(q, domain.ListingFilter{}, catalogv1.ListingSortField_LISTING_SORT_FIELD_CREATED_AT, 0)
			want := newestFirst
			if strings.Contains(q, "urgut") {
				want = []uuid.UUID{potato.ID} // every word must match (product + district)
			}
			same(q, got, want)
		}
		for _, q := range []string{"bug'doy", "Bugʻdoy", "bugdoy", "пшен"} {
			same(q, search(q, domain.ListingFilter{}, 0, 0), []uuid.UUID{wheat.ID})
		}
		same("no match", search("olcha", domain.ListingFilter{}, 0, 0), nil)
		// Relevance: the product name (weight A) beats a mention in the notes (C).
		same("relevance", search("sabzi", domain.ListingFilter{}, 0, 0), []uuid.UUID{carrot.ID, carrotNotes.ID})
	})

	t.Run("filters", func(t *testing.T) {
		same("kind", search("", domain.ListingFilter{Kind: domain.KindBuyRequest}, 0, 0), []uuid.UUID{wanted.ID})
		same("district, any case", search("", domain.ListingFilter{District: "URGUT"}, 0, 0), []uuid.UUID{carrotNotes.ID})
		same("products", search("", domain.ListingFilter{Products: []commonv1.ProductType{
			commonv1.ProductType_PRODUCT_TYPE_SABZI, commonv1.ProductType_PRODUCT_TYPE_BUGDOY}}, 0, 0), []uuid.UUID{carrot.ID, wheat.ID})
		same("min quantity", search("", domain.ListingFilter{MinQuantity: &domain.Mass{Grams: 50_000_000}}, 0, 0),
			[]uuid.UUID{wanted.ID, wheat.ID})
		// 3 900 000 so'm/t = 3 900 so'm/kg: in [3 500, 4 000] so'm/kg, like the 4 000 buy-request; negotiable excluded.
		same("price range across units", search("", domain.ListingFilter{MinPrice: perKg(3500), MaxPrice: perTonne(4_000_000)}, 0, 0),
			[]uuid.UUID{wanted.ID, wheat.ID})
		same("region", search("", domain.ListingFilter{Regions: []commonv1.Region{commonv1.Region_REGION_XORAZM}}, 0, 0), nil)
	})

	t.Run("sorts and pages", func(t *testing.T) {
		priceAsc := []uuid.UUID{carrot.ID, wheat.ID, wanted.ID, potato.ID, carrotNotes.ID} // negotiable last
		priceDesc := []uuid.UUID{potato.ID, wanted.ID, wheat.ID, carrot.ID, carrotNotes.ID}
		same("price asc", search("", domain.ListingFilter{}, catalogv1.ListingSortField_LISTING_SORT_FIELD_PRICE,
			commonv1.SortDirection_SORT_DIRECTION_ASC), priceAsc)
		same("price desc", search("", domain.ListingFilter{}, catalogv1.ListingSortField_LISTING_SORT_FIELD_PRICE, 0), priceDesc)
		same("quantity desc", search("", domain.ListingFilter{}, catalogv1.ListingSortField_LISTING_SORT_FIELD_QUANTITY, 0),
			[]uuid.UUID{wheat.ID, wanted.ID, potato.ID, carrot.ID, carrotNotes.ID})

		// Two at a time gives the same order, without gaps or repeats.
		for _, dir := range []commonv1.SortDirection{commonv1.SortDirection_SORT_DIRECTION_ASC, commonv1.SortDirection_SORT_DIRECTION_DESC} {
			var got []uuid.UUID
			token := ""
			for range 10 {
				page, err := svc.SearchListings(ctx, buyer, "", domain.ListingFilter{},
					catalogv1.ListingSortField_LISTING_SORT_FIELD_PRICE, dir, domain.PageRequest{Size: 2, Token: token})
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, ids(page.Listings)...)
				if token = page.NextPageToken; token == "" {
					break
				}
			}
			want := priceDesc
			if dir == commonv1.SortDirection_SORT_DIRECTION_ASC {
				want = priceAsc
			}
			same("paged price "+dir.String(), got, want)
		}
		var got []uuid.UUID
		token := ""
		for range 10 {
			page, err := svc.SearchListings(ctx, buyer, "kartoshka", domain.ListingFilter{}, 0, 0, domain.PageRequest{Size: 1, Token: token})
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, ids(page.Listings)...)
			if token = page.NextPageToken; token == "" {
				break
			}
		}
		if len(got) != 2 || !slices.Contains(got, potato.ID) || !slices.Contains(got, wanted.ID) {
			t.Errorf("paged relevance = %v", got)
		}
	})

	t.Run("statuses", func(t *testing.T) {
		if slices.Contains(search("", domain.ListingFilter{}, 0, 0), closed.ID) {
			t.Error("a CLOSED listing is on the board")
		}
		page, err := svc.ListListings(ctx, seller, domain.ListingFilter{OwnerID: seller.UserID.String(),
			Statuses: []eventsv1.ListingStatus{domain.StatusClosed}}, domain.PageRequest{})
		if err != nil || !slices.Equal(ids(page.Listings), []uuid.UUID{closed.ID}) {
			t.Fatalf("owner's closed listings = %v, %v", ids(page.Listings), err)
		}
		_, err = svc.ListListings(ctx, buyer, domain.ListingFilter{OwnerID: seller.UserID.String(),
			Statuses: []eventsv1.ListingStatus{domain.StatusClosed}}, domain.PageRequest{})
		wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED)
		page, err = svc.ListListings(ctx, buyer, domain.ListingFilter{OwnerID: seller.UserID.String()}, domain.PageRequest{})
		if err != nil || len(page.Listings) != 4 {
			t.Fatalf("seller's board listings = %v, %v", ids(page.Listings), err)
		}
	})
}

// deal is an escrow transaction order-escrow started on a listing: the
// owner's counterparty buys from an offer or sells to a buy-request.
type deal struct {
	id               uuid.UUID
	listing          domain.Listing
	farmer, reseller uuid.UUID
	grams            int64
}

func newDeal(l domain.Listing, counterparty uuid.UUID, grams int64) deal {
	d := deal{id: uuid.Must(uuid.NewV7()), listing: l, grams: grams}
	if l.Kind == domain.KindBuyRequest {
		d.farmer, d.reseller = counterparty, l.OwnerID
	} else {
		d.farmer, d.reseller = l.OwnerID, counterparty
	}
	return d
}

// event is one decoded record of the deal's life cycle: eventType at version
// in state, with a fresh event id.
func (d deal) event(eventType string, state eventsv1.TransactionState, version int64) domain.TransactionUpdate {
	return domain.TransactionUpdate{
		EventID:       uuid.Must(uuid.NewV7()),
		EventType:     "agrogram.events.v1." + eventType,
		Topic:         events.TopicTransaction,
		OccurredAt:    time.Now().UTC(),
		Version:       version,
		TransactionID: d.id,
		ListingID:     d.listing.ID,
		ListingKind:   d.listing.Kind,
		FarmerID:      d.farmer,
		ResellerID:    d.reseller,
		QuantityGrams: d.grams,
		State:         state,
		CreatedAt:     time.Now().UTC().Add(-time.Minute),
	}
}

func (d deal) created() domain.TransactionUpdate {
	return d.event("TransactionCreated", domain.StateAwaitingPayment, 1)
}

// TestIntegrationTransactionStream runs the transaction-stream consumer's
// service and repository on a real database: the inbox and the projection,
// commitments, SOLD_OUT, transaction-party visibility, the committed quantity
// in UpdateListing, concurrency, the inbox purge, outages and migration 00003.
func TestIntegrationTransactionStream(t *testing.T) {
	pool, provider := setup(t)
	store := postgres.New(pool)
	svc := service.New(store, nil, nil, service.WithQuotas(domain.Quotas{
		OfferDefault: 50, OfferPremium: 50, BuyRequestDefault: 50, BuyRequestPremium: 50,
	}))
	offer := func(t *testing.T, owner auth.Principal) domain.Listing { // 20 t
		t.Helper()
		l, err := svc.CreateListing(ctx, owner, uuid.NewString(), domain.KindOffer,
			fields(commonv1.ProductType_PRODUCT_TYPE_KARTOSHKA, "Urgut", "", perKg(4500), 20))
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	apply := func(t *testing.T, u domain.TransactionUpdate, want domain.ApplyOutcome) {
		t.Helper()
		if got, err := svc.ApplyTransactionEvent(ctx, u); err != nil || got != want {
			t.Fatalf("ApplyTransactionEvent(%s v%d) = %s, %v; want %s", u.EventType, u.Version, got, err, want)
		}
	}
	read := func(t *testing.T, id uuid.UUID) domain.Listing {
		t.Helper()
		l, err := svc.GetListing(ctx, user(domain.RoleReseller, domain.RoleAdmin), id.String())
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	wantListing := func(t *testing.T, l domain.Listing, remaining int64, status eventsv1.ListingStatus, version int64) {
		t.Helper()
		if l.RemainingGrams != remaining || l.Status != status || l.Version != version {
			t.Fatalf("listing = %d g remaining, %s, version %d; want %d g, %s, version %d",
				l.RemainingGrams, l.Status, l.Version, remaining, status, version)
		}
	}

	t.Run("inbox, projection, version guard, committed quantity, parties", func(t *testing.T) {
		owner, buyer := farmer(), user(domain.RoleReseller)
		l, other := offer(t, owner), offer(t, farmer())
		now := time.Now().UTC().Truncate(time.Microsecond)
		tx := domain.ListingTransaction{ID: uuid.New(), ListingID: l.ID, FarmerID: owner.UserID, ResellerID: buyer.UserID,
			QuantityGrams: 3_000_000, State: domain.StateAwaitingPayment, Version: 1, CreatedAt: now.Add(-time.Hour),
			RecordedAt: now, UpdatedAt: now}
		second := tx
		second.ID, second.ResellerID, second.QuantityGrams = uuid.New(), uuid.New(), 4_000_000
		inbox := domain.ProcessedEvent{EventID: uuid.New(), Topic: events.TopicTransaction,
			EventType: "agrogram.events.v1.TransactionCreated", AggregateID: tx.ID, AggregateVersion: 1, ProcessedAt: now}
		err := store.InTx(ctx, func(q service.Queries) error {
			for i, want := range []bool{true, false} {
				if fresh, err := q.MarkProcessed(ctx, inbox); err != nil || fresh != want {
					t.Fatalf("MarkProcessed %d = %v, %v; want %v", i+1, fresh, err, want)
				}
			}
			for _, row := range []domain.ListingTransaction{tx, second} {
				if err := q.InsertListingTransaction(ctx, row); err != nil {
					return err
				}
			}
			got, err := q.GetListingTransaction(ctx, tx.ID)
			if err != nil || got != tx {
				t.Fatalf("read back %+v, %v\nwant %+v", got, err, tx)
			}
			if _, err := q.GetListingTransaction(ctx, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("missing: %v", err)
			}
			next := tx
			next.State, next.Version, next.UpdatedAt = domain.StateCompleted, 6, now.Add(time.Minute)
			for i, want := range []bool{true, false} { // the second write repeats version 6
				if ok, err := q.UpdateListingTransaction(ctx, next); err != nil || ok != want {
					t.Fatalf("update %d = %v, %v; want %v", i+1, ok, err, want)
				}
			}
			older := next
			older.State, older.Version = domain.StatePaymentHeld, 2
			if ok, err := q.UpdateListingTransaction(ctx, older); err != nil || ok {
				t.Fatalf("older version written: %v, %v", ok, err)
			}
			if got, _ := q.GetListingTransaction(ctx, tx.ID); got.State != domain.StateCompleted || got.Version != 6 ||
				got.QuantityGrams != 3_000_000 || !got.RecordedAt.Equal(now) {
				t.Fatalf("after updates = %+v", got)
			}
			if grams, err := q.CommittedGrams(ctx, l.ID); err != nil || grams != 7_000_000 {
				t.Fatalf("committed = %d, %v; want 7 t", grams, err)
			}
			if grams, err := q.CommittedGrams(ctx, other.ID); err != nil || grams != 0 {
				t.Fatalf("committed on a listing without transactions = %d, %v", grams, err)
			}
			for who, want := range map[uuid.UUID]bool{owner.UserID: true, buyer.UserID: true, second.ResellerID: true,
				uuid.New(): false} {
				if party, err := q.IsTransactionParty(ctx, l.ID, who); err != nil || party != want {
					t.Fatalf("party %s of the listing = %v, %v; want %v", who, party, err, want)
				}
			}
			if party, err := q.IsTransactionParty(ctx, other.ID, buyer.UserID); err != nil || party {
				t.Fatalf("party of another listing = %v, %v", party, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		// The constraints hold on their own: a transaction on no listing, one
		// party on both sides, no quantity. None of it is an outage.
		for name, row := range map[string]domain.ListingTransaction{
			"unknown listing": func() domain.ListingTransaction { r := tx; r.ID, r.ListingID = uuid.New(), uuid.New(); return r }(),
			"same parties":    func() domain.ListingTransaction { r := tx; r.ID, r.ResellerID = uuid.New(), r.FarmerID; return r }(),
			"no quantity":     func() domain.ListingTransaction { r := tx; r.ID, r.QuantityGrams = uuid.New(), 0; return r }(),
		} {
			err := store.InTx(ctx, func(q service.Queries) error { return q.InsertListingTransaction(ctx, row) })
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code[:2] != "23" || postgres.IsTransient(err) {
				t.Errorf("%s: err = %v, want a constraint violation that is not transient", name, err)
			}
		}
	})

	t.Run("commit once, duplicate, stale, SOLD_OUT, party visibility", func(t *testing.T) {
		owner, a, b, outsider := farmer(), user(domain.RoleReseller), user(domain.RoleReseller), user(domain.RoleReseller)
		l := offer(t, owner)
		first := newDeal(l, a.UserID, 12_000_000)
		created := first.created()
		apply(t, created, domain.Applied)
		wantListing(t, read(t, l.ID), 8_000_000, domain.StatusActive, 2)
		apply(t, created, domain.Duplicate)
		apply(t, first.event("PaymentHeld", domain.StatePaymentHeld, 2), domain.Applied)
		apply(t, first.created(), domain.Stale) // TransactionCreated again with its own event id: older than v2
		wantListing(t, read(t, l.ID), 8_000_000, domain.StatusActive, 2)

		var (
			topic, eventType string
			aggregateID      uuid.UUID
			aggregateVersion int64
			state            string
			version          int64
		)
		if err := pool.QueryRow(ctx, `
SELECT topic, event_type, aggregate_id, aggregate_version FROM processed_events WHERE event_id = $1`, created.EventID).
			Scan(&topic, &eventType, &aggregateID, &aggregateVersion); err != nil || topic != events.TopicTransaction ||
			eventType != created.EventType || aggregateID != first.id || aggregateVersion != 1 {
			t.Fatalf("inbox row = %q, %q, %s, %d, %v", topic, eventType, aggregateID, aggregateVersion, err)
		}
		if err := pool.QueryRow(ctx, `SELECT state, version FROM listing_transactions WHERE transaction_id = $1`, first.id).
			Scan(&state, &version); err != nil || state != "PAYMENT_HELD" || version != 2 {
			t.Fatalf("projection = %s v%d, %v", state, version, err)
		}

		apply(t, newDeal(l, b.UserID, 8_000_000).created(), domain.Applied)
		wantListing(t, read(t, l.ID), 0, domain.StatusSoldOut, 3)
		page, err := svc.SearchListings(ctx, outsider, "kartoshka", domain.ListingFilter{}, 0, 0, domain.PageRequest{Size: 100})
		if err != nil || slices.Contains(ids(page.Listings), l.ID) {
			t.Fatalf("search = %v, %v; want the SOLD_OUT listing gone", ids(page.Listings), err)
		}
		for name, p := range map[string]auth.Principal{"anonymous": {RequestID: "r"}, "outsider": outsider} {
			t.Run(name, func(t *testing.T) {
				_, err := svc.GetListing(ctx, p, l.ID.String())
				wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)
			})
		}
		for name, p := range map[string]auth.Principal{"owner": owner, "buyer a": a, "buyer b": b} {
			if got, err := svc.GetListing(ctx, p, l.ID.String()); err != nil || got.Status != domain.StatusSoldOut {
				t.Fatalf("%s reads the SOLD_OUT listing: %+v, %v", name, got, err)
			}
		}
		_, err = svc.UpdateListing(ctx, owner, l.ID.String(), domain.ListingFields{Notes: "x"}, []string{"notes"}, 0)
		wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_LISTING_NOT_ACTIVE)
		_, err = svc.UpdateListing(ctx, a, l.ID.String(), domain.ListingFields{Notes: "x"}, []string{"notes"}, 0)
		wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED) // a party sees it, may not edit it

		// An inconsistent event writes nothing, not even its inbox row.
		ghost := newDeal(domain.Listing{ID: uuid.New(), Kind: domain.KindOffer, OwnerID: uuid.New()}, uuid.New(), 1_000_000)
		bad := ghost.created()
		if _, err := svc.ApplyTransactionEvent(ctx, bad); !errors.Is(err, domain.ErrInconsistentEvent) || postgres.IsTransient(err) {
			t.Fatalf("event on an unknown listing: err = %v", err)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM processed_events WHERE event_id = $1`, bad.EventID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("inbox rows of the inconsistent event = %d, %v", n, err)
		}
	})

	t.Run("UpdateListing keeps the committed quantity", func(t *testing.T) {
		owner := farmer()
		l := offer(t, owner)
		apply(t, newDeal(l, uuid.New(), 6_000_000).created(), domain.Applied)
		quantity := func(grams int64) domain.ListingFields {
			return domain.ListingFields{Quantity: domain.Mass{Grams: grams, Unit: domain.UnitTonne}}
		}
		_, err := svc.UpdateListing(ctx, owner, l.ID.String(), quantity(5_000_000), []string{"quantity"}, 2)
		if ae := wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_INSUFFICIENT_QUANTITY); ae.Metadata["committed_grams"] != "6000000" ||
			ae.Metadata["requested_grams"] != "5000000" {
			t.Fatalf("metadata = %v", ae.Metadata)
		}
		up, err := svc.UpdateListing(ctx, owner, l.ID.String(), quantity(10_000_000), []string{"quantity"}, 2)
		if err != nil {
			t.Fatal(err)
		}
		wantListing(t, up, 4_000_000, domain.StatusActive, 3)
		wantListing(t, read(t, l.ID), 4_000_000, domain.StatusActive, 3)
		up, err = svc.UpdateListing(ctx, owner, l.ID.String(), quantity(6_000_000), []string{"quantity"}, 3)
		if err != nil {
			t.Fatal(err)
		}
		wantListing(t, up, 0, domain.StatusSoldOut, 4)
		wantListing(t, read(t, l.ID), 0, domain.StatusSoldOut, 4)
	})

	t.Run("concurrent commitments and redeliveries", func(t *testing.T) {
		l := offer(t, farmer())
		updates := make([]domain.TransactionUpdate, 10)
		for i := range updates {
			updates[i] = newDeal(l, uuid.New(), 2_000_000).created()
		}
		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			outcomes = map[domain.ApplyOutcome]int{}
		)
		for range 2 { // every event delivered twice, all at once
			for _, u := range updates {
				wg.Go(func() {
					got, err := svc.ApplyTransactionEvent(ctx, u)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						t.Errorf("apply: %v", err)
					}
					outcomes[got]++
				})
			}
		}
		wg.Wait()
		if outcomes[domain.Applied] != 10 || outcomes[domain.Duplicate] != 10 {
			t.Fatalf("outcomes = %v, want 10 applied and 10 duplicates", outcomes)
		}
		// Ten commitments of 2 t, each seeing the one before: 20 t committed,
		// ten version bumps, SOLD_OUT with the last.
		wantListing(t, read(t, l.ID), 0, domain.StatusSoldOut, 11)
		var committed int64
		if err := pool.QueryRow(ctx, `SELECT sum(quantity_grams)::bigint FROM listing_transactions WHERE listing_id = $1`,
			l.ID).Scan(&committed); err != nil || committed != 20_000_000 {
			t.Fatalf("committed = %d, %v", committed, err)
		}
	})

	t.Run("inbox purge; a replay after it commits nothing", func(t *testing.T) {
		l := offer(t, farmer())
		d := newDeal(l, uuid.New(), 5_000_000)
		created := d.created()
		apply(t, created, domain.Applied)
		n, err := store.PurgeProcessedEvents(ctx, time.Now().Add(time.Hour))
		if err != nil || n < 1 {
			t.Fatalf("purged %d, %v", n, err)
		}
		var left int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM processed_events`).Scan(&left); err != nil || left != 0 {
			t.Fatalf("inbox rows after the purge = %d, %v", left, err)
		}
		// The event id is forgotten, but the transaction is not: the replay is
		// stale, and the quantity stays committed once.
		apply(t, created, domain.Stale)
		wantListing(t, read(t, l.ID), 15_000_000, domain.StatusActive, 2)
	})

	t.Run("an unreachable database is transient", func(t *testing.T) {
		cfg := pool.Config().Copy()
		cfg.ConnConfig.Port = 1 // nothing listens there
		cfg.ConnConfig.Fallbacks = nil
		cfg.ConnConfig.ConnectTimeout = time.Second
		dead, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer dead.Close()
		deadSvc := service.New(postgres.New(dead), nil, nil)
		ghost := newDeal(domain.Listing{ID: uuid.New(), Kind: domain.KindOffer, OwnerID: uuid.New()}, uuid.New(), 1_000_000)
		_, err = deadSvc.ApplyTransactionEvent(ctx, ghost.created())
		if err == nil || !postgres.IsTransient(err) || errors.Is(err, domain.ErrInconsistentEvent) {
			t.Fatalf("err = %v, want a transient error", err)
		}
		if _, err := postgres.New(dead).PurgeProcessedEvents(ctx, time.Now()); err == nil || !postgres.IsTransient(err) {
			t.Fatalf("purge: err = %v, want a transient error", err)
		}
	})

	t.Run("migration 00003 rolls back and forward", func(t *testing.T) {
		exists := func(table string) bool {
			t.Helper()
			var found bool
			if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&found); err != nil {
				t.Fatal(err)
			}
			return found
		}
		sold := offer(t, farmer())
		apply(t, newDeal(sold, uuid.New(), 20_000_000).created(), domain.Applied)
		res, err := provider.Down(ctx)
		if err != nil || res.Source.Version != 3 {
			t.Fatalf("down = %v, %v; want migration 3 rolled back", res, err)
		}
		if exists("listing_transactions") || exists("processed_events") || !exists("listings") {
			t.Fatal("down must drop exactly the projection and the inbox")
		}
		// Listings keep what the commitments made of them.
		wantListing(t, read(t, sold.ID), 0, domain.StatusSoldOut, 2)
		results, err := provider.Up(ctx)
		if err != nil || len(results) != 1 || results[0].Source.Version != 3 {
			t.Fatalf("up = %v, %v; want migration 3 applied again", results, err)
		}
		if !exists("listing_transactions") || !exists("processed_events") {
			t.Fatal("up must recreate the projection and the inbox")
		}
		if version, err := provider.GetDBVersion(ctx); err != nil || version != 3 {
			t.Fatalf("schema version = %d, %v", version, err)
		}
	})
}
