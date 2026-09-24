package service

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	profilesocialv1 "github.com/tbilol/dxrl/gen/go/profilesocial/v1"

	"github.com/tbilol/dxrl/services/profile-social/internal/apperr"
	"github.com/tbilol/dxrl/services/profile-social/internal/auth"
	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
)

// ListUserListings lists userID's listings on both boards (or the one kind
// names), newest first, by asking catalog (ListListings with an owner
// filter); profile-social owns no listing data.
//
// Other users see ACTIVE listings only (asking for another status is
// PERMISSION_DENIED) and only of a user whose profile is public; the owner
// may ask for any status. When catalog is not configured or unreachable the
// answer is an empty page and a warning is logged.
//
// TODO(catalog): catalog is not built yet, so this degrades to empty pages.
// The proto's service comment has profile-social consume the listing stream
// (agrogram.catalog.listing.v1) for this RPC instead; if the live call ever
// becomes a latency or availability problem, project ListingEvent snapshots
// into a local listing-card table with the account-stream consumer's
// machinery (inbox, version guard) and serve from it.
func (s *Service) ListUserListings(ctx context.Context, p auth.Principal, userID string, kind commonv1.ListingKind,
	statuses []eventsv1.ListingStatus, page domain.PageRequest,
) (domain.ListingPage, error) {
	owner, err := parseID("user_id", userID)
	if err != nil {
		return domain.ListingPage{}, err
	}
	isOwner := p.Authenticated() && p.UserID == owner
	if !isOwner {
		for _, st := range statuses {
			if st != domain.ListingActive {
				return domain.ListingPage{}, apperr.New(commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED,
					"only the owner may list listings that are not ACTIVE", "status", st.String())
			}
		}
		var prof domain.Profile
		err := s.store.Run(ctx, func(q Queries) error {
			prof, err = q.GetProfile(ctx, owner, uuid.Nil)
			return err
		})
		switch {
		case errors.Is(err, domain.ErrNotFound) || (err == nil && !prof.Public()):
			return domain.ListingPage{}, notFound("profile", owner)
		case err != nil:
			return domain.ListingPage{}, fmt.Errorf("list user listings: %w", err)
		}
	}
	if s.listings == nil {
		// STUB: no catalog address (catalog is not built yet): an empty page.
		s.log.WarnContext(ctx, "catalog is not configured (PROFILE_SOCIAL_CATALOG_ADDR); answering an empty page",
			"owner_id", owner)
		return domain.ListingPage{Listings: []*profilesocialv1.ListingCard{}}, nil
	}
	out, err := s.listings.ListUserListings(ctx, p, domain.ListingQuery{
		OwnerID: owner, Kind: kind, Statuses: statuses, Page: page,
	})
	if errors.Is(err, domain.ErrDependencyUnavailable) {
		// STUB: catalog unreachable (not deployed yet, or down): an empty page.
		s.log.WarnContext(ctx, "catalog unavailable; answering an empty page", "owner_id", owner, "error", err)
		return domain.ListingPage{Listings: []*profilesocialv1.ListingCard{}}, nil
	}
	if err != nil {
		return domain.ListingPage{}, err
	}
	if !isOwner { // defence in depth: never show other users non-ACTIVE listings
		out.Listings = slices.DeleteFunc(out.Listings, func(c *profilesocialv1.ListingCard) bool {
			return c.GetStatus() != domain.ListingActive
		})
	}
	return out, nil
}
