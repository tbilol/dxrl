package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"

	"github.com/tbilol/dxrl/services/catalog/internal/apperr"
	"github.com/tbilol/dxrl/services/catalog/internal/auth"
	"github.com/tbilol/dxrl/services/catalog/internal/domain"
	"github.com/tbilol/dxrl/services/catalog/internal/events"
)

// CreateListing posts a listing on the board kind names, owned by the
// caller, and publishes ListingCreated through the outbox in the same
// transaction.
//
// Rules, in order: a signed-in caller; offers need ROLE_FARMER
// (FARMER_VERIFICATION_REQUIRED), buy-requests ROLE_RESELLER
// (PERMISSION_DENIED); the fields are valid; the per-user creation rate
// limit (RATE_LIMITED, fails open without Redis); then, under a per-owner
// advisory lock: a retry with the same request id returns the listing it
// created, the product must be in the reference data and not retired, and
// the owner's ACTIVE listings of kind must be below the plan's quota
// (PLAN_LIMIT_REACHED). Only ACTIVE listings count: CLOSED, SOLD_OUT,
// BLOCKED and DELETED ones are off the board, so they do not hold a slot.
func (s *Service) CreateListing(ctx context.Context, p auth.Principal, requestID string, kind commonv1.ListingKind,
	f domain.ListingFields,
) (domain.Listing, error) {
	owner, err := auth.RequireUser(p)
	if err != nil {
		return domain.Listing{}, err
	}
	reqID, err := parseID("request_id", requestID)
	if err != nil {
		return domain.Listing{}, err
	}
	if err := checkRole(p, kind); err != nil {
		return domain.Listing{}, err
	}
	if f, err = normalize(f, domain.AllFields); err != nil {
		return domain.Listing{}, err
	}
	if err := checkMedia(p, f.MediaIDs); err != nil {
		return domain.Listing{}, err
	}
	if err := s.checkRateLimit(ctx, owner); err != nil {
		return domain.Listing{}, err
	}
	limit, plan := s.quotas.Limit(kind, p.Premium)
	tr := requestTrigger(p)
	var (
		l       domain.Listing
		created bool
	)
	err = s.store.InTx(ctx, func(q Queries) error {
		if err := q.LockOwner(ctx, owner); err != nil {
			return err
		}
		prev, err := q.ListingByRequest(ctx, owner, reqID)
		switch {
		case err == nil: // a retry: the original result
			l = prev
			return nil
		case !errors.Is(err, domain.ErrNotFound):
			return err
		}
		pt, err := productType(ctx, q, f.Product, true)
		if err != nil {
			return err
		}
		n, err := q.CountActiveListings(ctx, owner, kind)
		if err != nil {
			return err
		}
		if n >= limit {
			return apperr.New(commonv1.ErrorReason_ERROR_REASON_PLAN_LIMIT_REACHED,
				fmt.Sprintf("the %s plan allows %d active %s listings", plan, limit, kindName(kind)),
				"plan", plan, "listing_kind", kindName(kind), "limit", strconv.Itoa(limit))
		}
		id, err := newID()
		if err != nil {
			return err
		}
		now := s.clock()
		l = domain.Listing{
			ID:              id,
			Kind:            kind,
			OwnerID:         owner,
			ListingFields:   f,
			RemainingGrams:  f.Quantity.Grams, // a new listing has no escrow transactions: all of it remains
			Status:          domain.StatusActive,
			Version:         1,
			CreateRequestID: reqID,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		doc := searchDocument(pt, f)
		if err := q.InsertListing(ctx, l, doc); err != nil {
			return err
		}
		msg, err := events.NewListingEvent(events.ListingChange{
			Listing:       l,
			Event:         &eventsv1.ListingCreated{},
			OccurredAt:    now,
			CorrelationID: tr.correlationID,
			Traceparent:   tr.traceparent,
			Tracestate:    tr.tracestate,
			Actor:         tr.actor,
		})
		if err != nil {
			return err
		}
		created = true
		return q.EnqueueOutbox(ctx, msg)
	})
	if err != nil {
		return domain.Listing{}, fmt.Errorf("create listing: %w", err)
	}
	if created {
		s.log.InfoContext(ctx, "listing created", "listing_id", l.ID, "owner_id", owner, "kind", kindName(kind),
			"product", l.Product.String(), "plan", plan, "request_id", p.RequestID)
	}
	return l, nil
}

// GetListing returns a listing. ACTIVE listings are visible to everyone
// (anonymous callers too); other statuses (CLOSED, SOLD_OUT, BLOCKED,
// DELETED) only to the owner, the parties of escrow transactions on the
// listing (catalog.proto; catalog knows them from order-escrow's transaction
// stream), staff (ROLE_ADMIN) and order-escrow (x-agrogram-caller), which
// snapshots listings into transactions. Everyone else gets NOT_FOUND. An
// ACTIVE listing opened by someone other than its owner, staff or a service
// counts as a view, in the background.
func (s *Service) GetListing(ctx context.Context, p auth.Principal, listingID string) (domain.Listing, error) {
	id, err := parseID("listing_id", listingID)
	if err != nil {
		return domain.Listing{}, err
	}
	var (
		l       domain.Listing
		visible bool
	)
	err = s.store.Run(ctx, func(q Queries) error {
		if l, err = q.GetListing(ctx, id); err != nil {
			return err
		}
		visible, err = canRead(ctx, q, p, l)
		return err
	})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return domain.Listing{}, listingNotFound(id)
	case err != nil:
		return domain.Listing{}, fmt.Errorf("get listing: %w", err)
	}
	if !visible {
		return domain.Listing{}, listingNotFound(id)
	}
	s.countView(p, l)
	return l, nil
}

