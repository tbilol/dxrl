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

// member is a signed-in user: every account may post on either board.
func member() auth.Principal { return user() }

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
		p := member()
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
			got.Quantity != f.Quantity || !slices.Equal(got.MediaIDs, f.MediaIDs) ||
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
		p := member()
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			ok, lim int
		)
		const over = 5 // attempts beyond the quota
		for range domain.DefaultListingQuota + over {
			wg.Go(func() {
				_, err := svc.CreateListing(ctx, p, uuid.NewString(), domain.KindOffer,
					fields(commonv1.ProductType_PRODUCT_TYPE_PIYOZ, "Quva", "", nil, 1))
				mu.Lock()
				defer mu.Unlock()
				var ae *apperr.Error
				switch {
				case err == nil:
					ok++
				case errors.As(err, &ae) && ae.Reason == commonv1.ErrorReason_ERROR_REASON_LISTING_LIMIT_REACHED:
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
		if q := domain.DefaultListingQuota; ok != q || lim != over || n != q {
			t.Fatalf("created %d, limited %d, stored %d; want %d, %d, %d", ok, lim, n, q, over, q)
		}
		// Buy-requests have their own quota.
		if _, err := svc.CreateListing(ctx, p, uuid.NewString(), domain.KindBuyRequest,
			fields(commonv1.ProductType_PRODUCT_TYPE_PIYOZ, "Quva", "", nil, 1)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("update, version guard, delete, views", func(t *testing.T) {
		p := member()
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
		page, err := svc.SearchListings(ctx, member(), "sovutgich", domain.ListingFilter{}, 0, 0, domain.PageRequest{})
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
		_, err = svc.GetListing(ctx, member(), l.ID.String())
		wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)
	})

	t.Run("migrations roll back and forward", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `TRUNCATE listings, outbox`); err != nil {
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
	svc := service.New(postgres.New(pool), nil, nil, service.WithListingQuota(50))
	seller, buyer := member(), member()
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

// TestIntegrationListingsOnlyMigration: migration 00004 drops the
// transaction projection and the remaining quantity, and turns SOLD_OUT
// listings into CLOSED ones; its down migration restores the 00003 schema.
func TestIntegrationListingsOnlyMigration(t *testing.T) {
	pool, provider := setup(t)
	exists := func(table string) bool {
		t.Helper()
		var found bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&found); err != nil {
			t.Fatal(err)
		}
		return found
	}
	hasColumn := func() bool {
		t.Helper()
		var found bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = 'listings' AND column_name = 'remaining_grams')`).Scan(&found); err != nil {
			t.Fatal(err)
		}
		return found
	}
	if exists("listing_transactions") || exists("processed_events") || hasColumn() {
		t.Fatal("00004 must drop listing_transactions, processed_events and listings.remaining_grams")
	}

	svc := service.New(postgres.New(pool), nil, nil)
	l, err := svc.CreateListing(ctx, member(), uuid.NewString(), domain.KindOffer,
		fields(commonv1.ProductType_PRODUCT_TYPE_BUGDOY, "Urgut", "", perKg(4000), 5))
	if err != nil {
		t.Fatal(err)
	}
	res, err := provider.Down(ctx)
	if err != nil || res.Source.Version != 4 {
		t.Fatalf("down = %v, %v; want migration 4 rolled back", res, err)
	}
	if !exists("listing_transactions") || !exists("processed_events") || !hasColumn() {
		t.Fatal("down must restore the 00003 schema")
	}
	var remaining int64
	if err := pool.QueryRow(ctx, `SELECT remaining_grams FROM listings WHERE id = $1`, l.ID).Scan(&remaining); err != nil ||
		remaining != l.Quantity.Grams {
		t.Fatalf("remaining after down = %d, %v; want the whole quantity", remaining, err)
	}
	// A listing that sold out under the old schema is CLOSED afterwards.
	if _, err := pool.Exec(ctx, `UPDATE listings SET status = 'SOLD_OUT', remaining_grams = 0 WHERE id = $1`, l.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
	var got domain.Listing
	err = postgres.New(pool).Run(ctx, func(q service.Queries) (err error) { got, err = q.GetListing(ctx, l.ID); return })
	if err != nil || got.Status != domain.StatusClosed || got.Version != l.Version {
		t.Fatalf("sold-out listing after 00004 = %+v, %v; want CLOSED", got, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE listings SET status = 'SOLD_OUT' WHERE id = $1`, l.ID); err == nil {
		t.Fatal("SOLD_OUT must violate listings_status_check")
	}
	if version, err := provider.GetDBVersion(ctx); err != nil || version != 4 {
		t.Fatalf("schema version = %d, %v", version, err)
	}
}
