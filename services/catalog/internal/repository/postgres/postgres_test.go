package postgres

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v5"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"

	"github.com/tbilol/dxrl/services/catalog/internal/domain"
	"github.com/tbilol/dxrl/services/catalog/internal/events"
	"github.com/tbilol/dxrl/services/catalog/internal/service"
)

var (
	ctx         = context.Background()
	listingColN = []string{"id", "kind", "owner_id", "product_type", "quantity_grams", "quantity_unit",
		"price_minor", "price_currency", "price_unit", "phone", "region", "district",
		"address_line", "latitude", "longitude", "notes", "media_ids", "status", "views_count", "version",
		"create_request_id", "created_at", "updated_at", "deleted_at"}
	tashkent = time.FixedZone("UZT", 5*3600)
)

func newMock(t *testing.T) (pgxmock.PgxPoolIface, *Store) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		mock.Close()
	})
	return mock, New(mock)
}

func run(t *testing.T, s *Store, fn func(q service.Queries) error) {
	t.Helper()
	if err := s.Run(ctx, fn); err != nil {
		t.Fatal(err)
	}
}

func ptr[T any](v T) *T { return &v }

// listingRow is a stored ACTIVE offer: 20 t of potatoes at 4 500 so'm/kg
// with a map pin and two photos.
func listingRow(mock pgxmock.PgxPoolIface, id, owner, req uuid.UUID, media []uuid.UUID, created time.Time, extra ...any) *pgxmock.Rows {
	cols := listingColN
	if len(extra) > 0 {
		cols = append(append([]string{}, cols...), "rank")
	}
	vals := []any{id, "OFFER", owner, int32(9), int64(20_000_000), ptr("TONNE"),
		ptr(int64(450_000)), ptr("UZS"), ptr("KILOGRAM"), "+998901234567", "SAMARQAND", "Urgut",
		"Mahalla 5", ptr(39.65), ptr(66.96), "Sifatli", media, "ACTIVE", int64(17), int64(2),
		req, created, created.Add(time.Hour), (*time.Time)(nil)}
	return mock.NewRows(cols).AddRow(append(vals, extra...)...)
}

func TestGetListingMapsRow(t *testing.T) {
	mock, store := newMock(t)
	id, owner, req := uuid.New(), uuid.New(), uuid.New()
	media := []uuid.UUID{uuid.New(), uuid.New()}
	created := time.Date(2026, 9, 25, 15, 0, 0, 0, tashkent)
	mock.ExpectQuery(`FROM listings l WHERE l.id = \$1$`).WithArgs(id).
		WillReturnRows(listingRow(mock, id, owner, req, media, created))

	var l domain.Listing
	run(t, store, func(q service.Queries) (err error) { l, err = q.GetListing(ctx, id); return })
	want := domain.Listing{
		ID: id, Kind: domain.KindOffer, OwnerID: owner,
		ListingFields: domain.ListingFields{
			Product:  commonv1.ProductType_PRODUCT_TYPE_KARTOSHKA,
			Quantity: domain.Mass{Grams: 20_000_000, Unit: domain.UnitTonne},
			Price:    &domain.Price{AmountMinor: 450_000, Currency: "UZS", Unit: domain.UnitKilogram},
			Phone:    "+998901234567",
			Address: domain.Address{Region: commonv1.Region_REGION_SAMARQAND, District: "Urgut", Line: "Mahalla 5",
				Location: &domain.GeoPoint{Latitude: 39.65, Longitude: 66.96}},
			Notes:    "Sifatli",
			MediaIDs: media,
		},
		Status: domain.StatusActive, ViewsCount: 17, Version: 2, CreateRequestID: req,
		CreatedAt: created.UTC(), UpdatedAt: created.Add(time.Hour).UTC(),
	}
	if !reflect.DeepEqual(l, want) {
		t.Fatalf("listing =\n%+v\nwant\n%+v", l, want)
	}
	if l.CreatedAt.Location() != time.UTC {
		t.Fatalf("created_at not UTC: %s", l.CreatedAt)
	}
}