// canSee reports whether p may read l by the rules that need no lookup: l is
// ACTIVE, or p is its owner, staff or order-escrow. canRead adds the parties
// of escrow transactions on l.
func canSee(p auth.Principal, l domain.Listing) bool {
	return l.Status == domain.StatusActive ||
		(p.Authenticated() && p.UserID == l.OwnerID) ||
		p.Staff() ||
		p.Caller == CallerOrderEscrow
}

// canRead reports whether p may read l: canSee, or p is signed in and the
// farmer or the reseller of an escrow transaction on l, whatever its status
// (catalog.proto: "Non-ACTIVE listings are visible to their owner, to the
// parties of transactions on them, and to staff"). The party lookup runs only
// when canSee says no.
func canRead(ctx context.Context, q Queries, p auth.Principal, l domain.Listing) (bool, error) {
	if canSee(p, l) {
		return true, nil
	}
	if !p.Authenticated() {
		return false, nil
	}
	return q.IsTransactionParty(ctx, l.ID, p.UserID)
}

// countView records a view of l unless the owner, staff or another service
// opened it. It never blocks or fails the read.
func (s *Service) countView(p auth.Principal, l domain.Listing) {
	if s.views == nil || l.Status != domain.StatusActive || p.Internal() || p.Staff() ||
		(p.Authenticated() && p.UserID == l.OwnerID) {
		return
	}
	s.views.Record(l.ID, p.UserID)
}

