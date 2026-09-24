package postgres

import (
	"fmt"
	"strings"

	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
)

// enumMap maps enum values to the readable text the CHECK constraints in
// migrations/ allow (proto value names without the enum prefix).
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
	profileStatuses = newEnumMap("profile status", map[domain.ProfileStatus]string{
		domain.ProfilePending: "PENDING",
		domain.ProfileActive:  "ACTIVE",
		domain.ProfileDeleted: "DELETED",
	})
	// regions: every defined common.v1.Region but UNSPECIFIED (stored as NULL).
	// The CHECK constraint on profiles.region lists the same names.
	regions = func() enumMap[commonv1.Region] {
		m := make(map[commonv1.Region]string, len(commonv1.Region_name))
		for v, name := range commonv1.Region_name {
			if v != 0 {
				m[commonv1.Region(v)] = strings.TrimPrefix(name, "REGION_")
			}
		}
		return newEnumMap("region", m)
	}()
)
