// Package domain holds identity's business types. Enum fields use the
// generated proto enums, so the API layer needs no translation; the Postgres
// repository stores them as readable text.
package domain

import (
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	identityv1 "github.com/tbilol/dxrl/gen/go/identity/v1"
)

// Short names for the enum values identity works with.
const (
	RoleAdmin = commonv1.Role_ROLE_ADMIN

	AccountActive  = identityv1.AccountStatus_ACCOUNT_STATUS_ACTIVE
	AccountBlocked = identityv1.AccountStatus_ACCOUNT_STATUS_BLOCKED
	AccountDeleted = identityv1.AccountStatus_ACCOUNT_STATUS_DELETED
)

// ErrNotFound is returned by repositories when a row does not exist.
var ErrNotFound = errors.New("not found")

// Account is a user account as identity sees it. AGROGRAM has one flat tier
// and no seller verification, so an account carries no plan and no farmer
// status; ADMIN (staff) is the only role.
type Account struct {
	ID        uuid.UUID
	Roles     []commonv1.Role
	Status    identityv1.AccountStatus
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewAccount returns a sign-up account: ACTIVE, no roles (any signed-in user
// may post on either board) and version 1.
func NewAccount(id uuid.UUID, now time.Time) Account {
	return Account{
		ID:        id,
		Status:    AccountActive,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// HasRole reports whether the account holds r.
func (a Account) HasRole(r commonv1.Role) bool { return slices.Contains(a.Roles, r) }
