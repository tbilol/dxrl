package service

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	catalogv1 "github.com/tbilol/dxrl/gen/go/catalog/v1"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"

	"github.com/tbilol/dxrl/services/catalog/internal/apperr"
	"github.com/tbilol/dxrl/services/catalog/internal/auth"
	"github.com/tbilol/dxrl/services/catalog/internal/domain"
)

// Operations named in page-token fingerprints: a token of one never works
// for the other.
const (
	opList   = "list"
	opSearch = "search"
)

// ListListings browses a board (filter.kind) or one owner's listings
// (filter.owner_id; profile-social's ListUserListings), newest first, with
// opaque keyset page tokens. Statuses default to ACTIVE; other statuses are
// allowed only for the caller's own listings (filter.owner_id = the caller)
// or to staff, and are PERMISSION_DENIED otherwise, which profile-social
// expects.
func (s *Service) ListListings(ctx context.Context, p auth.Principal, f domain.ListingFilter,
	page domain.PageRequest,
) (domain.ListingPage, error) {
	c, err := criteria(p, f)
	if err != nil {
		return domain.ListingPage{}, err
	}
	return s.find(ctx, p, opList, domain.ListingQuery{Criteria: c, Sort: domain.SortCreatedAt, Desc: true}, page)
}

// SearchListings searches listings by free text and filters. The text
// matches product names (uz / ru) and keys, the district and the notes, by
// word prefix ("kart" finds kartoshka); every word must match. The sort is
// RELEVANCE by default when there is text, CREATED_AT otherwise; RELEVANCE
// without text falls back to CREATED_AT, and RELEVANCE is descending only.
// PRICE sorts by price per kg with negotiable listings last in both
// directions; QUANTITY by remaining quantity. Ties go newest first.
func (s *Service) SearchListings(ctx context.Context, p auth.Principal, query string, f domain.ListingFilter,
	sortBy catalogv1.ListingSortField, dir commonv1.SortDirection, page domain.PageRequest,
) (domain.ListingPage, error) {
	c, err := criteria(p, f)
	if err != nil {
		return domain.ListingPage{}, err
	}
	c.Terms = queryTerms(query)
	sort, desc, err := searchOrder(sortBy, dir, len(c.Terms) > 0)
	if err != nil {
		return domain.ListingPage{}, err
	}
	return s.find(ctx, p, opSearch, domain.ListingQuery{Criteria: c, Sort: sort, Desc: desc}, page)
}

// find runs q for one page. The page token must come from the same query
// (fingerprint of the operation, criteria and sort); a foreign one is
// INVALID_ARGUMENT.
func (s *Service) find(ctx context.Context, p auth.Principal, op string, q domain.ListingQuery,
	page domain.PageRequest,
) (domain.ListingPage, error) {
	fp := fingerprint(op, q)
	after, err := decodePageToken(page.Token, fp)
	if err != nil {
		return domain.ListingPage{}, err
	}
	size := pageSize(page.Size)
	q.After, q.Limit = after, size+1
	var hits []domain.Hit
	err = s.store.Run(ctx, func(qs Queries) error {
		hits, err = qs.FindListings(ctx, q)
		return err
	})
	if err != nil {
		return domain.ListingPage{}, fmt.Errorf("find listings: %w", err)
	}
	out := domain.ListingPage{Listings: make([]domain.Listing, 0, min(len(hits), size))}
	for _, h := range hits[:min(len(hits), size)] {
		// Defence in depth: the criteria already restrict the statuses.
		if canSee(p, h.Listing) {
			out.Listings = append(out.Listings, h.Listing)
		}
	}
	if len(hits) > size {
		last := hits[size-1]
		out.NextPageToken = encodePageToken(fp, domain.Cursor{
			Key: last.SortKey(q.Sort, q.Desc), Rank: last.Rank, CreatedAt: last.CreatedAt, ID: last.ID,
		})
	}
	return out, nil
}

