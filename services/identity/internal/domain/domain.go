// Package domain holds identity's business types. Enum fields use the
// generated proto enums, so the API layer needs no translation; the Postgres
// repository stores them as readable text.
package domain

import (
	"errors"
	"slices"
	"time"
	_ "time/tzdata" // distroless images have no zoneinfo

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	identityv1 "github.com/tbilol/dxrl/gen/go/identity/v1"
)

// Short names for the enum values identity works with.
const (
	RoleReseller = commonv1.Role_ROLE_RESELLER
	RoleFarmer   = commonv1.Role_ROLE_FARMER
	RoleAdmin    = commonv1.Role_ROLE_ADMIN

	AccountActive  = identityv1.AccountStatus_ACCOUNT_STATUS_ACTIVE
	AccountBlocked = identityv1.AccountStatus_ACCOUNT_STATUS_BLOCKED
	AccountDeleted = identityv1.AccountStatus_ACCOUNT_STATUS_DELETED

	PlanDefault = identityv1.Plan_PLAN_DEFAULT
	PlanPremium = identityv1.Plan_PLAN_PREMIUM

	VerificationPending  = identityv1.FarmerVerificationStatus_FARMER_VERIFICATION_STATUS_PENDING
	VerificationVerified = identityv1.FarmerVerificationStatus_FARMER_VERIFICATION_STATUS_VERIFIED
	VerificationRejected = identityv1.FarmerVerificationStatus_FARMER_VERIFICATION_STATUS_REJECTED
	VerificationExpired  = identityv1.FarmerVerificationStatus_FARMER_VERIFICATION_STATUS_EXPIRED

	ReasonContractInvalid = commonv1.ErrorReason_ERROR_REASON_LEASE_CONTRACT_INVALID
	ReasonContractClaimed = commonv1.ErrorReason_ERROR_REASON_LEASE_CONTRACT_ALREADY_CLAIMED
)

var (
	// ErrNotFound is returned by repositories when a row does not exist.
	ErrNotFound = errors.New("not found")
	// ErrContractClaimed is returned when marking a verification VERIFIED
	// would give a lease contract a second VERIFIED verification (the partial
	// unique index tripped).
	ErrContractClaimed = errors.New("lease contract already claimed by another verification")
)

// Tashkent is the zone of every calendar-day rule ("today", lease validity).
var Tashkent = mustLoadLocation("Asia/Tashkent")

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err) // time/tzdata is embedded, so this cannot happen
	}
	return loc
}

// TashkentDate returns the calendar date of t in Asia/Tashkent, as 00:00 UTC
// of that date. Civil dates are represented this way throughout identity (and
// map to Postgres `date`).
func TashkentDate(t time.Time) time.Time {
	y, m, d := t.In(Tashkent).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// Account is a user account as identity sees it.
type Account struct {
	ID           uuid.UUID
	Roles        []commonv1.Role
	Status       identityv1.AccountStatus
	Plan         identityv1.Plan
	PremiumUntil *time.Time
	Version      int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// FarmerVerificationStatus is the status of the latest verification,
	// UNSPECIFIED when the user never submitted one.
	FarmerVerificationStatus identityv1.FarmerVerificationStatus
	// IsPremium is computed by the service from Plan, PremiumUntil and its
	// clock; repositories leave it false.
	IsPremium bool
}

// NewAccount returns a sign-up account: ACTIVE, ROLE_RESELLER, the Default
// plan and version 1.
func NewAccount(id uuid.UUID, now time.Time) Account {
	return Account{
		ID:        id,
		Roles:     []commonv1.Role{RoleReseller},
		Status:    AccountActive,
		Plan:      PlanDefault,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// HasRole reports whether the account holds r.
func (a Account) HasRole(r commonv1.Role) bool { return slices.Contains(a.Roles, r) }

// PremiumActive reports whether Premium is active at now: the plan is PREMIUM
// and now is before PremiumUntil (no end date means active).
func (a Account) PremiumActive(now time.Time) bool {
	return a.Plan == PlanPremium && (a.PremiumUntil == nil || now.Before(*a.PremiumUntil))
}

// Lease is the land-lease contract e-ijara confirmed.
type Lease struct {
	ContractNumber string
	// ValidFrom and ValidUntil are civil dates (see TashkentDate). ValidUntil
	// is the last day of the lease.
	ValidFrom  time.Time
	ValidUntil time.Time
}

// Verification is one attempt to verify a user as a farmer through e-ijara.
type Verification struct {
	ID              uuid.UUID
	UserID          uuid.UUID
	RequestID       uuid.UUID
	ContractNumber  string
	Status          identityv1.FarmerVerificationStatus
	RejectionReason commonv1.ErrorReason
	Lease           *Lease
	// Attempts counts background e-ijara retries; NextAttemptAt is when the
	// worker may retry a PENDING verification next.
	Attempts      int
	NextAttemptAt time.Time
	SubmittedAt   time.Time
	DecidedAt     *time.Time
}

// Decision is e-ijara's answer applied to a PENDING verification.
type Decision struct {
	VerificationID  uuid.UUID
	Status          identityv1.FarmerVerificationStatus // VERIFIED or REJECTED
	RejectionReason commonv1.ErrorReason                // set when REJECTED
	Lease           *Lease                              // set when VERIFIED
	DecidedAt       time.Time
}

// Apply returns v with the decision applied.
func (v Verification) Apply(d Decision) Verification {
	v.Status = d.Status
	v.RejectionReason = d.RejectionReason
	v.Lease = d.Lease
	decided := d.DecidedAt
	v.DecidedAt = &decided
	return v
}