func TestGetListingNegotiableAndMissing(t *testing.T) {
	mock, store := newMock(t)
	id := uuid.New()
	now := time.Now()
	mock.ExpectQuery(`FROM listings l WHERE l.id = \$1`).WithArgs(id).WillReturnRows(mock.NewRows(listingColN).AddRow(
		id, "BUY_REQUEST", uuid.New(), int32(1), int64(1000), (*string)(nil),
		(*int64)(nil), (*string)(nil), (*string)(nil), "+998901234567", "XORAZM", "", "",
		(*float64)(nil), (*float64)(nil), "", []uuid.UUID{}, "DELETED", int64(0), int64(3),
		uuid.New(), now, now, &now))
	mock.ExpectQuery(`FROM listings l WHERE l.id = \$1`).WithArgs(id).WillReturnRows(mock.NewRows(listingColN))
	mock.ExpectQuery(`FROM listings l WHERE l.id = \$1`).WithArgs(id).WillReturnRows(mock.NewRows(listingColN).AddRow(
		id, "SWAP", uuid.New(), int32(1), int64(1000), (*string)(nil),
		(*int64)(nil), (*string)(nil), (*string)(nil), "+998901234567", "XORAZM", "", "",
		(*float64)(nil), (*float64)(nil), "", []uuid.UUID{}, "ACTIVE", int64(0), int64(1),
		uuid.New(), now, now, (*time.Time)(nil)))

	_ = store.Run(ctx, func(q service.Queries) error {
		l, err := q.GetListing(ctx, id)
		if err != nil || l.Kind != domain.KindBuyRequest || l.Price != nil || l.Quantity.Unit != 0 ||
			l.Address.Location != nil || l.MediaIDs != nil || l.Status != domain.StatusDeleted || l.DeletedAt == nil {
			t.Fatalf("negotiable deleted buy-request = %+v, %v", l, err)
		}
		if _, err := q.GetListing(ctx, id); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing: err = %v", err)
		}
		if _, err := q.GetListing(ctx, id); err == nil || !strings.Contains(err.Error(), "SWAP") {
			t.Fatalf("unknown kind: err = %v", err)
		}
		return nil
	})
}

func TestInsertListingArgs(t *testing.T) {
	mock, store := newMock(t)
	now := time.Now().UTC()
	l := domain.Listing{
		ID: uuid.New(), Kind: domain.KindBuyRequest, OwnerID: uuid.New(),
		ListingFields: domain.ListingFields{
			Product:  commonv1.ProductType_PRODUCT_TYPE_PIYOZ,
			Quantity: domain.Mass{Grams: 500_000}, // no display unit
			Phone:    "+998901234567",
			Address:  domain.Address{Region: commonv1.Region_REGION_NAMANGAN},
		},
		Status: domain.StatusActive, Version: 1, CreateRequestID: uuid.New(),
		CreatedAt: now, UpdatedAt: now,
	}
	doc := domain.SearchDocument{Product: "piyoz лук", Place: "", Notes: ""}
	mock.ExpectExec(`INSERT INTO listings .* setweight\(to_tsvector\('simple', \$25\), 'A'\) \|\| setweight\(to_tsvector\('simple', \$26\), 'B'\) \|\| setweight\(to_tsvector\('simple', \$27\), 'C'\)\)$`).
		WithArgs(l.ID, "BUY_REQUEST", l.OwnerID, int32(11), int64(500_000), (*string)(nil),
			(*int64)(nil), (*string)(nil), (*string)(nil), "+998901234567", "NAMANGAN", "", "", (*float64)(nil), (*float64)(nil),
			"", []uuid.UUID{}, "ACTIVE", int64(0), int64(1), l.CreateRequestID, now, now, (*time.Time)(nil),
			"piyoz лук", "", "").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	run(t, store, func(q service.Queries) error { return q.InsertListing(ctx, l, doc) })
}

func TestSaveListing(t *testing.T) {
	mock, store := newMock(t)
	now := time.Now().UTC()
	l := domain.Listing{
		ID: uuid.New(), Kind: domain.KindOffer, OwnerID: uuid.New(),
		ListingFields: domain.ListingFields{
			Product:  commonv1.ProductType_PRODUCT_TYPE_OLMA,
			Quantity: domain.Mass{Grams: 1_000_000, Unit: domain.UnitKilogram},
			Price:    &domain.Price{AmountMinor: 900_000_000, Currency: "UZS", Unit: domain.UnitTonne},
			Phone:    "+998901234567",
			Address:  domain.Address{Region: commonv1.Region_REGION_ANDIJON, Location: &domain.GeoPoint{Latitude: 1, Longitude: 2}},
			MediaIDs: []uuid.UUID{uuid.New()},
		},
		Status: domain.StatusDeleted, Version: 4, UpdatedAt: now, DeletedAt: &now,
	}
	base := []any{l.ID, int32(28), int64(1_000_000), ptr("KILOGRAM"), ptr(int64(900_000_000)), ptr("UZS"),
		ptr("TONNE"), "+998901234567", "ANDIJON", "", "", ptr(1.0), ptr(2.0), "", l.MediaIDs, "DELETED", int64(4), now, &now}
	mock.ExpectExec(`UPDATE listings SET .* search_document = search_document\s+WHERE id = \$1`).
		WithArgs(base...).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`search_document = setweight\(to_tsvector\('simple', \$20\), 'A'\)`).
		WithArgs(append(base, "olma яблоко olma olma", "", "")...).WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	_ = store.Run(ctx, func(q service.Queries) error {
		if err := q.SaveListing(ctx, l, nil); err != nil {
			t.Fatal(err)
		}
		doc := domain.SearchDocument{Product: "olma яблоко olma olma"}
		if err := q.SaveListing(ctx, l, &doc); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("no row updated: err = %v", err)
		}
		return nil
	})
}

