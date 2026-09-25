package domain

import (
	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
)

// Proto returns m as common.v1.Mass.
func (m Mass) Proto() *commonv1.Mass {
	return &commonv1.Mass{Grams: m.Grams, DisplayUnit: m.Unit}
}

// Proto returns p as common.v1.UnitPrice; nil (negotiable) stays nil.
func (p *Price) Proto() *commonv1.UnitPrice {
	if p == nil {
		return nil
	}
	return &commonv1.UnitPrice{
		Amount: &commonv1.Money{AmountMinor: p.AmountMinor, CurrencyCode: p.Currency},
		Unit:   p.Unit,
	}
}

// Proto returns a as common.v1.Address.
func (a Address) Proto() *commonv1.Address {
	pb := &commonv1.Address{Region: a.Region, District: a.District, AddressLine: a.Line}
	if a.Location != nil {
		pb.Location = &commonv1.GeoPoint{Latitude: a.Location.Latitude, Longitude: a.Location.Longitude}
	}
	return pb
}

// MediaRefs returns the media of a listing as common.v1.MediaRef, in display
// order.
//
// TODO(media): only media_id is known; type, status, URLs and thumbnails
// come from media-pipeline, which is not built yet. Until then clients see
// bare ids (and profile-social's cover picker, which looks for an image,
// falls back to the first media).
func MediaRefs(ids []uuid.UUID) []*commonv1.MediaRef {
	if len(ids) == 0 {
		return nil
	}
	out := make([]*commonv1.MediaRef, len(ids))
	for i, id := range ids {
		out[i] = &commonv1.MediaRef{MediaId: id.String()}
	}
	return out
}
