package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/catalog/internal/domain"
)

// listingCols are the listing columns every read selects, in scanListing's
// order.
const listingCols = `l.id, l.kind, l.owner_id, l.product_type, l.quantity_grams, l.quantity_unit,
	l.remaining_grams, l.price_minor, l.price_currency, l.price_unit, l.phone, l.region, l.district,
	l.address_line, l.latitude, l.longitude, l.notes, l.media_ids, l.status, l.views_count, l.version,
	l.create_request_id, l.created_at, l.updated_at, l.deleted_at`

// searchDocumentSQL builds listings.search_document from the three
// normalized parts, weighted A (product), B (district) and C (notes). The
// 'simple' configuration only splits words and lower-cases them; the service
// has normalized the text already, so the result does not depend on the
// database locale.
func searchDocumentSQL(product, place, notes string) string {
	return "setweight(to_tsvector('simple', " + product + "), 'A') || " +
		"setweight(to_tsvector('simple', " + place + "), 'B') || " +
		"setweight(to_tsvector('simple', " + notes + "), 'C')"
}

// scanListing scans listingCols (and extra destinations after them).
func scanListing(row pgx.Row, extra ...any) (domain.Listing, error) {
	var (
		l                             domain.Listing
		kind, status, region          string
		product                       int32
		quantityUnit, currency, pUnit *string
		priceMinor                    *int64
		lat, lon                      *float64
	)
	dest := append([]any{&l.ID, &kind, &l.OwnerID, &product, &l.Quantity.Grams, &quantityUnit,
		&l.RemainingGrams, &priceMinor, &currency, &pUnit, &l.Phone, &region, &l.Address.District,
		&l.Address.Line, &lat, &lon, &l.Notes, &l.MediaIDs, &status, &l.ViewsCount, &l.Version,
		&l.CreateRequestID, &l.CreatedAt, &l.UpdatedAt, &l.DeletedAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return l, domain.ErrNotFound
		}
		return l, err
	}
	var err error
	if l.Kind, err = kinds.value(kind); err != nil {
		return l, err
	}
	if l.Status, err = listingStatuses.value(status); err != nil {
		return l, err
	}
	if l.Address.Region, err = regions.value(region); err != nil {
		return l, err
	}
	l.Product = commonv1.ProductType(product)
	if quantityUnit != nil {
		if l.Quantity.Unit, err = massUnits.value(*quantityUnit); err != nil {
			return l, err
		}
	}
	if priceMinor != nil && currency != nil && pUnit != nil {
		unit, err := massUnits.value(*pUnit)
		if err != nil {
			return l, err
		}
		l.Price = &domain.Price{AmountMinor: *priceMinor, Currency: *currency, Unit: unit}
	}
	if lat != nil && lon != nil {
		l.Address.Location = &domain.GeoPoint{Latitude: *lat, Longitude: *lon}
	}
	if len(l.MediaIDs) == 0 {
		l.MediaIDs = nil
	}
	l.CreatedAt, l.UpdatedAt, l.DeletedAt = utc(l.CreatedAt), utc(l.UpdatedAt), utcPtr(l.DeletedAt)
	return l, nil
}

// listingValues are the column values of l's owner fields, in the order
// product_type .. media_ids of listingCols.
func listingValues(l domain.Listing) ([]any, error) {
	var (
		quantityUnit, currency, pUnit *string
		priceMinor                    *int64
		lat, lon                      *float64
	)
	if l.Quantity.Unit != commonv1.MassUnit_MASS_UNIT_UNSPECIFIED {
		u, err := massUnits.text(l.Quantity.Unit)
		if err != nil {
			return nil, err
		}
		quantityUnit = &u
	}
	if p := l.Price; p != nil {
		u, err := massUnits.text(p.Unit)
		if err != nil {
			return nil, err
		}
		pUnit, priceMinor, currency = &u, &p.AmountMinor, nullString(p.Currency)
	}
	region, err := regions.text(l.Address.Region)
	if err != nil {
		return nil, err
	}
	if loc := l.Address.Location; loc != nil {
		lat, lon = &loc.Latitude, &loc.Longitude
	}
	media := l.MediaIDs
	if media == nil {
		media = []uuid.UUID{} // NOT NULL; pgx sends a nil slice as NULL
	}
	return []any{int32(l.Product), l.Quantity.Grams, quantityUnit, l.RemainingGrams, priceMinor, currency, pUnit,
		l.Phone, region, l.Address.District, l.Address.Line, lat, lon, l.Notes, media}, nil
}

func (q *queries) LockOwner(ctx context.Context, owner uuid.UUID) error {
	// hashtextextended maps the key to a bigint; a collision only serializes
	// two owners' creations.
	_, err := q.db.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"catalog:listings:"+owner.String())
	if err != nil {
		return fmt.Errorf("lock owner %s: %w", owner, err)
	}
	return nil
}

