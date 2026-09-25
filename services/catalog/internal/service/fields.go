package service

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/catalog/internal/apperr"
	"github.com/tbilol/dxrl/services/catalog/internal/domain"
)

// Limits of catalog.proto, re-checked here for direct callers and after
// trimming.
const (
	maxNotes       = 1000
	maxDistrict    = 120
	maxAddressLine = 255
	maxMedia       = 10
)

var phonePattern = regexp.MustCompile(`^\+998[0-9]{9}$`)

// normalize trims the text fields named in paths and checks what the
// contract cannot express (or what direct callers may skip): defined enums,
// a positive quantity, a UZS price whose per-tonne value fits int64, and
// distinct media ids. Paths not in domain.AllFields are ignored.
func normalize(f domain.ListingFields, paths []string) (domain.ListingFields, error) {
	var violations []*commonv1.FieldViolation
	bad := func(field, desc, rule string) {
		violations = append(violations, &commonv1.FieldViolation{Field: "listing." + field, Description: desc, Rule: rule})
	}
	for _, path := range paths {
		switch path {
		case domain.FieldProduct:
			if _, ok := commonv1.ProductType_name[int32(f.Product)]; !ok || f.Product == 0 {
				bad(path, "product is required and must be a defined value", "create_listing.product_required")
			}
		case domain.FieldQuantity:
			if f.Quantity.Grams <= 0 {
				bad("quantity.grams", "quantity must be positive", "listing.quantity_positive")
			}
			if _, ok := commonv1.MassUnit_name[int32(f.Quantity.Unit)]; !ok {
				bad("quantity.display_unit", "display_unit must be a defined value", "enum.defined_only")
			}
		case domain.FieldPrice:
			if f.Price == nil {
				break // negotiable
			}
			if f.Price.Currency != domain.CurrencyUZS || f.Price.AmountMinor <= 0 {
				bad("price", "price must be a positive amount in UZS", "listing.price_positive_uzs")
			} else if _, ok := f.Price.PerTonneMinor(); !ok {
				bad("price", "price must be per MASS_UNIT_KILOGRAM or MASS_UNIT_TONNE and not absurdly large",
					"listing.price_range")
			}
		case domain.FieldPhone:
			f.Phone = strings.TrimSpace(f.Phone)
			if !phonePattern.MatchString(f.Phone) {
				bad(path, "phone must be +998 followed by 9 digits", "string.pattern")
			}
		case domain.FieldAddress:
			a := &f.Address
			if _, ok := commonv1.Region_name[int32(a.Region)]; !ok || a.Region == 0 {
				bad("address.region", "region is required and must be a defined value", "required")
			}
			a.District = strings.TrimSpace(a.District)
			if utf8.RuneCountInString(a.District) > maxDistrict {
				bad("address.district", "district must be at most 120 characters", "string.max_len")
			}
			a.Line = strings.TrimSpace(a.Line)
			if utf8.RuneCountInString(a.Line) > maxAddressLine {
				bad("address.address_line", "address_line must be at most 255 characters", "string.max_len")
			}
			if l := a.Location; l != nil && (l.Latitude < -90 || l.Latitude > 90 || l.Longitude < -180 || l.Longitude > 180) {
				bad("address.location", "latitude must be -90..90 and longitude -180..180", "double.range")
			}
		case domain.FieldNotes:
			f.Notes = strings.TrimSpace(f.Notes)
			if utf8.RuneCountInString(f.Notes) > maxNotes {
				bad(path, "notes must be at most 1000 characters", "string.max_len")
			}
		case domain.FieldMedia:
			if len(f.MediaIDs) > maxMedia {
				bad(path, "at most 10 media", "repeated.max_items")
			}
			seen := make(map[uuid.UUID]bool, len(f.MediaIDs))
			for _, id := range f.MediaIDs {
				if id == uuid.Nil || seen[id] {
					bad(path, "media ids must be distinct UUIDs", "repeated.unique")
					break
				}
				seen[id] = true
			}
		}
	}
	if len(violations) > 0 {
		return f, apperr.Validation(violations...)
	}
	return f, nil
}

// applyMask copies the masked fields of f into dst and reports whether any
// value changed.
func applyMask(dst *domain.ListingFields, f domain.ListingFields, mask []string) bool {
	changed := false
	for _, path := range mask {
		switch path {
		case domain.FieldProduct:
			changed = set(&dst.Product, f.Product) || changed
		case domain.FieldQuantity:
			changed = set(&dst.Quantity, f.Quantity) || changed
		case domain.FieldPrice:
			if !samePrice(dst.Price, f.Price) {
				dst.Price, changed = clonePrice(f.Price), true
			}
		case domain.FieldPhone:
			changed = set(&dst.Phone, f.Phone) || changed
		case domain.FieldAddress:
			if !sameAddress(dst.Address, f.Address) {
				dst.Address, changed = cloneAddress(f.Address), true
			}
		case domain.FieldNotes:
			changed = set(&dst.Notes, f.Notes) || changed
		case domain.FieldMedia:
			if !slices.Equal(dst.MediaIDs, f.MediaIDs) {
				dst.MediaIDs, changed = slices.Clone(f.MediaIDs), true
			}
		}
	}
	return changed
}

func set[T comparable](dst *T, v T) bool {
	if *dst == v {
		return false
	}
	*dst = v
	return true
}

func samePrice(a, b *domain.Price) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func clonePrice(p *domain.Price) *domain.Price {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}

func sameAddress(a, b domain.Address) bool {
	if a.Region != b.Region || a.District != b.District || a.Line != b.Line {
		return false
	}
	if a.Location == nil || b.Location == nil {
		return a.Location == b.Location
	}
	return *a.Location == *b.Location
}

func cloneAddress(a domain.Address) domain.Address {
	if a.Location != nil {
		loc := *a.Location
		a.Location = &loc
	}
	return a
}
