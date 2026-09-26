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
	RoleReseller = commonv1.Role_ROLE_RESELLER
	RoleFarmer   = commonv1.Role_ROLE_FARMER
	RoleAdmin    = commonv1.Role_ROLE_ADMIN

	KindOffer      = commonv1.ListingKind_LISTING_KIND_OFFER
	KindBuyRequest = commonv1.ListingKind_LISTING_KIND_BUY_REQUEST

	StatusActive  = eventsv1.ListingStatus_LISTING_STATUS_ACTIVE
	StatusClosed  = eventsv1.ListingStatus_LISTING_STATUS_CLOSED
	StatusSoldOut = eventsv1.ListingStatus_LISTING_STATUS_SOLD_OUT
	StatusBlocked = eventsv1.ListingStatus_LISTING_STATUS_BLOCKED
	StatusDeleted = eventsv1.ListingStatus_LISTING_STATUS_DELETED

	UnitKilogram = commonv1.MassUnit_MASS_UNIT_KILOGRAM
	UnitTonne    = commonv1.MassUnit_MASS_UNIT_TONNE

	StateAwaitingPayment  = eventsv1.TransactionState_TRANSACTION_STATE_AWAITING_PAYMENT
	StatePaymentHeld      = eventsv1.TransactionState_TRANSACTION_STATE_PAYMENT_HELD
	StateReceiptSubmitted = eventsv1.TransactionState_TRANSACTION_STATE_RECEIPT_SUBMITTED
	StateHandedOver       = eventsv1.TransactionState_TRANSACTION_STATE_HANDED_OVER
	StateReceived         = eventsv1.TransactionState_TRANSACTION_STATE_RECEIVED
	StateCompleted        = eventsv1.TransactionState_TRANSACTION_STATE_COMPLETED
)

// CurrencyUZS is the currency of every listing price (catalog.proto:
// "price must be a positive amount in UZS").
const CurrencyUZS = "UZS"

// ErrNotFound is returned by repositories when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrInconsistentEvent is wrapped by the service when a consumed event
// contradicts catalog's data or itself: the listing it names does not exist,
// has another kind or another owner, or the transaction's terms differ from
// an earlier event of it. Retrying cannot fix such a record, so the consumer
// dead-letters it at once.
var ErrInconsistentEvent = errors.New("inconsistent event")

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
// board).
type Listing struct {
	ID      uuid.UUID
	Kind    commonv1.ListingKind
	OwnerID uuid.UUID
	ListingFields

	// RemainingGrams is the quantity not yet committed to escrow
	// transactions: Quantity.Grams minus the quantities of every transaction
	// on the listing that catalog has seen on order-escrow's transaction
	// stream, never below 0. An ACTIVE listing becomes SOLD_OUT when it
	// reaches 0.
	RemainingGrams int64
	Status         eventsv1.ListingStatus
	// ViewsCount is how many times non-owners opened the listing. It changes
	// without bumping Version.
	ViewsCount int64
	// Version is the optimistic-concurrency version: 1 on creation, + 1 on
	// every update, on deletion and whenever a new escrow transaction changes
	// the remaining quantity or the status.
	Version int64
	// CreateRequestID is CreateListing's idempotency key.
	CreateRequestID uuid.UUID
	CreatedAt       time.Time
	UpdatedAt       time.Time
	DeletedAt       *time.Time
}

// Remaining is the remaining quantity in the listing's display unit.
func (l Listing) Remaining() Mass { return Mass{Grams: l.RemainingGrams, Unit: l.Quantity.Unit} }

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
	Kind              commonv1.ListingKind   // UNSPECIFIED: both boards
	Products          []commonv1.ProductType // sorted; empty: any
	Regions           []commonv1.Region      // sorted; empty: any
	District          string                 // trimmed; matched case-insensitively; "": any
	OwnerID           uuid.UUID              // uuid.Nil: any owner
	Statuses          []eventsv1.ListingStatus
	MinRemainingGrams int64  // 0: no minimum
	MinPricePerTonne  *int64 // nil: no minimum; set: negotiable listings excluded
	MaxPricePerTonne  *int64 // nil: no maximum; set: negotiable listings excluded
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
		return h.RemainingGrams
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