// UpdateListing changes the update_mask fields of the caller's own listing.
// Only ACTIVE listings can be edited (LISTING_NOT_ACTIVE otherwise: a closed,
// sold-out, blocked or deleted listing is off the board and has no way back
// yet); kind never changes. expectedVersion 0 skips the optimistic
// concurrency check; a stale one is ABORTED / CONFLICT with the current
// version. Values equal to the current ones change nothing (no version bump).
// A new product must not be retired; an unchanged retired product stays.
//
// The quantity cannot drop below what escrow transactions have committed of
// the listing (INSUFFICIENT_QUANTITY, with requested_grams and
// committed_grams). The remaining quantity becomes the quantity minus that,
// and a listing with nothing left becomes SOLD_OUT, the rule the
// transaction-stream consumer applies (ApplyTransactionEvent). The committed
// quantity is read under the listing's row lock, which the consumer takes
// too, so the two cannot interleave.
func (s *Service) UpdateListing(ctx context.Context, p auth.Principal, listingID string, f domain.ListingFields,
	mask []string, expectedVersion int64,
) (domain.Listing, error) {
	caller, err := auth.RequireUser(p)
	if err != nil {
		return domain.Listing{}, err
	}
	id, err := parseID("listing.id", listingID)
	if err != nil {
		return domain.Listing{}, err
	}
	mask = slices.Compact(slices.Sorted(slices.Values(mask)))
	if len(mask) == 0 {
		return domain.Listing{}, invalid("update_mask", "update_mask must name at least one field",
			"update_listing.update_mask_not_empty")
	}
	for _, path := range mask {
		if !slices.Contains(domain.AllFields, path) {
			return domain.Listing{}, invalid("update_mask", "update_mask may name only "+
				strings.Join(domain.AllFields, ", "), "field_mask.in")
		}
	}
	if f, err = normalize(f, mask); err != nil {
		return domain.Listing{}, err
	}
	if slices.Contains(mask, domain.FieldMedia) {
		if err := checkMedia(p, f.MediaIDs); err != nil {
			return domain.Listing{}, err
		}
	}
	var (
		l       domain.Listing
		changed bool
	)
	err = s.store.InTx(ctx, func(q Queries) error {
		cur, err := lockOwned(ctx, q, p, caller, id, "update")
		if err != nil {
			return err
		}
		if cur.Status != domain.StatusActive {
			return notActive(cur)
		}
		if expectedVersion > 0 && expectedVersion != cur.Version {
			return conflict(cur)
		}
		next := cur
		next.MediaIDs = slices.Clone(cur.MediaIDs)
		if !applyMask(&next.ListingFields, f, mask) {
			l = cur
			return nil
		}
		pt, err := productType(ctx, q, next.Product, next.Product != cur.Product)
		if err != nil {
			return err
		}
		committed, err := q.CommittedGrams(ctx, id)
		if err != nil {
			return err
		}
		if next.Quantity.Grams < committed {
			return insufficient(cur, next.Quantity.Grams, committed)
		}
		next = withCommitted(next, committed)
		next.Version++
		next.UpdatedAt = s.clock()
		doc := searchDocument(pt, next.ListingFields)
		if err := q.SaveListing(ctx, next, &doc); err != nil {
			return err
		}
		// TODO(events): publish ListingUpdated here (same transaction, through
		// the outbox) once listing.proto defines it; consumers keep the
		// ListingCreated snapshot until then.
		l, changed = next, true
		return nil
	})
	if err != nil {
		return domain.Listing{}, fmt.Errorf("update listing: %w", err)
	}
	if changed {
		s.log.InfoContext(ctx, "listing updated", "listing_id", id, "fields", mask, "version", l.Version,
			"status", statusName(l.Status), "request_id", p.RequestID)
	}
	return l, nil
}

