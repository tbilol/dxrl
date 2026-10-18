// Package domain holds catalog's business types. Enum fields use the
// generated proto enums, so the API layer needs no translation; the Postgres
// repository stores them as readable text.
package domain

import (
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
)

// Short names for the enum values catalog works with.
const (
	RoleAdmin = commonv1.Role_ROLE_ADMIN

	KindOffer      = commonv1.ListingKind_LISTING_KIND_OFFER
	KindBuyRequest = commonv1.ListingKind_LISTING_KIND_BUY_REQUEST

	StatusActive  = eventsv1.ListingStatus_LISTING_STATUS_ACTIVE
	StatusClosed  = eventsv1.ListingStatus_LISTING_STATUS_CLOSED
	StatusBlocked = eventsv1.ListingStatus_LISTING_STATUS_BLOCKED
	StatusDeleted = eventsv1.ListingStatus_LISTING_STATUS_DELETED

	UnitKilogram = commonv1.MassUnit_MASS_UNIT_KILOGRAM
	UnitTonne    = commonv1.MassUnit_MASS_UNIT_TONNE
)

// CurrencyUZS is the currency of every listing price (catalog.proto:
// "price must be a positive amount in UZS").
const CurrencyUZS = "UZS"

// ErrNotFound is returned by repositories when a row does not exist.
var ErrNotFound = errors.New("not found")

// Field names of UpdateListing's update_mask (catalog.proto restricts the
// mask to these).
const (
	FieldProduct  = "product"
	FieldQuantity = "quantity"
	FieldPrice    = "price"
	FieldPhone    = "phone"
	FieldAddress  = "address"
	FieldNotes    = "notes"
	FieldMedia    = "media"
)

// AllFields are the owner's fields, all of which CreateListing sets.
var AllFields = []string{FieldProduct, FieldQuantity, FieldPrice, FieldPhone, FieldAddress, FieldNotes, FieldMedia}

// Mass is common.v1.Mass: an exact number of grams and the unit to show it in.
type Mass struct {
	Grams int64
	Unit  commonv1.MassUnit // UNSPECIFIED: the client picks a unit
}

// Price is common.v1.UnitPrice: minor units (tiyin) per one kilogram or tonne.
type Price struct {
	AmountMinor int64
	Currency    string
	Unit        commonv1.MassUnit
}

// PerTonneMinor is the price of one tonne in minor units, which is the price
// per kg times 1000. It is exact for both units, so prices quoted per kg and
// per tonne compare and sort correctly (listings.price_per_tonne_minor is the
// same value). ok is false for an unknown unit or when the value overflows.
func (p Price) PerTonneMinor() (int64, bool) {
	switch p.Unit {
	case UnitTonne:
		return p.AmountMinor, true
	case UnitKilogram:
		if p.AmountMinor > math.MaxInt64/1000 || p.AmountMinor < math.MinInt64/1000 {
			return 0, false
		}
		return p.AmountMinor * 1000, true
	}
	return 0, false
}

// GeoPoint is common.v1.GeoPoint.
type GeoPoint struct {
	Latitude  float64
	Longitude float64
}

// Address is common.v1.Address.
type Address struct {
	Region   commonv1.Region
	District string
	Line     string
	Location *GeoPoint // nil: no map pin
}

// ListingFields are the fields the owner sets: all of them on CreateListing,
// the update_mask ones on UpdateListing.
type ListingFields struct {
	Product  commonv1.ProductType
	Quantity Mass
	Price    *Price // nil: negotiable ("kelishiladi")
	Phone    string
	Address  Address
	Notes    string
	// MediaIDs are media-pipeline object ids in display order. Only ids are
	// stored; type, status and URLs are media-pipeline's (TODO(media)).
	MediaIDs []uuid.UUID
}

// Listing is a sell offer (farmers' board) or a buy-request (resellers'
// board). Any signed-in user may post either; the board is the listing's
// kind, not a role.
type Listing struct {
	ID      uuid.UUID
	Kind    commonv1.ListingKind
	OwnerID uuid.UUID
	ListingFields

	Status eventsv1.ListingStatus
	// ViewsCount is how many times non-owners opened the listing. It changes
	// without bumping Version.
	ViewsCount int64
	// Version is the optimistic-concurrency version: 1 on creation, + 1 on
	// every update and on deletion.
	Version int64
	// CreateRequestID is CreateListing's idempotency key.
	CreateRequestID uuid.UUID
	CreatedAt       time.Time
	UpdatedAt       time.Time
	DeletedAt       *time.Time
}

