package grpcserver

import (
	"context"
	"strconv"

	"github.com/google/uuid"
	catalogv1 "github.com/tbilol/dxrl/gen/go/catalog/v1"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tbilol/dxrl/services/catalog/internal/apperr"
	"github.com/tbilol/dxrl/services/catalog/internal/auth"
	"github.com/tbilol/dxrl/services/catalog/internal/domain"
)

// Service is the business API the handler exposes (*service.Service).
type Service interface {
	CreateListing(ctx context.Context, p auth.Principal, requestID string, kind commonv1.ListingKind,
		f domain.ListingFields) (domain.Listing, error)
	GetListing(ctx context.Context, p auth.Principal, listingID string) (domain.Listing, error)
	UpdateListing(ctx context.Context, p auth.Principal, listingID string, f domain.ListingFields, mask []string,
		expectedVersion int64) (domain.Listing, error)
	DeleteListing(ctx context.Context, p auth.Principal, listingID string, expectedVersion int64) error
	ListListings(ctx context.Context, p auth.Principal, f domain.ListingFilter, page domain.PageRequest) (domain.ListingPage, error)
	SearchListings(ctx context.Context, p auth.Principal, query string, f domain.ListingFilter,
		sortBy catalogv1.ListingSortField, dir commonv1.SortDirection, page domain.PageRequest) (domain.ListingPage, error)
	ListProductTypes(ctx context.Context, p auth.Principal, category commonv1.ProductCategory) ([]domain.ProductType, error)
}

// handler maps agrogram.catalog.v1.CatalogService onto Service. Requests
// arrive validated and with a Principal in the context (see the
// interceptors).
type handler struct {
	catalogv1.UnimplementedCatalogServiceServer
	svc Service
}

func principal(ctx context.Context) auth.Principal {
	p, _ := auth.FromContext(ctx)
	return p
}

func (h *handler) CreateListing(ctx context.Context, req *catalogv1.CreateListingRequest) (*catalogv1.CreateListingResponse, error) {
	f, err := fields(req.GetListing())
	if err != nil {
		return nil, err
	}
	l, err := h.svc.CreateListing(ctx, principal(ctx), req.GetRequestId(), req.GetListing().GetKind(), f)
	if err != nil {
		return nil, err
	}
	return &catalogv1.CreateListingResponse{Listing: listingProto(l)}, nil
}

func (h *handler) GetListing(ctx context.Context, req *catalogv1.GetListingRequest) (*catalogv1.GetListingResponse, error) {
	l, err := h.svc.GetListing(ctx, principal(ctx), req.GetListingId())
	if err != nil {
		return nil, err
	}
	return &catalogv1.GetListingResponse{Listing: listingProto(l)}, nil
}

func (h *handler) UpdateListing(ctx context.Context, req *catalogv1.UpdateListingRequest) (*catalogv1.UpdateListingResponse, error) {
	f, err := fields(req.GetListing())
	if err != nil {
		return nil, err
	}
	l, err := h.svc.UpdateListing(ctx, principal(ctx), req.GetListing().GetId(), f, req.GetUpdateMask().GetPaths(),
		req.GetExpectedVersion())
	if err != nil {
		return nil, err
	}
	return &catalogv1.UpdateListingResponse{Listing: listingProto(l)}, nil
}

func (h *handler) DeleteListing(ctx context.Context, req *catalogv1.DeleteListingRequest) (*catalogv1.DeleteListingResponse, error) {
	if err := h.svc.DeleteListing(ctx, principal(ctx), req.GetListingId(), req.GetExpectedVersion()); err != nil {
		return nil, err
	}
	return &catalogv1.DeleteListingResponse{}, nil
}

func (h *handler) ListListings(ctx context.Context, req *catalogv1.ListListingsRequest) (*catalogv1.ListListingsResponse, error) {
	page, err := h.svc.ListListings(ctx, principal(ctx), filter(req.GetFilter()), pageRequest(req.GetPage()))
	if err != nil {
		return nil, err
	}
	return &catalogv1.ListListingsResponse{Listings: listingsProto(page.Listings), Page: pageResponse(page)}, nil
}

func (h *handler) SearchListings(ctx context.Context, req *catalogv1.SearchListingsRequest) (*catalogv1.SearchListingsResponse, error) {
	page, err := h.svc.SearchListings(ctx, principal(ctx), req.GetQuery(), filter(req.GetFilter()), req.GetSortBy(),
		req.GetSortDirection(), pageRequest(req.GetPage()))
	if err != nil {
		return nil, err
	}
	return &catalogv1.SearchListingsResponse{Listings: listingsProto(page.Listings), Page: pageResponse(page)}, nil
}