func TestLockOwnerCountAndRequest(t *testing.T) {
	mock, store := newMock(t)
	owner, req := uuid.New(), uuid.New()
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock\(hashtextextended\(\$1, 0\)\)`).WithArgs("catalog:listings:" + owner.String()).
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectQuery(`WHERE l.owner_id = \$1 AND l.create_request_id = \$2`).WithArgs(owner, req).
		WillReturnRows(mock.NewRows(listingColN))
	mock.ExpectQuery(`SELECT count\(\*\) FROM listings\s+WHERE owner_id = \$1 AND kind = \$2 AND status = 'ACTIVE'`).
		WithArgs(owner, "OFFER").WillReturnRows(mock.NewRows([]string{"count"}).AddRow(4))
	mock.ExpectCommit()
	err := store.InTx(ctx, func(q service.Queries) error {
		if err := q.LockOwner(ctx, owner); err != nil {
			return err
		}
		if _, err := q.ListingByRequest(ctx, owner, req); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("by request: %v", err)
		}
		n, err := q.CountActiveListings(ctx, owner, domain.KindOffer)
		if err != nil || n != 4 {
			t.Fatalf("count = %d, %v", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestInTxRollsBackOnError(t *testing.T) {
	mock, store := newMock(t)
	boom := errors.New("boom")
	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectRollback()
	err := store.InTx(ctx, func(q service.Queries) error {
		if err := q.LockOwner(ctx, uuid.New()); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestFindListingsScansHits(t *testing.T) {
	mock, store := newMock(t)
	id, owner, req := uuid.New(), uuid.New(), uuid.New()
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`ts_rank\(l.search_document, to_tsquery\('simple', \$1\)\) AS rank`).
		WithArgs("'kart':*", 3).WillReturnRows(listingRow(mock, id, owner, req, []uuid.UUID{}, created, float32(0.25)))
	var hits []domain.Hit
	run(t, store, func(q service.Queries) (err error) {
		hits, err = q.FindListings(ctx, domain.ListingQuery{
			Criteria: domain.Criteria{Statuses: []eventsv1.ListingStatus{domain.StatusActive}, Terms: []string{"kart"}},
			Sort:     domain.SortRelevance, Desc: true, Limit: 3,
		})
		return err
	})
	if len(hits) != 1 || hits[0].ID != id || hits[0].Rank != 0.25 || hits[0].Product != commonv1.ProductType_PRODUCT_TYPE_KARTOSHKA {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestFindListingsSQL(t *testing.T) {
	active := []eventsv1.ListingStatus{domain.StatusActive}
	at := time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC)
	cursorID := uuid.New()
	owner := uuid.New()
	cases := map[string]struct {
		q        domain.ListingQuery
		contains []string
		args     []any
	}{
		"board browse, first page": {
			q: domain.ListingQuery{Criteria: domain.Criteria{Statuses: active, Kind: domain.KindOffer},
				Sort: domain.SortCreatedAt, Desc: true, Limit: 21},
			contains: []string{"0::real AS rank", "WHERE l.status = 'ACTIVE'\n  AND l.kind = $1",
				"ORDER BY l.created_at DESC, l.id DESC\nLIMIT $2"},
			args: []any{"OFFER", 21},
		},
		"owner, several statuses, next page": {
			q: domain.ListingQuery{Criteria: domain.Criteria{OwnerID: owner,
				Statuses: []eventsv1.ListingStatus{domain.StatusActive, domain.StatusClosed}},
				Sort: domain.SortCreatedAt, Desc: true, Limit: 6, After: &domain.Cursor{CreatedAt: at, ID: cursorID}},
			contains: []string{"l.status = ANY($1)", "l.owner_id = $2", "(l.created_at, l.id) < ($3, $4)"},
			args:     []any{[]string{"ACTIVE", "CLOSED"}, owner, at, cursorID, 6},
		},
		"every filter": {
			q: domain.ListingQuery{
				Criteria: domain.Criteria{
					Statuses:         active,
					Products:         []commonv1.ProductType{commonv1.ProductType_PRODUCT_TYPE_BUGDOY, commonv1.ProductType_PRODUCT_TYPE_ARPA},
					Regions:          []commonv1.Region{commonv1.Region_REGION_BUXORO},
					District:         "Urgut",
					MinQuantityGrams: 1_000_000,
					MinPricePerTonne: ptr(int64(0)),
					MaxPricePerTonne: ptr(int64(5_000_000)),
					Terms:            []string{"bugdoy", "o'k"},
				},
				Sort: domain.SortCreatedAt, Desc: false, Limit: 11,
			},
			contains: []string{"l.product_type = ANY($1)", "l.region = ANY($2)", "lower(l.district) = lower($3)",
				"l.quantity_grams >= $4", "l.price_per_tonne_minor >= $5", "l.price_per_tonne_minor <= $6",
				"l.search_document @@ to_tsquery('simple', $7)", "ts_rank(l.search_document, to_tsquery('simple', $7)) AS rank",
				"ORDER BY l.created_at ASC, l.id ASC"},
			args: []any{[]int32{1, 2}, []string{"BUXORO"}, "Urgut", int64(1_000_000), int64(0), int64(5_000_000),
				"'bugdoy':* & 'o''k':*", 11},
		},
		"price ascending after a negotiable listing": {
			q: domain.ListingQuery{Criteria: domain.Criteria{Statuses: active}, Sort: domain.SortPrice, Desc: false, Limit: 3,
				After: &domain.Cursor{Key: math.MaxInt64, CreatedAt: at, ID: cursorID}},
			contains: []string{
				"((COALESCE(l.price_per_tonne_minor, 9223372036854775807) > $1) OR " +
					"(COALESCE(l.price_per_tonne_minor, 9223372036854775807) = $1 AND l.created_at < $2) OR " +
					"(COALESCE(l.price_per_tonne_minor, 9223372036854775807) = $1 AND l.created_at = $2 AND l.id < $3))",
				"ORDER BY COALESCE(l.price_per_tonne_minor, 9223372036854775807) ASC, l.created_at DESC, l.id DESC"},
			args: []any{int64(math.MaxInt64), at, cursorID, 3},
		},
		"price descending": {
			q: domain.ListingQuery{Criteria: domain.Criteria{Statuses: active}, Sort: domain.SortPrice, Desc: true, Limit: 3,
				After: &domain.Cursor{Key: 450_000_000, CreatedAt: at, ID: cursorID}},
			contains: []string{"(COALESCE(l.price_per_tonne_minor, -1), l.created_at, l.id) < ($1, $2, $3)",
				"ORDER BY COALESCE(l.price_per_tonne_minor, -1) DESC, l.created_at DESC, l.id DESC"},
			args: []any{int64(450_000_000), at, cursorID, 3},
		},
		"quantity ascending": {
			q:        domain.ListingQuery{Criteria: domain.Criteria{Statuses: active}, Sort: domain.SortQuantity, Limit: 3},
			contains: []string{"ORDER BY l.quantity_grams ASC, l.created_at DESC, l.id DESC"},
			args:     []any{3},
		},
		"relevance, next page": {
			q: domain.ListingQuery{Criteria: domain.Criteria{Statuses: active, Terms: []string{"sabzi"}},
				Sort: domain.SortRelevance, Desc: true, Limit: 3,
				After: &domain.Cursor{Rank: 0.0607927, CreatedAt: at, ID: cursorID}},
			contains: []string{
				"(ts_rank(l.search_document, to_tsquery('simple', $1)), l.created_at, l.id) < ($2::real, $3, $4)",
				"ORDER BY ts_rank(l.search_document, to_tsquery('simple', $1)) DESC, l.created_at DESC, l.id DESC"},
			args: []any{"'sabzi':*", float32(0.0607927), at, cursorID, 3},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sql, args, err := findListingsSQL(tc.q)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.contains {
				if !strings.Contains(sql, want) {
					t.Fatalf("SQL does not contain %q:\n%s", want, sql)
				}
			}
			if !reflect.DeepEqual(args, tc.args) {
				t.Fatalf("args = %#v\nwant   %#v", args, tc.args)
			}
		})
	}
	if _, _, err := findListingsSQL(domain.ListingQuery{Sort: domain.SortCreatedAt, Limit: 1}); err == nil {
		t.Fatal("a query without statuses must fail")
	}
	if _, _, err := findListingsSQL(domain.ListingQuery{Criteria: domain.Criteria{Statuses: active}, Sort: "views", Limit: 1}); err == nil {
		t.Fatal("an unknown sort must fail")
	}
}

func TestPrefixQueryEscapes(t *testing.T) {
	if got := prefixQuery([]string{"kart", `a'b\c`}); got != `'kart':* & 'a''b\\c':*` {
		t.Fatalf("prefixQuery = %s", got)
	}
}