// Plan names used in PLAN_LIMIT_REACHED metadata (error.proto: {"plan",
// "listing_kind", "limit"}).
const (
	PlanDefault = "default"
	PlanPremium = "premium"
)

// Quotas are the per-kind limits of ACTIVE listings an owner may have, by
// plan (market.proto: offers Default 5 / Premium 20, buy-requests Default 5 /
// Premium 25). The kinds are independent: a user with both roles has both
// quotas.
type Quotas struct {
	OfferDefault      int
	OfferPremium      int
	BuyRequestDefault int
	BuyRequestPremium int
}

// DefaultQuotas are the contract's quotas.
var DefaultQuotas = Quotas{OfferDefault: 5, OfferPremium: 20, BuyRequestDefault: 5, BuyRequestPremium: 25}

// Limit returns the quota of kind for the plan and the plan's name.
func (q Quotas) Limit(kind commonv1.ListingKind, premium bool) (limit int, plan string) {
	switch {
	case kind == KindOffer && premium:
		return q.OfferPremium, PlanPremium
	case kind == KindOffer:
		return q.OfferDefault, PlanDefault
	case premium:
		return q.BuyRequestPremium, PlanPremium
	}
	return q.BuyRequestDefault, PlanDefault
}

// ListingTransaction is catalog's projection of one escrow transaction on a
// listing (table listing_transactions), kept from order-escrow's transaction
// stream. Its quantity is committed from the first event of it catalog sees,
// for good: the contract has no cancel or expiry yet.
type ListingTransaction struct {
	ID        uuid.UUID // the transaction id
	ListingID uuid.UUID
	// FarmerID and ResellerID are the parties: the listing's owner (the
	// farmer of an offer, the reseller of a buy-request) and the counterparty
	// who started the transaction.
	FarmerID   uuid.UUID
	ResellerID uuid.UUID
	// QuantityGrams is the agreed quantity: what the transaction commits of
	// the listing. The terms (listing, parties, quantity) never change.
	QuantityGrams int64
	// State and Version are those of the newest snapshot applied.
	State   eventsv1.TransactionState
	Version int64
	// CreatedAt is when order-escrow created the transaction, RecordedAt when
	// catalog first saw it (the commitment), UpdatedAt when catalog last
	// applied an event of it.
	CreatedAt  time.Time
	RecordedAt time.Time
	UpdatedAt  time.Time
}

// TransactionUpdate is one decoded record of order-escrow's transaction
// stream: the event's identity and the transaction terms and state its
// snapshot carries. Every event type carries all of them (event-carried
// state transfer).
type TransactionUpdate struct {
	EventID    uuid.UUID
	EventType  string // EventMetadata.event_type
	Topic      string
	OccurredAt time.Time
	// Version is the snapshot's version (= EventMetadata.aggregate_version).
	Version int64

	TransactionID uuid.UUID
	ListingID     uuid.UUID
	ListingKind   commonv1.ListingKind
	FarmerID      uuid.UUID
	ResellerID    uuid.UUID
	QuantityGrams int64
	State         eventsv1.TransactionState
	CreatedAt     time.Time // TransactionSnapshot.created_at
}

// ApplyOutcome is what processing one consumed transaction event did.
type ApplyOutcome int8

const (
	// Applied: the event changed the projection. The first event of a
	// transaction committed its quantity; a later one with a newer version
	// updated its state.
	Applied ApplyOutcome = iota + 1
	// Stale: the event was new but changed nothing: its version was not newer
	// than the stored one (an older event after a newer one).
	Stale
	// Duplicate: the event id was processed before; nothing changed.
	Duplicate
)

func (o ApplyOutcome) String() string {
	switch o {
	case Applied:
		return "applied"
	case Stale:
		return "stale"
	case Duplicate:
		return "duplicate"
	}
	return "unknown"
}

// ProcessedEvent is a consumer inbox row (de-duplication on event id).
type ProcessedEvent struct {
	EventID          uuid.UUID
	Topic            string
	EventType        string
	AggregateID      uuid.UUID
	AggregateVersion int64
	ProcessedAt      time.Time
}
