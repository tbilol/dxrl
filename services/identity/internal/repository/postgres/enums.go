package postgres

import (
	"fmt"

	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	identityv1 "github.com/tbilol/dxrl/gen/go/identity/v1"

	"github.com/tbilol/dxrl/services/identity/internal/domain"
)

// enumMap maps proto enum values to the readable text the CHECK constraints
// in migrations/ allow (the value name without the enum prefix).
type enumMap[E comparable] struct {
	name   string
	toDB   map[E]string
	fromDB map[string]E
}

func newEnumMap[E comparable](name string, m map[E]string) enumMap[E] {
	rev := make(map[string]E, len(m))
	for k, v := range m {
		rev[v] = k
	}
	return enumMap[E]{name: name, toDB: m, fromDB: rev}
}

func (m enumMap[E]) text(v E) (string, error) {
	s, ok := m.toDB[v]
	if !ok {
		return "", fmt.Errorf("%s %v cannot be stored", m.name, v)
	}
	return s, nil
}

func (m enumMap[E]) value(s string) (E, error) {
	v, ok := m.fromDB[s]
	if !ok {
		var zero E
		return zero, fmt.Errorf("unknown %s %q in database", m.name, s)
	}
	return v, nil
}

var (
	roles = newEnumMap("role", map[commonv1.Role]string{
		domain.RoleReseller: "RESELLER",
		domain.RoleFarmer:   "FARMER",
		domain.RoleAdmin:    "ADMIN",
	})
	accountStatuses = newEnumMap("account status", map[identityv1.AccountStatus]string{
		domain.AccountActive:  "ACTIVE",
		domain.AccountBlocked: "BLOCKED",
		domain.AccountDeleted: "DELETED",
	})
	plans = newEnumMap("plan", map[identityv1.Plan]string{
		domain.PlanDefault: "DEFAULT",
		domain.PlanPremium: "PREMIUM",
	})
	verificationStatuses = newEnumMap("verification status", map[identityv1.FarmerVerificationStatus]string{
		domain.VerificationPending:  "PENDING",
		domain.VerificationVerified: "VERIFIED",
		domain.VerificationRejected: "REJECTED",
		domain.VerificationExpired:  "EXPIRED",
	})
	rejectionReasons = newEnumMap("rejection reason", map[commonv1.ErrorReason]string{
		domain.ReasonContractInvalid: "LEASE_CONTRACT_INVALID",
		domain.ReasonContractClaimed: "LEASE_CONTRACT_ALREADY_CLAIMED",
	})
)