// criteria normalizes and authorizes a filter: sorted, distinct values;
// statuses ACTIVE unless others are asked for, which only the owner of the
// listings (filter.owner_id = the caller) and staff may do; prices converted
// to per-tonne minor units, in UZS, with min <= max across units.
func criteria(p auth.Principal, f domain.ListingFilter) (domain.Criteria, error) {
	c := domain.Criteria{
		Kind:     f.Kind,
		Products: sortedSet(f.Products),
		Regions:  sortedSet(f.Regions),
		District: strings.TrimSpace(f.District),
		Statuses: sortedSet(f.Statuses),
	}
	if f.OwnerID != "" {
		id, err := parseID("filter.owner_id", f.OwnerID)
		if err != nil {
			return c, err
		}
		c.OwnerID = id
	}
	if len(c.Statuses) == 0 {
		c.Statuses = []eventsv1.ListingStatus{domain.StatusActive}
	}
	if i := slices.IndexFunc(c.Statuses, func(st eventsv1.ListingStatus) bool { return st != domain.StatusActive }); i >= 0 {
		own := p.Authenticated() && c.OwnerID == p.UserID
		if !own && !p.Staff() {
			return c, apperr.New(commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED,
				"only the owner (filter.owner_id = the caller) or staff may list listings that are not ACTIVE",
				"status", statusName(c.Statuses[i]))
		}
	}
	if f.MinQuantity != nil {
		c.MinRemainingGrams = max(f.MinQuantity.Grams, 0)
	}
	var err error
	if c.MinPricePerTonne, err = perTonne("filter.min_price", f.MinPrice); err != nil {
		return c, err
	}
	if c.MaxPricePerTonne, err = perTonne("filter.max_price", f.MaxPrice); err != nil {
		return c, err
	}
	if c.MinPricePerTonne != nil && c.MaxPricePerTonne != nil && *c.MinPricePerTonne > *c.MaxPricePerTonne {
		return c, invalid("filter", "min_price must not exceed max_price", "listing_filter.price_range")
	}
	return c, nil
}

// perTonne converts a filter price to minor units per tonne; nil stays nil.
func perTonne(field string, p *domain.Price) (*int64, error) {
	if p == nil {
		return nil, nil
	}
	if p.Currency != domain.CurrencyUZS {
		return nil, invalid(field+".amount.currency_code", "prices are in UZS", "listing_filter.price_uzs")
	}
	if p.AmountMinor < 0 {
		return nil, invalid(field+".amount.amount_minor", "price must not be negative", "unit_price.amount.non_negative")
	}
	v, ok := p.PerTonneMinor()
	if !ok {
		return nil, invalid(field, "price must be per MASS_UNIT_KILOGRAM or MASS_UNIT_TONNE and not absurdly large",
			"listing_filter.price_range")
	}
	return &v, nil
}

// searchOrder resolves SearchListings' sort: UNSPECIFIED is RELEVANCE with
// text and CREATED_AT without; RELEVANCE without text is CREATED_AT;
// RELEVANCE ascending is INVALID_ARGUMENT; UNSPECIFIED direction is
// descending.
func searchOrder(field catalogv1.ListingSortField, dir commonv1.SortDirection, hasText bool) (domain.SortField, bool, error) {
	desc := dir != commonv1.SortDirection_SORT_DIRECTION_ASC
	switch field {
	case catalogv1.ListingSortField_LISTING_SORT_FIELD_UNSPECIFIED, catalogv1.ListingSortField_LISTING_SORT_FIELD_RELEVANCE:
		if !hasText {
			return domain.SortCreatedAt, desc, nil
		}
		if !desc {
			return "", false, invalid("sort_direction", "RELEVANCE sorts descending only", "listing_sort.relevance_desc")
		}
		return domain.SortRelevance, true, nil
	case catalogv1.ListingSortField_LISTING_SORT_FIELD_CREATED_AT:
		return domain.SortCreatedAt, desc, nil
	case catalogv1.ListingSortField_LISTING_SORT_FIELD_PRICE:
		return domain.SortPrice, desc, nil
	case catalogv1.ListingSortField_LISTING_SORT_FIELD_QUANTITY:
		return domain.SortQuantity, desc, nil
	}
	return "", false, invalid("sort_by", "sort_by must be a defined value", "enum.defined_only")
}

// sortedSet returns the distinct values of vs in ascending order (nil for
// none).
func sortedSet[E cmp.Ordered](vs []E) []E {
	if len(vs) == 0 {
		return nil
	}
	return slices.Compact(slices.Sorted(slices.Values(vs)))
}
