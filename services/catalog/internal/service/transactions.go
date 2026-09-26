package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/tbilol/dxrl/services/catalog/internal/domain"
)

// ApplyTransactionEvent applies one event of order-escrow's transaction
// stream (the caller is events.TransactionHandler) to catalog's projection of
// the escrow transactions on listings, following the consumer rules of
// metadata.proto:
//
//   - de-duplication: the event id goes into the inbox (processed_events) in
//     the same transaction; a known id is a Duplicate and changes nothing;
//   - versioning: a transaction's row takes the snapshot's state and version
//     only when that version is newer, so an older event arriving after a
//     newer one is Stale;
//   - every event type, including a oneof case newer than this code, takes
//     this one path: the snapshot carries everything catalog needs.
//
// The first event of a transaction catalog sees, whatever its type (normally
// TransactionCreated), inserts the transaction's row and so commits its
// quantity against the listing: the remaining quantity becomes the listing's
// quantity minus the quantities of all its transactions, never below 0, and
// an ACTIVE listing with nothing left becomes SOLD_OUT. Committing on first
// sight keeps the remaining quantity equal to what order-escrow will still
// sell: its oversell guard counts every transaction from its creation,
// unpaid and completed alike, so committing later (on PaymentHeld or
// PayoutReleased) would advertise quantity order-escrow already refuses with
// INSUFFICIENT_QUANTITY, and listing.proto defines SOLD_OUT as the whole
// quantity being committed to escrow transactions. Any event commits, not only
// TransactionCreated: every event carries the terms, which are fixed at
// creation, so a lost or expired TransactionCreated loses nothing. The
// remaining quantity is recomputed from the sum, never decremented, so a
// replay cannot count a transaction twice. Nothing releases a commitment: the
// contract has no cancel or expiry event (order-escrow's TODO(cancel)), so
// SOLD_OUT is final.
//
// All of it runs in one database transaction under the listing's row lock,
// which serializes it with UpdateListing, DeleteListing and the other
// transactions on the listing. An event that contradicts catalog's data (its
// listing does not exist, has another kind, or is not owned by the
// owner-side party: the farmer of an offer, the reseller of a buy-request) or
// an earlier event of the transaction (other terms), and an update that cannot
// describe a transaction at all, wrap domain.ErrInconsistentEvent; the
// rollback leaves no inbox row, and the consumer dead-letters the record at
// once.
func (s *Service) ApplyTransactionEvent(ctx context.Context, u domain.TransactionUpdate) (domain.ApplyOutcome, error) {
	if err := checkTransactionUpdate(u); err != nil {
		return 0, fmt.Errorf("apply transaction event %s (transaction %s): %w", u.EventID, u.TransactionID, err)
	}
	var (
		outcome   domain.ApplyOutcome
		committed bool
		l         domain.Listing
	)
	err := s.store.InTx(ctx, func(q Queries) error {
		now := s.clock()
		fresh, err := q.MarkProcessed(ctx, domain.ProcessedEvent{
			EventID:          u.EventID,
			Topic:            u.Topic,
			EventType:        u.EventType,
			AggregateID:      u.TransactionID,
			AggregateVersion: u.Version,
			ProcessedAt:      now,
		})
		if err != nil {
			return err
		}
		if !fresh {
			outcome = domain.Duplicate
			return nil
		}
		l, err = q.LockListing(ctx, u.ListingID)
		if errors.Is(err, domain.ErrNotFound) {
			return inconsistent("listing %s does not exist", u.ListingID)
		}
		if err != nil {
			return err
		}
		if err := checkListing(l, u); err != nil {
			return err
		}
		prev, err := q.GetListingTransaction(ctx, u.TransactionID)
		switch {
		case errors.Is(err, domain.ErrNotFound): // the first event seen: the commitment, below
		case err != nil:
			return err
		default:
			outcome, err = updateTransaction(ctx, q, prev, u, now)
			return err
		}
		if err := q.InsertListingTransaction(ctx, domain.ListingTransaction{
			ID:            u.TransactionID,
			ListingID:     u.ListingID,
			FarmerID:      u.FarmerID,
			ResellerID:    u.ResellerID,
			QuantityGrams: u.QuantityGrams,
			State:         u.State,
			Version:       u.Version,
			CreatedAt:     u.CreatedAt.UTC().Truncate(time.Microsecond), // what Postgres stores
			RecordedAt:    now,
			UpdatedAt:     now,
		}); err != nil {
			return err
		}
		outcome, committed = domain.Applied, true
		l, err = s.recomputeRemaining(ctx, q, l, now)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("apply transaction event %s (transaction %s): %w", u.EventID, u.TransactionID, err)
	}
	if committed {
		s.log.InfoContext(ctx, "escrow transaction committed listing quantity", "listing_id", l.ID,
			"transaction_id", u.TransactionID, "grams", u.QuantityGrams, "remaining_grams", l.RemainingGrams,
			"status", statusName(l.Status), "event_type", u.EventType)
	}
	return outcome, nil
}

// updateTransaction applies a later event of a known transaction: the terms
// must be the stored ones; a newer version takes the state (Applied), any
// other is Stale. The listing does not change: the quantity was committed
// when the transaction was first seen.
func updateTransaction(ctx context.Context, q Queries, prev domain.ListingTransaction, u domain.TransactionUpdate,
	now time.Time,
) (domain.ApplyOutcome, error) {
	if prev.ListingID != u.ListingID || prev.FarmerID != u.FarmerID || prev.ResellerID != u.ResellerID ||
		prev.QuantityGrams != u.QuantityGrams {
		return 0, inconsistent("transaction %s changed its terms: listing %s, farmer %s, reseller %s, %d g before; "+
			"listing %s, farmer %s, reseller %s, %d g now", u.TransactionID, prev.ListingID, prev.FarmerID,
			prev.ResellerID, prev.QuantityGrams, u.ListingID, u.FarmerID, u.ResellerID, u.QuantityGrams)
	}
	if u.Version <= prev.Version {
		return domain.Stale, nil
	}
	prev.State, prev.Version, prev.UpdatedAt = u.State, u.Version, now
	updated, err := q.UpdateListingTransaction(ctx, prev)
	if err != nil || !updated {
		return domain.Stale, err
	}
	return domain.Applied, nil
}

// recomputeRemaining brings l's remaining quantity and status up to date
// after a new escrow transaction on it, from the sum of all its transactions
// (see withCommitted). A change bumps the version and updated_at, so
// optimistic concurrency notices it. It returns the listing as stored.
func (s *Service) recomputeRemaining(ctx context.Context, q Queries, l domain.Listing, now time.Time) (domain.Listing, error) {
	committed, err := q.CommittedGrams(ctx, l.ID)
	if err != nil {
		return l, err
	}
	if committed > l.Quantity.Grams {
		// Possible only when the owner lowered the quantity before catalog
		// saw this transaction.
		s.log.WarnContext(ctx, "escrow transactions commit more than the listing's quantity; remaining clamped at 0",
			"listing_id", l.ID, "quantity_grams", l.Quantity.Grams, "committed_grams", committed)
	}
	next := withCommitted(l, committed)
	if next.RemainingGrams == l.RemainingGrams && next.Status == l.Status {
		return l, nil
	}
	next.Version++
	next.UpdatedAt = now
	if err := q.SaveRemaining(ctx, next); err != nil {
		return l, err
	}
	// TODO(events): publish the new remaining quantity and SOLD_OUT here
	// (same transaction, through the outbox) once listing.proto defines a
	// case for it; consumers keep the ListingCreated snapshot until then.
	return next, nil
}

// withCommitted returns l with committed grams of it committed to escrow
// transactions: remaining = quantity - committed, never below 0, and an
// ACTIVE listing with nothing left is SOLD_OUT (listing.proto: "The whole
// quantity is committed to escrow transactions"). Other statuses (CLOSED,
// SOLD_OUT, BLOCKED, DELETED) stay: they are off the board already.
func withCommitted(l domain.Listing, committed int64) domain.Listing {
	l.RemainingGrams = max(l.Quantity.Grams-committed, 0)
	if l.Status == domain.StatusActive && l.RemainingGrams == 0 {
		l.Status = domain.StatusSoldOut
	}
	return l
}

// checkTransactionUpdate refuses an update that cannot describe an escrow
// transaction. The decoder guarantees all of it; this guards direct callers.
func checkTransactionUpdate(u domain.TransactionUpdate) error {
	switch {
	case u.EventID == uuid.Nil || u.TransactionID == uuid.Nil || u.ListingID == uuid.Nil ||
		u.FarmerID == uuid.Nil || u.ResellerID == uuid.Nil:
		return inconsistent("the event id, the transaction id, the listing id and both parties are required")
	case u.FarmerID == u.ResellerID:
		return inconsistent("the farmer and the reseller must differ")
	case u.QuantityGrams <= 0:
		return inconsistent("the quantity must be positive, got %d g", u.QuantityGrams)
	case u.Version < 1:
		return inconsistent("the version must be at least 1, got %d", u.Version)
	case u.State == 0:
		return inconsistent("the state is required")
	}
	return nil
}

// checkListing verifies that the transaction u describes fits the listing l
// it names: the same kind, and l's owner is the owner-side party, the farmer
// of an offer or the reseller of a buy-request (order-escrow lets only the
// owner's counterparty start a transaction).
func checkListing(l domain.Listing, u domain.TransactionUpdate) error {
	if l.Kind != u.ListingKind {
		return inconsistent("listing %s is %s, the transaction says %s", l.ID, kindName(l.Kind), kindName(u.ListingKind))
	}
	owner, side := u.FarmerID, "farmer"
	if l.Kind == domain.KindBuyRequest {
		owner, side = u.ResellerID, "reseller"
	}
	if l.OwnerID != owner {
		return inconsistent("%s %s is owned by %s, not by the transaction's %s %s", kindName(l.Kind), l.ID, l.OwnerID,
			side, owner)
	}
	return nil
}

// inconsistent wraps domain.ErrInconsistentEvent with what is wrong.
func inconsistent(format string, args ...any) error {
	return fmt.Errorf("%w: %s", domain.ErrInconsistentEvent, fmt.Sprintf(format, args...))
}