func (h *handler) ListProductTypes(ctx context.Context, req *catalogv1.ListProductTypesRequest) (*catalogv1.ListProductTypesResponse, error) {
	types, err := h.svc.ListProductTypes(ctx, principal(ctx), req.GetCategory())
	if err != nil {
		return nil, err
	}
	resp := &catalogv1.ListProductTypesResponse{ProductTypes: make([]*catalogv1.ProductTypeInfo, len(types))}
	for i, t := range types {
		resp.ProductTypes[i] = &catalogv1.ProductTypeInfo{
			Type:      t.Type,
			Key:       t.Key,
			Name:      &commonv1.LocalizedText{Uz: t.NameUZ, Ru: t.NameRU},
			Category:  t.Category,
			SortOrder: t.SortOrder,
			IsRetired: t.Retired,
		}
	}
	return resp, nil
}

// fields reads the owner's fields of a request listing. Output-only fields
// (id, owner, remaining quantity, status, counters, timestamps) are ignored;
// of each media only media_id is read (MediaRef's other fields are output
// only, so a client-declared media type is never trusted).
func fields(pb *catalogv1.Listing) (domain.ListingFields, error) {
	f := domain.ListingFields{
		Product: pb.GetProduct(),
		Phone:   pb.GetPhone(),
		Notes:   pb.GetNotes(),
	}
	if q := pb.GetQuantity(); q != nil {
		f.Quantity = domain.Mass{Grams: q.GetGrams(), Unit: q.GetDisplayUnit()}
	}
	f.Price = price(pb.GetPrice())
	if a := pb.GetAddress(); a != nil {
		f.Address = domain.Address{Region: a.GetRegion(), District: a.GetDistrict(), Line: a.GetAddressLine()}
		if loc := a.GetLocation(); loc != nil {
			f.Address.Location = &domain.GeoPoint{Latitude: loc.GetLatitude(), Longitude: loc.GetLongitude()}
		}
	}
	for i, m := range pb.GetMedia() {
		id, err := uuid.Parse(m.GetMediaId())
		if err != nil { // protovalidate checked it; this guards direct callers
			return f, apperr.Validation(&commonv1.FieldViolation{
				Field: "listing.media[" + strconv.Itoa(i) + "].media_id", Description: "value must be a valid UUID",
				Rule: "string.uuid",
			})
		}
		f.MediaIDs = append(f.MediaIDs, id)
	}
	return f, nil
}

func price(pb *commonv1.UnitPrice) *domain.Price {
	if pb == nil {
		return nil
	}
	return &domain.Price{
		AmountMinor: pb.GetAmount().GetAmountMinor(),
		Currency:    pb.GetAmount().GetCurrencyCode(),
		Unit:        pb.GetUnit(),
	}
}

func filter(pb *catalogv1.ListingFilter) domain.ListingFilter {
	f := domain.ListingFilter{
		Kind:     pb.GetKind(),
		Products: pb.GetProductTypes(),
		Regions:  pb.GetRegions(),
		District: pb.GetDistrict(),
		OwnerID:  pb.GetOwnerId(),
		Statuses: pb.GetStatuses(),
		MinPrice: price(pb.GetMinPrice()),
		MaxPrice: price(pb.GetMaxPrice()),
	}
	if q := pb.GetMinQuantity(); q != nil {
		f.MinQuantity = &domain.Mass{Grams: q.GetGrams(), Unit: q.GetDisplayUnit()}
	}
	return f
}

func pageRequest(pb *commonv1.PageRequest) domain.PageRequest {
	return domain.PageRequest{Size: pb.GetPageSize(), Token: pb.GetPageToken()}
}

// pageResponse leaves total_size unset: counting every match is not cheap
// for board-wide queries (pagination.proto: "unset when unknown").
func pageResponse(page domain.ListingPage) *commonv1.PageResponse {
	return &commonv1.PageResponse{NextPageToken: page.NextPageToken}
}

func listingsProto(ls []domain.Listing) []*catalogv1.Listing {
	out := make([]*catalogv1.Listing, len(ls))
	for i, l := range ls {
		out[i] = listingProto(l)
	}
	return out
}

func listingProto(l domain.Listing) *catalogv1.Listing {
	return &catalogv1.Listing{
		Id:                l.ID.String(),
		Kind:              l.Kind,
		OwnerId:           l.OwnerID.String(),
		Product:           l.Product,
		Quantity:          l.Quantity.Proto(),
		RemainingQuantity: l.Remaining().Proto(), // quantity minus the escrow commitments from the transaction stream
		Price:             l.Price.Proto(),
		Phone:             l.Phone,
		Address:           l.Address.Proto(),
		Notes:             l.Notes,
		Media:             domain.MediaRefs(l.MediaIDs), // TODO(media): ids only
		Status:            l.Status,
		// TODO(edge): only user_id. The owner card (display name, avatar,
		// rating, Premium and verified-farmer badges) is profile-social's
		// data; by decision the API gateway / client composes it, and
		// catalog neither calls profile-social nor consumes the profile
		// stream (catalog.proto's "projected from the profile stream" is
		// not implemented).
		Owner:      &catalogv1.ListingOwner{UserId: l.OwnerID.String()},
		ViewsCount: l.ViewsCount,
		Version:    l.Version,
		CreatedAt:  timestamppb.New(l.CreatedAt),
		UpdatedAt:  timestamppb.New(l.UpdatedAt),
	}
}