func TestProductTypes(t *testing.T) {
	mock, store := newMock(t)
	cols := []string{"id", "key", "name_uz", "name_ru", "category", "sort_order", "is_retired"}
	mock.ExpectQuery(`FROM product_types ORDER BY sort_order, id`).WillReturnRows(mock.NewRows(cols).
		AddRow(int32(1), "bugdoy", "Bug'doy", "Пшеница", "GRAIN_INDUSTRIAL", int32(10), false).
		AddRow(int32(42), "yongoq", "Yong'oq", "Грецкий орех", "NUT", int32(420), true))
	mock.ExpectQuery(`FROM product_types WHERE category = \$1`).WithArgs("MELON").WillReturnRows(mock.NewRows(cols))
	mock.ExpectQuery(`FROM product_types WHERE id = \$1`).WithArgs(int32(44)).WillReturnRows(mock.NewRows(cols))
	_ = store.Run(ctx, func(q service.Queries) error {
		all, err := q.ListProductTypes(ctx, 0)
		want := []domain.ProductType{
			{Type: commonv1.ProductType_PRODUCT_TYPE_BUGDOY, Key: "bugdoy", NameUZ: "Bug'doy", NameRU: "Пшеница",
				Category: commonv1.ProductCategory_PRODUCT_CATEGORY_GRAIN_INDUSTRIAL, SortOrder: 10},
			{Type: commonv1.ProductType_PRODUCT_TYPE_YONGOQ, Key: "yongoq", NameUZ: "Yong'oq", NameRU: "Грецкий орех",
				Category: commonv1.ProductCategory_PRODUCT_CATEGORY_NUT, SortOrder: 420, Retired: true},
		}
		if err != nil || !reflect.DeepEqual(all, want) {
			t.Fatalf("all = %+v, %v", all, err)
		}
		if melons, err := q.ListProductTypes(ctx, commonv1.ProductCategory_PRODUCT_CATEGORY_MELON); err != nil || len(melons) != 0 {
			t.Fatalf("melons = %+v, %v", melons, err)
		}
		if _, err := q.GetProductType(ctx, commonv1.ProductType_PRODUCT_TYPE_PISTA); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing product: %v", err)
		}
		return nil
	})
}

