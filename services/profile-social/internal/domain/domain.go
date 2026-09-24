// Package domain holds profile-social's business types. Enum fields use the
// generated proto enums, so the API layer needs no translation; the Postgres
// repository stores them as readable text.
package domain

import (
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	profilesocialv1 "github.com/tbilol/dxrl/gen/go/profilesocial/v1"
)

// Short names for the enum values profile-social works with.
const (
	RoleReseller = commonv1.Role_ROLE_RESELLER
	RoleFarmer   = commonv1.Role_ROLE_FARMER
	RoleAdmin    = commonv1.Role_ROLE_ADMIN

	ListingActive = eventsv1.ListingStatus_LISTING_STATUS_ACTIVE
)

var (
	// ErrNotFound is returned by repositories when a row does not exist.
	ErrNotFound = errors.New("not found")
	// ErrDependencyUnavailable marks a failed call to another service that
	// the caller may degrade on: not configured, down, too slow, or not
	// implementing the method yet.
	ErrDependencyUnavailable = errors.New("dependency unavailable")
)

// ProfileStatus is where a profile row is in its life cycle.
type ProfileStatus int8

const (
	// ProfilePending: the row exists (an account event or a received rating
	// created it) but the user has not created the profile yet. Not public.
	ProfilePending ProfileStatus = iota + 1
	// ProfileActive: created by the user (CreateProfile). Public.
	ProfileActive
	// ProfileDeleted: soft-deleted (DeleteProfile). Not public; the fields,
	// ratings and transaction history stay, follow edges are removed.
	ProfileDeleted
)

func (s ProfileStatus) String() string {
	switch s {
	case ProfilePending:
		return "PENDING"
	case ProfileActive:
		return "ACTIVE"
	case ProfileDeleted:
		return "DELETED"
	}
	return "UNKNOWN"
}

// Account is the account projection: identity's AccountSnapshot as last
// applied from agrogram.identity.account.v1.
type Account struct {
	Roles            []commonv1.Role // sorted, no duplicates
	IsVerifiedFarmer bool
	// IsPremium is Premium at the event's occurred_at; PremiumUntil ends it
	// (identity publishes no event when that moment passes).
	IsPremium    bool
	PremiumUntil *time.Time
	// Version is the applied snapshot's version; 0 = no account event yet.
	Version int64
}

// PremiumActive reports whether the Premium badge shows at now.
func (a Account) PremiumActive(now time.Time) bool {
	return a.IsPremium && (a.PremiumUntil == nil || now.Before(*a.PremiumUntil))
}

// ProfileFields are the public fields the user owns.
type ProfileFields struct {
	DisplayName   string
	AvatarMediaID uuid.UUID // uuid.Nil: no avatar
	Bio           string
	Region        commonv1.Region
	District      string
}

// UpdateProfile's update_mask paths.
const (
	FieldDisplayName = "display_name"
	FieldAvatar      = "avatar"
	FieldBio         = "bio"
	FieldRegion      = "region"
	FieldDistrict    = "district"
)

// Profile is one profile row: the user's public fields, counters and the
// account projection.
type Profile struct {
	ID     uuid.UUID
	Status ProfileStatus
	ProfileFields

	// CreateRequestID is the CreateProfile idempotency key of the current
	// creation (uuid.Nil while PENDING).
	CreateRequestID uuid.UUID
	// Version is the optimistic-concurrency version: 0 until the user creates
	// the profile, + 1 on every create, update, delete and received rating.
	Version   int64
	CreatedAt time.Time // zero while PENDING
	UpdatedAt time.Time
	DeletedAt *time.Time

	FollowerCount  int64
	FollowingCount int64
	RatingCount    int64
	RatingSum      int64
	// DealsCount counts completed escrow transactions. TODO(phase3): stays 0
	// until the transaction stream consumer calls RecordCompletedTransaction.
	DealsCount int64

	Account Account

	// IsFollowedByMe is filled by the repository for the viewer it is given.
	IsFollowedByMe bool
	// IsPremium is computed by the service from Account and its clock;
	// repositories leave it false.
	IsPremium bool
}