// DeleteListing soft-deletes the caller's own listing: status DELETED,
// version + 1. Transactions already started on it continue (the row stays).
// Deleting a deleted listing is a no-op (so a retry succeeds even though the
// first call bumped the version); otherwise expectedVersion 0 skips the
// check and a stale one is ABORTED / CONFLICT. Listings in any other status
// can be deleted.
//
// TODO(proto): catalog.proto has no CloseListing RPC (status CLOSED, "closed
// by the owner": off the board, transactions continue), and update_mask
// cannot name `status`. Closing belongs next to this method once the
// contract has it.
func (s *Service) DeleteListing(ctx context.Context, p auth.Principal, listingID string, expectedVersion int64) error {
	caller, err := auth.RequireUser(p)
	if err != nil {
		return err
	}
	id, err := parseID("listing_id", listingID)
	if err != nil {
		return err
	}
	deleted := false
	err = s.store.InTx(ctx, func(q Queries) error {
		cur, err := lockOwned(ctx, q, p, caller, id, "delete")
		if err != nil {
			return err
		}
		if cur.Status == domain.StatusDeleted {
			return nil
		}
		if expectedVersion > 0 && expectedVersion != cur.Version {
			return conflict(cur)
		}
		now := s.clock()
		cur.Status = domain.StatusDeleted
		cur.DeletedAt = &now
		cur.UpdatedAt = now
		cur.Version++
		if err := q.SaveListing(ctx, cur, nil); err != nil {
			return err
		}
		// TODO(events): publish ListingDeleted here once listing.proto
		// defines it (order-escrow and profile-social rely on the stream to
		// learn that a listing left the board); nothing is published today.
		deleted = true
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete listing: %w", err)
	}
	if deleted {
		s.log.InfoContext(ctx, "listing deleted", "listing_id", id, "owner_id", caller, "request_id", p.RequestID)
	}
	return nil
}

// lockOwned locks listing id for a change by its owner. A listing of
// someone else is PERMISSION_DENIED when the caller may read it (canRead,
// transaction parties included) and NOT_FOUND otherwise, so hidden listings
// do not leak.
func lockOwned(ctx context.Context, q Queries, p auth.Principal, caller, id uuid.UUID, action string) (domain.Listing, error) {
	cur, err := q.LockListing(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return cur, listingNotFound(id)
	}
	if err != nil {
		return cur, err
	}
	if cur.OwnerID != caller {
		visible, err := canRead(ctx, q, p, cur)
		if err != nil {
			return cur, err
		}
		if visible {
			return cur, apperr.New(commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED,
				"only the owner may "+action+" a listing", "listing_id", id.String())
		}
		return cur, listingNotFound(id)
	}
	return cur, nil
}

func notActive(l domain.Listing) error {
	return apperr.New(commonv1.ErrorReason_ERROR_REASON_LISTING_NOT_ACTIVE,
		"only ACTIVE listings can be edited", "listing_id", l.ID.String(), "status", statusName(l.Status))
}

// insufficient is UpdateListing's INSUFFICIENT_QUANTITY: a quantity below
// what escrow transactions have committed of l (metadata like order-escrow's
// error of the same reason).
func insufficient(l domain.Listing, requested, committed int64) error {
	return apperr.New(commonv1.ErrorReason_ERROR_REASON_INSUFFICIENT_QUANTITY,
		"the quantity cannot be less than what escrow transactions have already committed",
		"listing_id", l.ID.String(), "requested_grams", strconv.FormatInt(requested, 10),
		"committed_grams", strconv.FormatInt(committed, 10))
}

func conflict(l domain.Listing) error {
	return apperr.New(commonv1.ErrorReason_ERROR_REASON_CONFLICT,
		"the listing changed since it was read; reload it and retry",
		"current_version", strconv.FormatInt(l.Version, 10))
}

// checkRole enforces who may post on which board (market.proto, auth.proto):
// offers need ROLE_FARMER (FARMER_VERIFICATION_REQUIRED), buy-requests
// ROLE_RESELLER, which every active account holds (PERMISSION_DENIED, the
// closest reason error.proto has). Roles are the token-time snapshot from
// x-agrogram-roles.
func checkRole(p auth.Principal, kind commonv1.ListingKind) error {
	switch kind {
	case domain.KindOffer:
		if !p.HasRole(domain.RoleFarmer) {
			return apperr.New(commonv1.ErrorReason_ERROR_REASON_FARMER_VERIFICATION_REQUIRED,
				"posting sell offers requires an e-ijara-verified farmer (ROLE_FARMER)",
				"listing_kind", kindName(kind), "required_role", "FARMER")
		}
	case domain.KindBuyRequest:
		if !p.HasRole(domain.RoleReseller) {
			return apperr.New(commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED,
				"posting buy-requests requires ROLE_RESELLER",
				"listing_kind", kindName(kind), "required_role", "RESELLER")
		}
	default:
		return invalid("listing.kind", "kind is required", "create_listing.kind_required")
	}
	return nil
}

// checkMedia applies the media rules the contract states.
//
// STUB: the Premium rule for listing video (catalog.proto: "a video needs
// Premium (PREMIUM_REQUIRED)") is not enforced. A request carries only media
// ids (MediaRef.type is output only, so a client-declared type is never
// trusted), and nothing can tell an image id from a video id yet.
// TODO(media): look every id up in media-pipeline (exists, uploaded by the
// caller, purpose MEDIA_PURPOSE_LISTING_IMAGE / _VIDEO, not FAILED) and
// reject a video without p.Premium with PREMIUM_REQUIRED, at most one video
// per listing.
func checkMedia(_ auth.Principal, _ []uuid.UUID) error {
	return nil
}

// productType loads the reference data of t. A product unknown to the
// reference data, or retired when forNew is set, is VALIDATION_FAILED.
func productType(ctx context.Context, q Queries, t commonv1.ProductType, forNew bool) (domain.ProductType, error) {
	pt, err := q.GetProductType(ctx, t)
	if errors.Is(err, domain.ErrNotFound) {
		return pt, invalid("listing.product", "product is not in the product reference data", "product.known")
	}
	if err != nil {
		return pt, err
	}
	if forNew && pt.Retired {
		return pt, invalid("listing.product", "product is retired and cannot be used by new listings",
			"product.not_retired")
	}
	return pt, nil
}

// kindName is the ListingKind name without its prefix ("OFFER",
// "BUY_REQUEST"), as used in error metadata.
func kindName(k commonv1.ListingKind) string {
	return strings.TrimPrefix(k.String(), "LISTING_KIND_")
}

func statusName(s eventsv1.ListingStatus) string {
	return strings.TrimPrefix(s.String(), "LISTING_STATUS_")
}