func (q *queries) ListingByRequest(ctx context.Context, owner, requestID uuid.UUID) (domain.Listing, error) {
	return scanListing(q.db.QueryRow(ctx, `SELECT `+listingCols+`
FROM listings l WHERE l.owner_id = $1 AND l.create_request_id = $2`, owner, requestID))
}

func (q *queries) CountActiveListings(ctx context.Context, owner uuid.UUID, kind commonv1.ListingKind) (int, error) {
	k, err := kinds.text(kind)
	if err != nil {
		return 0, err
	}
	var n int
	err = q.db.QueryRow(ctx, `SELECT count(*) FROM listings
WHERE owner_id = $1 AND kind = $2 AND status = 'ACTIVE'`, owner, k).Scan(&n)
	return n, err
}

func (q *queries) InsertListing(ctx context.Context, l domain.Listing, doc domain.SearchDocument) error {
	kind, err := kinds.text(l.Kind)
	if err != nil {
		return err
	}
	status, err := listingStatuses.text(l.Status)
	if err != nil {
		return err
	}
	values, err := listingValues(l)
	if err != nil {
		return err
	}
	args := append([]any{l.ID, kind, l.OwnerID}, values...)
	args = append(args, status, l.ViewsCount, l.Version, l.CreateRequestID, l.CreatedAt, l.UpdatedAt, l.DeletedAt,
		doc.Product, doc.Place, doc.Notes)
	_, err = q.db.Exec(ctx, `
INSERT INTO listings (id, kind, owner_id, product_type, quantity_grams, quantity_unit, remaining_grams,
	price_minor, price_currency, price_unit, phone, region, district, address_line, latitude, longitude,
	notes, media_ids, status, views_count, version, create_request_id, created_at, updated_at, deleted_at,
	search_document)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22,
	$23, $24, $25, `+searchDocumentSQL("$26", "$27", "$28")+`)`, args...)
	if err != nil {
		return fmt.Errorf("insert listing: %w", err)
	}
	return nil
}

func (q *queries) GetListing(ctx context.Context, id uuid.UUID) (domain.Listing, error) {
	return scanListing(q.db.QueryRow(ctx, `SELECT `+listingCols+` FROM listings l WHERE l.id = $1`, id))
}

func (q *queries) LockListing(ctx context.Context, id uuid.UUID) (domain.Listing, error) {
	return scanListing(q.db.QueryRow(ctx, `SELECT `+listingCols+` FROM listings l WHERE l.id = $1
FOR NO KEY UPDATE`, id))
}

func (q *queries) SaveListing(ctx context.Context, l domain.Listing, doc *domain.SearchDocument) error {
	status, err := listingStatuses.text(l.Status)
	if err != nil {
		return err
	}
	values, err := listingValues(l)
	if err != nil {
		return err
	}
	args := append([]any{l.ID}, values...)
	args = append(args, status, l.Version, l.UpdatedAt, l.DeletedAt)
	document := "search_document"
	if doc != nil {
		args = append(args, doc.Product, doc.Place, doc.Notes)
		document = searchDocumentSQL("$21", "$22", "$23")
	}
	tag, err := q.db.Exec(ctx, `
UPDATE listings SET product_type = $2, quantity_grams = $3, quantity_unit = $4, remaining_grams = $5,
	price_minor = $6, price_currency = $7, price_unit = $8, phone = $9, region = $10, district = $11,
	address_line = $12, latitude = $13, longitude = $14, notes = $15, media_ids = $16, status = $17,
	version = $18, updated_at = $19, deleted_at = $20, search_document = `+document+`
WHERE id = $1`, args...)
	if err != nil {
		return fmt.Errorf("save listing: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	return nil
}

func (q *queries) FindListings(ctx context.Context, lq domain.ListingQuery) ([]domain.Hit, error) {
	sql, args, err := findListingsSQL(lq)
	if err != nil {
		return nil, err
	}
	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("find listings: %w", err)
	}
	hits, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Hit, error) {
		var h domain.Hit
		l, err := scanListing(row, &h.Rank)
		h.Listing = l
		return h, err
	})
	if err != nil {
		return nil, fmt.Errorf("find listings: %w", err)
	}
	return hits, nil
}

// AddViews implements views.Store: it adds the counts to views_count
// without touching version or updated_at, in one statement. Ids are sent in
// order so concurrent flushes tend to lock rows in the same order; a flush
// that still loses a deadlock is retried by the caller.
func (s *Store) AddViews(ctx context.Context, counts map[uuid.UUID]int64) error {
	if len(counts) == 0 {
		return nil
	}
	ids := slices.SortedFunc(maps.Keys(counts), func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	ns := make([]int64, len(ids))
	for i, id := range ids {
		ns[i] = counts[id]
	}
	_, err := s.db.Exec(ctx, `
UPDATE listings AS l SET views_count = l.views_count + v.n
FROM unnest($1::uuid[], $2::bigint[]) AS v (id, n)
WHERE l.id = v.id`, ids, ns)
	if err != nil {
		return fmt.Errorf("add listing views: %w", err)
	}
	return nil
}