// NewPendingProfile is the row an account event (or a received rating)
// creates for a user who has not created a profile yet.
func NewPendingProfile(id uuid.UUID, now time.Time) Profile {
	return Profile{ID: id, Status: ProfilePending, UpdatedAt: now}
}

// Public reports whether the profile is visible through the API.
func (p Profile) Public() bool { return p.Status == ProfileActive }

// RatingAverage is the mean score, 0 when there are no ratings yet.
func (p Profile) RatingAverage() float64 {
	if p.RatingCount == 0 {
		return 0
	}
	return float64(p.RatingSum) / float64(p.RatingCount)
}

// HasRole reports whether the account projection holds r.
func (p Profile) HasRole(r commonv1.Role) bool { return slices.Contains(p.Account.Roles, r) }

// AccountUpdate is one decoded record of identity's account stream: the
// event's identity and the AccountSnapshot it carries.
type AccountUpdate struct {
	EventID    uuid.UUID
	EventType  string // EventMetadata.event_type
	Topic      string
	OccurredAt time.Time
	UserID     uuid.UUID
	Account    Account
}

// ApplyOutcome is what applying an account update did.
type ApplyOutcome int8

const (
	// Applied: the snapshot was newer and is now the projection.
	Applied ApplyOutcome = iota + 1
	// Stale: the snapshot was not newer than the projection (an older event
	// after a newer one, or an event that repeats the current version).
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

// CompletedTransaction is a completed escrow transaction between two users;
// it unlocks one rating per party.
type CompletedTransaction struct {
	ID          uuid.UUID
	FarmerID    uuid.UUID
	ResellerID  uuid.UUID
	CompletedAt time.Time
}

// Counterparty returns the other party of the transaction for user, and
// false when user is not a party.
func (t CompletedTransaction) Counterparty(user uuid.UUID) (uuid.UUID, bool) {
	switch user {
	case t.FarmerID:
		return t.ResellerID, true
	case t.ResellerID:
		return t.FarmerID, true
	}
	return uuid.Nil, false
}

// Rating is one user's score for the other party of a completed transaction.
type Rating struct {
	ID            uuid.UUID
	TransactionID uuid.UUID
	RaterID       uuid.UUID
	RatedUserID   uuid.UUID
	RequestID     uuid.UUID
	Score         int32
	Comment       string
	CreatedAt     time.Time
}

// Cursor is a keyset position in a newest-first list: the sort time and the
// tie-breaking id of the last item returned.
type Cursor struct {
	At time.Time
	ID uuid.UUID
}

// PageRequest is common.v1.PageRequest.
type PageRequest struct {
	Size  int32
	Token string
}

// FollowEntry is a profile in a follower or following list and when the
// follow happened (the list's sort key).
type FollowEntry struct {
	Profile    Profile
	FollowedAt time.Time
}

// ProfilePage is one page of ListFollowers / ListFollowing.
type ProfilePage struct {
	Profiles      []Profile
	NextPageToken string
	TotalSize     int64
}

// RatingPage is one page of ListRatings plus the aggregate.
type RatingPage struct {
	Ratings       []Rating
	Average       float64
	Count         int64
	NextPageToken string
}

// ListingQuery asks catalog for one owner's listings.
type ListingQuery struct {
	OwnerID  uuid.UUID
	Kind     commonv1.ListingKind
	Statuses []eventsv1.ListingStatus // empty: ACTIVE
	Page     PageRequest
}

// ListingPage is one page of a user's listings as profile-social serves them.
// The page token is catalog's own and is passed through unchanged.
type ListingPage struct {
	Listings      []*profilesocialv1.ListingCard
	NextPageToken string
}