// ProductType is the reference data of one common.v1.ProductType (table
// product_types, seeded by migrations/00002_seed_product_types.sql).
type ProductType struct {
	Type      commonv1.ProductType
	Key       string // front-end key, e.g. "achchiq-qalampir"
	NameUZ    string
	NameRU    string
	Category  commonv1.ProductCategory
	SortOrder int32
	// Retired types keep their labels on old listings but cannot be used by
	// new ones.
	Retired bool
}

// SearchDocument is the text SearchListings matches a listing against, by
// weight: A the product's names and key, B the district, C the notes. Each
// part is normalized text (lower case, apostrophes removed, words separated
// by single spaces; see service.Terms).
type SearchDocument struct {
	Product string
	Place   string
	Notes   string
}

// ListingFilter is the filter of ListListings and SearchListings as
// requested (catalog.v1.ListingFilter).
type ListingFilter struct {
	Kind        commonv1.ListingKind // UNSPECIFIED: both boards
	Products    []commonv1.ProductType
	Regions     []commonv1.Region
	District    string
	OwnerID     string                   // "": any owner
	Statuses    []eventsv1.ListingStatus // empty: ACTIVE
	MinQuantity *Mass
	MinPrice    *Price
	MaxPrice    *Price
}

// SortField is what a listing query is ordered by.
type SortField string

// Sort fields. Ties are broken by created_at and id, newest first.
const (
	SortCreatedAt SortField = "created_at"
	SortPrice     SortField = "price" // per kg; negotiable listings last in both directions
	SortQuantity  SortField = "quantity"
	SortRelevance SortField = "relevance" // text rank, descending only
)

// Criteria is a normalized, authorized listing filter: the WHERE clause of a
// listing query.
type Criteria struct {
	Kind             commonv1.ListingKind   // UNSPECIFIED: both boards
	Products         []commonv1.ProductType // sorted; empty: any
	Regions          []commonv1.Region      // sorted; empty: any
	District         string                 // trimmed; matched case-insensitively; "": any
	OwnerID          uuid.UUID              // uuid.Nil: any owner
	Statuses         []eventsv1.ListingStatus
	MinQuantityGrams int64  // 0: no minimum
	MinPricePerTonne *int64 // nil: no minimum; set: negotiable listings excluded
	MaxPricePerTonne *int64 // nil: no maximum; set: negotiable listings excluded
	// Terms are normalized search words; a listing matches when its
	// SearchDocument has a word starting with each of them.
	Terms []string
}

// ListingQuery asks for one page of listings.
type ListingQuery struct {
	Criteria Criteria
	Sort     SortField
	Desc     bool
	After    *Cursor // nil: the first page
	Limit    int
}

// Cursor is the keyset position of the last listing of a page: the primary
// sort key (Key for price and quantity, Rank for relevance) and the
// tie-breakers.
type Cursor struct {
	Key       int64
	Rank      float32
	CreatedAt time.Time
	ID        uuid.UUID
}

// NegotiableSortKey is the price sort key of a listing without a price, so
// negotiable listings come last in both directions (prices are positive).
func NegotiableSortKey(desc bool) int64 {
	if desc {
		return -1
	}
	return math.MaxInt64
}

// Hit is a listing a query found, with its text rank (0 without terms).
type Hit struct {
	Listing
	Rank float32
}

// SortKey is h's primary sort key for s (Key of a Cursor).
func (h Hit) SortKey(s SortField, desc bool) int64 {
	switch s {
	case SortPrice:
		if h.Price == nil {
			return NegotiableSortKey(desc)
		}
		v, _ := h.Price.PerTonneMinor()
		return v
	case SortQuantity:
		return h.Quantity.Grams
	}
	return 0
}

// PageRequest is common.v1.PageRequest.
type PageRequest struct {
	Size  int32
	Token string
}

// ListingPage is one page of ListListings or SearchListings.
type ListingPage struct {
	Listings      []Listing
	NextPageToken string
}

// DefaultListingQuota is how many ACTIVE listings every user may have on
// each board (market.proto): one flat tier, the boards counted separately.
const DefaultListingQuota = 10
