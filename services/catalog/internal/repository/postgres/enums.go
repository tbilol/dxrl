package postgres

import (
	"fmt"
	"strings"

	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"

	"github.com/tbilol/dxrl/services/catalog/internal/domain"
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

// fromProto maps every defined value of a proto enum but the zero one to its
// name without prefix.
func fromProto[E ~int32](names map[int32]string, prefix string) map[E]string {
	m := make(map[E]string, len(names))
	for v, name := range names {
		if v != 0 {
			m[E(v)] = strings.TrimPrefix(name, prefix)
		}
	}
	return m
}

var (
	kinds = newEnumMap("listing kind", map[commonv1.ListingKind]string{
		domain.KindOffer:      "OFFER",
		domain.KindBuyRequest: "BUY_REQUEST",
	})
	listingStatuses = newEnumMap("listing status", map[eventsv1.ListingStatus]string{
		domain.StatusActive:  "ACTIVE",
		domain.StatusClosed:  "CLOSED",
		domain.StatusSoldOut: "SOLD_OUT",
		domain.StatusBlocked: "BLOCKED",
		domain.StatusDeleted: "DELETED",
	})
	// massUnits: MASS_UNIT_UNSPECIFIED (a quantity's "pick a unit") is NULL.
	massUnits = newEnumMap("mass unit", map[commonv1.MassUnit]string{
		domain.UnitKilogram: "KILOGRAM",
		domain.UnitTonne:    "TONNE",
	})
	// regions and categories: every defined value but UNSPECIFIED; the CHECK
	// constraints list the same names.
	regions    = newEnumMap("region", fromProto[commonv1.Region](commonv1.Region_name, "REGION_"))
	categories = newEnumMap("product category",
		fromProto[commonv1.ProductCategory](commonv1.ProductCategory_name, "PRODUCT_CATEGORY_"))
)