func TestAddViewsSortsIDs(t *testing.T) {
	mock, store := newMock(t)
	a := uuid.MustParse("00000000-0000-7000-8000-000000000001")
	b := uuid.MustParse("ffffffff-0000-7000-8000-000000000001")
	mock.ExpectExec(`UPDATE listings AS l SET views_count = l.views_count \+ v.n\s+FROM unnest\(\$1::uuid\[\], \$2::bigint\[\]\)`).
		WithArgs([]uuid.UUID{a, b}, []int64{3, 1}).WillReturnResult(pgxmock.NewResult("UPDATE", 2))
	if err := store.AddViews(ctx, map[uuid.UUID]int64{b: 1, a: 3}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddViews(ctx, nil); err != nil { // nothing to do: no statement
		t.Fatal(err)
	}
}

func TestEnqueueOutbox(t *testing.T) {
	mock, store := newMock(t)
	m := events.Message{ID: uuid.New(), Topic: events.TopicListing, Key: "k", Value: []byte{1},
		Headers: map[string]string{"event-type": "x"}, EventType: "x", CreatedAt: time.Now()}
	mock.ExpectExec(`INSERT INTO outbox`).WithArgs(m.ID, m.Topic, m.Key, m.Value, m.Headers, m.EventType, m.CreatedAt).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	run(t, store, func(q service.Queries) error { return q.EnqueueOutbox(ctx, m) })
}
