package service_test

import (
	"errors"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tbilol/dxrl/services/catalog/internal/apperr"
	"github.com/tbilol/dxrl/services/catalog/internal/auth"
	"github.com/tbilol/dxrl/services/catalog/internal/domain"
	"github.com/tbilol/dxrl/services/catalog/internal/events"
)

// lifecycle is the events of one escrow transaction in the order order-escrow
// publishes them (transaction.proto); the version after event i is i + 1.
var lifecycle = []struct {
	name  string
	state eventsv1.TransactionState
}{
	{"TransactionCreated", domain.StateAwaitingPayment},
	{"PaymentHeld", domain.StatePaymentHeld},
	{"ReceiptSubmitted", domain.StateReceiptSubmitted},
	{"ProductHandover", domain.StateHandedOver},
	{"ReceiptConfirmed", domain.StateReceived},
	{"PayoutReleased", domain.StateCompleted},
	{"BonusAwarded", domain.StateCompleted},
}

// deal is an escrow transaction order-escrow started on a listing: the
// owner's counterparty buys from an offer or sells to a buy-request.
type deal struct {
	id               uuid.UUID
	listing          domain.Listing
	farmer, reseller uuid.UUID
	grams            int64
}

func newDeal(l domain.Listing, counterparty uuid.UUID, grams int64) deal {
	d := deal{id: uuid.Must(uuid.NewV7()), listing: l, grams: grams}
	if l.Kind == domain.KindBuyRequest {
		d.farmer, d.reseller = counterparty, l.OwnerID
	} else {
		d.farmer, d.reseller = l.OwnerID, counterparty
	}
	return d
}

// event is the deal's event step (see lifecycle) as the decoder hands it
// over: version step + 1, a fresh event id.
func (d deal) event(step int) domain.TransactionUpdate {
	return domain.TransactionUpdate{
		EventID:       uuid.Must(uuid.NewV7()),
		EventType:     "agrogram.events.v1." + lifecycle[step].name,
		Topic:         events.TopicTransaction,
		OccurredAt:    time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC),
		Version:       int64(step + 1),
		TransactionID: d.id,
		ListingID:     d.listing.ID,
		ListingKind:   d.listing.Kind,
		FarmerID:      d.farmer,
		ResellerID:    d.reseller,
		QuantityGrams: d.grams,
		State:         lifecycle[step].state,
		CreatedAt:     time.Date(2026, 9, 25, 11, 0, 0, 123_456_789, time.UTC),
	}
}

func (e env) apply(t *testing.T, u domain.TransactionUpdate, want domain.ApplyOutcome) {
	t.Helper()
	got, err := e.svc.ApplyTransactionEvent(ctx, u)
	if err != nil {
		t.Fatalf("ApplyTransactionEvent(%s v%d): %v", u.EventType, u.Version, err)
	}
	if got != want {
		t.Fatalf("ApplyTransactionEvent(%s v%d) = %s, want %s", u.EventType, u.Version, got, want)
	}
}

// listing is the stored listing id.
func (e env) listing(id uuid.UUID) domain.Listing { return e.store.state().listings[id] }

func wantListing(t *testing.T, l domain.Listing, remaining int64, status eventsv1.ListingStatus, version int64) {
	t.Helper()
	if l.RemainingGrams != remaining || l.Status != status || l.Version != version {
		t.Fatalf("listing = %d g remaining, %s, version %d; want %d g, %s, version %d",
			l.RemainingGrams, l.Status, l.Version, remaining, status, version)
	}
}

// TransactionCreated commits the quantity once; the rest of the life cycle
// only moves the transaction's state, and neither a redelivery nor an older
// event commits anything again.
func TestTransactionCreatedCommitsQuantity(t *testing.T) {
	e := newEnv()
	owner, buyer := farmer(), reseller()
	l := e.create(t, owner, domain.KindOffer) // 20 t
	d := newDeal(l, buyer.UserID, 5_000_000)

	created := d.event(0)
	e.apply(t, created, domain.Applied)
	got := e.listing(l.ID)
	wantListing(t, got, 15_000_000, domain.StatusActive, 2)
	if !got.UpdatedAt.After(l.UpdatedAt) || got.Quantity != l.Quantity || got.Notes != l.Notes ||
		!slices.Equal(got.MediaIDs, l.MediaIDs) {
		t.Fatalf("committed listing = %+v", got)
	}
	db := e.store.state()
	row := db.txs[d.id]
	want := domain.ListingTransaction{
		ID: d.id, ListingID: l.ID, FarmerID: owner.UserID, ResellerID: buyer.UserID, QuantityGrams: 5_000_000,
		State: domain.StateAwaitingPayment, Version: 1, CreatedAt: created.CreatedAt.Truncate(time.Microsecond),
		RecordedAt: got.UpdatedAt, UpdatedAt: got.UpdatedAt,
	}
	if row != want {
		t.Fatalf("projection =\n%+v\nwant\n%+v", row, want)
	}
	if in := db.processed[created.EventID]; in.Topic != events.TopicTransaction || in.EventType != created.EventType ||
		in.AggregateID != d.id || in.AggregateVersion != 1 || !in.ProcessedAt.Equal(got.UpdatedAt) {
		t.Fatalf("inbox row = %+v", in)
	}
	if len(db.outbox) != 1 { // TODO(events): only ListingCreated; no case for the new remaining quantity yet
		t.Fatalf("outbox = %d messages, want the ListingCreated only", len(db.outbox))
	}

	// A redelivery is a duplicate: no second commitment.
	e.apply(t, created, domain.Duplicate)
	wantListing(t, e.listing(l.ID), 15_000_000, domain.StatusActive, 2)

	// The rest of the life cycle, to BonusAwarded: the state moves, the
	// quantity stays committed once.
	for step := 1; step < len(lifecycle); step++ {
		e.apply(t, d.event(step), domain.Applied)
	}
	wantListing(t, e.listing(l.ID), 15_000_000, domain.StatusActive, 2)
	row = e.store.state().txs[d.id]
	if row.State != domain.StateCompleted || row.Version != 7 || !row.RecordedAt.Equal(want.RecordedAt) ||
		!row.UpdatedAt.After(row.RecordedAt) || row.QuantityGrams != 5_000_000 {
		t.Fatalf("projection after the life cycle = %+v", row)
	}

	// An older event arriving after a newer one (its own event id) is stale.
	e.apply(t, d.event(2), domain.Stale)
	if row := e.store.state().txs[d.id]; row.State != domain.StateCompleted || row.Version != 7 {
		t.Fatalf("projection after a stale event = %+v", row)
	}
	wantListing(t, e.listing(l.ID), 15_000_000, domain.StatusActive, 2)
	if n := len(e.store.state().processed); n != 8 {
		t.Fatalf("inbox rows = %d, want 8 (seven events and the stale one; the duplicate adds none)", n)
	}
}

// Whatever event of a transaction catalog sees first commits its quantity:
// TransactionCreated may have been lost or deleted by retention. The
// TransactionCreated arriving late is then stale.
func TestFirstEventSeenCommits(t *testing.T) {
	for step := 1; step < len(lifecycle); step++ {
		t.Run(lifecycle[step].name, func(t *testing.T) {
			e := newEnv()
			l := e.create(t, farmer(), domain.KindOffer)
			d := newDeal(l, reseller().UserID, 5_000_000)
			e.apply(t, d.event(step), domain.Applied)
			wantListing(t, e.listing(l.ID), 15_000_000, domain.StatusActive, 2)
			if row := e.store.state().txs[d.id]; row.State != lifecycle[step].state || row.Version != int64(step+1) {
				t.Fatalf("projection = %+v", row)
			}
			e.apply(t, d.event(0), domain.Stale)
			wantListing(t, e.listing(l.ID), 15_000_000, domain.StatusActive, 2)
		})
	}
}

// Transactions using up the whole quantity make the listing SOLD_OUT: off the
// boards and hidden from everyone but its owner, the parties and staff.
func TestWholeQuantityCommittedIsSoldOut(t *testing.T) {
	e := newEnv()
	owner, a, b, outsider := farmer(), reseller(), reseller(), reseller()
	l := e.create(t, owner, domain.KindOffer) // 20 t
	e.apply(t, newDeal(l, a.UserID, 12_000_000).event(0), domain.Applied)
	wantListing(t, e.listing(l.ID), 8_000_000, domain.StatusActive, 2)
	e.apply(t, newDeal(l, b.UserID, 8_000_000).event(0), domain.Applied)
	wantListing(t, e.listing(l.ID), 0, domain.StatusSoldOut, 3)

	board, err := e.svc.ListListings(ctx, outsider, domain.ListingFilter{Kind: domain.KindOffer}, domain.PageRequest{})
	if err != nil || len(board.Listings) != 0 {
		t.Fatalf("offer board = %+v, %v; want the SOLD_OUT listing gone", board, err)
	}
	found, err := e.svc.SearchListings(ctx, outsider, "", domain.ListingFilter{}, 0, 0, domain.PageRequest{})
	if err != nil || len(found.Listings) != 0 {
		t.Fatalf("search = %+v, %v; want the SOLD_OUT listing gone", found, err)
	}
	for name, p := range map[string]auth.Principal{"anonymous": {RequestID: "r"}, "outsider": outsider} {
		t.Run(name, func(t *testing.T) {
			_, err := e.svc.GetListing(ctx, p, l.ID.String())
			wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)
		})
	}
	for name, p := range map[string]auth.Principal{"owner": owner, "buyer a": a, "buyer b": b, "staff": admin()} {
		if got, err := e.svc.GetListing(ctx, p, l.ID.String()); err != nil || got.Status != domain.StatusSoldOut ||
			got.RemainingGrams != 0 {
			t.Fatalf("%s reads SOLD_OUT: %+v, %v", name, got, err)
		}
	}
	own, err := e.svc.ListListings(ctx, owner, domain.ListingFilter{OwnerID: owner.UserID.String(),
		Statuses: []eventsv1.ListingStatus{domain.StatusSoldOut}}, domain.PageRequest{})
	if err != nil || len(own.Listings) != 1 || own.Listings[0].ID != l.ID {
		t.Fatalf("owner's SOLD_OUT listings = %+v, %v", own, err)
	}
	if views := e.views.recorded(); len(views) != 0 {
		t.Fatalf("views = %v: reads of a listing that is not ACTIVE count none", views)
	}
	// SOLD_OUT is final: a later commitment (a transaction order-escrow
	// started before it saw the listing sold out) changes nothing more.
	e.apply(t, newDeal(l, outsider.UserID, 1_000_000).event(0), domain.Applied)
	wantListing(t, e.listing(l.ID), 0, domain.StatusSoldOut, 3)
}

// The owner lowered the quantity before catalog saw a transaction that
// order-escrow had already started: the commitment exceeds the quantity, and
// the remaining quantity is clamped at 0.
func TestOverCommitmentClampsAtZero(t *testing.T) {
	e := newEnv()
	owner, buyer := farmer(), reseller()
	l := e.create(t, owner, domain.KindOffer) // 20 t
	late := newDeal(l, buyer.UserID, 15_000_000)
	cut, err := e.svc.UpdateListing(ctx, owner, l.ID.String(),
		domain.ListingFields{Quantity: domain.Mass{Grams: 10_000_000, Unit: domain.UnitTonne}}, []string{"quantity"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	wantListing(t, cut, 10_000_000, domain.StatusActive, 2)
	e.apply(t, late.event(0), domain.Applied)
	got := e.listing(l.ID)
	wantListing(t, got, 0, domain.StatusSoldOut, 3)
	if got.Quantity.Grams != 10_000_000 {
		t.Fatalf("quantity = %d g, want the owner's 10 t", got.Quantity.Grams)
	}
}

// A commitment on a listing that is off the board already updates its
// remaining quantity but keeps its status: only ACTIVE becomes SOLD_OUT.
func TestCommitmentsKeepOffBoardStatuses(t *testing.T) {
	for _, st := range []eventsv1.ListingStatus{domain.StatusClosed, domain.StatusBlocked, domain.StatusDeleted} {
		t.Run(st.String(), func(t *testing.T) {
			e := newEnv()
			owner := farmer()
			l := e.create(t, owner, domain.KindOffer)
			version := int64(1)
			if st == domain.StatusDeleted {
				if err := e.svc.DeleteListing(ctx, owner, l.ID.String(), 0); err != nil {
					t.Fatal(err)
				}
				version = 2
			} else {
				e.setStatus(l.ID, st)
			}
			e.apply(t, newDeal(l, reseller().UserID, 5_000_000).event(0), domain.Applied)
			wantListing(t, e.listing(l.ID), 15_000_000, st, version+1)
			e.apply(t, newDeal(l, reseller().UserID, 15_000_000).event(0), domain.Applied) // the rest
			got := e.listing(l.ID)
			wantListing(t, got, 0, st, version+2)
			if (got.DeletedAt != nil) != (st == domain.StatusDeleted) {
				t.Fatalf("deleted_at = %v", got.DeletedAt)
			}
		})
	}
}

// An event that contradicts catalog's data or an earlier event of the
// transaction wraps ErrInconsistentEvent and writes nothing, not even its
// inbox row.
func TestInconsistentTransactionEvents(t *testing.T) {
	cases := map[string]func(t *testing.T, e env) domain.TransactionUpdate{
		"unknown listing": func(t *testing.T, e env) domain.TransactionUpdate {
			ghost := domain.Listing{ID: uuid.New(), Kind: domain.KindOffer, OwnerID: uuid.New()}
			return newDeal(ghost, uuid.New(), 1_000_000).event(0)
		},
		"another kind": func(t *testing.T, e env) domain.TransactionUpdate {
			u := newDeal(e.create(t, farmer(), domain.KindOffer), uuid.New(), 1_000_000).event(0)
			u.ListingKind = domain.KindBuyRequest
			return u
		},
		"offer not owned by the farmer": func(t *testing.T, e env) domain.TransactionUpdate {
			u := newDeal(e.create(t, farmer(), domain.KindOffer), uuid.New(), 1_000_000).event(0)
			u.FarmerID = uuid.New()
			return u
		},
		"buy-request not owned by the reseller": func(t *testing.T, e env) domain.TransactionUpdate {
			br := e.create(t, reseller(), domain.KindBuyRequest)
			u := newDeal(br, uuid.New(), 1_000_000).event(0)
			u.FarmerID, u.ResellerID = br.OwnerID, uuid.New() // parties as if it were an offer
			return u
		},
		"changed quantity": func(t *testing.T, e env) domain.TransactionUpdate {
			d := newDeal(e.create(t, farmer(), domain.KindOffer), uuid.New(), 1_000_000)
			e.apply(t, d.event(0), domain.Applied)
			u := d.event(1)
			u.QuantityGrams = 2_000_000
			return u
		},
		"changed counterparty": func(t *testing.T, e env) domain.TransactionUpdate {
			d := newDeal(e.create(t, farmer(), domain.KindOffer), uuid.New(), 1_000_000)
			e.apply(t, d.event(0), domain.Applied)
			u := d.event(1)
			u.ResellerID = uuid.New()
			return u
		},
		"moved to another listing": func(t *testing.T, e env) domain.TransactionUpdate {
			owner := farmer()
			d := newDeal(e.create(t, owner, domain.KindOffer), uuid.New(), 1_000_000)
			e.apply(t, d.event(0), domain.Applied)
			u := d.event(1)
			u.ListingID = e.create(t, owner, domain.KindOffer).ID
			return u
		},
		"changed terms in an older event": func(t *testing.T, e env) domain.TransactionUpdate {
			d := newDeal(e.create(t, farmer(), domain.KindOffer), uuid.New(), 1_000_000)
			e.apply(t, d.event(3), domain.Applied)
			u := d.event(0)
			u.QuantityGrams = 3_000_000
			return u
		},
		"no event id": func(t *testing.T, e env) domain.TransactionUpdate {
			u := newDeal(e.create(t, farmer(), domain.KindOffer), uuid.New(), 1_000_000).event(0)
			u.EventID = uuid.Nil
			return u
		},
		"one party on both sides": func(t *testing.T, e env) domain.TransactionUpdate {
			u := newDeal(e.create(t, farmer(), domain.KindOffer), uuid.New(), 1_000_000).event(0)
			u.ResellerID = u.FarmerID
			return u
		},
		"no quantity": func(t *testing.T, e env) domain.TransactionUpdate {
			return newDeal(e.create(t, farmer(), domain.KindOffer), uuid.New(), 0).event(0)
		},
		"no version": func(t *testing.T, e env) domain.TransactionUpdate {
			u := newDeal(e.create(t, farmer(), domain.KindOffer), uuid.New(), 1_000_000).event(0)
			u.Version = 0
			return u
		},
		"no state": func(t *testing.T, e env) domain.TransactionUpdate {
			u := newDeal(e.create(t, farmer(), domain.KindOffer), uuid.New(), 1_000_000).event(0)
			u.State = 0
			return u
		},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv()
			u := bad(t, e)
			before := e.store.state()
			_, err := e.svc.ApplyTransactionEvent(ctx, u)
			if !errors.Is(err, domain.ErrInconsistentEvent) {
				t.Fatalf("err = %v, want ErrInconsistentEvent", err)
			}
			after := e.store.state()
			if !reflect.DeepEqual(after.listings, before.listings) || !maps.Equal(after.txs, before.txs) ||
				!maps.Equal(after.processed, before.processed) {
				t.Fatal("an inconsistent event changed the database")
			}
		})
	}
}

// A failed database transaction writes nothing, not even the inbox row, so
// the consumer's retry commits the quantity; the error is not an
// inconsistency.
func TestApplyTransactionEventDatabaseDown(t *testing.T) {
	e := newEnv()
	l := e.create(t, farmer(), domain.KindOffer)
	u := newDeal(l, reseller().UserID, 5_000_000).event(0)
	down := errors.New("connection refused")
	e.store.update(func(d *fakeDB) { d.failWith = down })
	if _, err := e.svc.ApplyTransactionEvent(ctx, u); !errors.Is(err, down) || errors.Is(err, domain.ErrInconsistentEvent) {
		t.Fatalf("err = %v, want the database error", err)
	}
	e.store.update(func(d *fakeDB) { d.failWith = nil })
	if db := e.store.state(); len(db.processed) != 0 || len(db.txs) != 0 {
		t.Fatalf("a failed transaction wrote %d inbox rows, %d transactions", len(db.processed), len(db.txs))
	}
	e.apply(t, u, domain.Applied)
	wantListing(t, e.listing(l.ID), 15_000_000, domain.StatusActive, 2)
}

// The parties of escrow transactions on a listing read it in any status
// (catalog.proto); a party of another listing's transaction does not, and
// being a party does not make one an editor.
func TestTransactionPartiesReadTheListing(t *testing.T) {
	e := newEnv()
	owner, buyer, outsider := farmer(), reseller(), reseller()
	l := e.create(t, owner, domain.KindOffer)
	other := e.create(t, farmer(), domain.KindOffer)
	e.apply(t, newDeal(other, outsider.UserID, 1_000_000).event(0), domain.Applied) // a party elsewhere
	e.apply(t, newDeal(l, buyer.UserID, 20_000_000).event(0), domain.Applied)
	wantListing(t, e.listing(l.ID), 0, domain.StatusSoldOut, 2)

	reads := func(name string, p auth.Principal, want eventsv1.ListingStatus) {
		t.Helper()
		if got, err := e.svc.GetListing(ctx, p, l.ID.String()); err != nil || got.Status != want {
			t.Fatalf("%s reads %s: %+v, %v", name, want, got, err)
		}
	}
	hidden := func(name string, p auth.Principal) {
		t.Helper()
		if _, err := e.svc.GetListing(ctx, p, l.ID.String()); !isReason(err, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND) {
			t.Fatalf("%s: err = %v, want NOT_FOUND", name, err)
		}
	}
	reads("buyer", buyer, domain.StatusSoldOut)
	hidden("party of another listing", outsider)
	hidden("anonymous", auth.Principal{RequestID: "r"})

	// Deleted by the owner: the transaction continues, and its parties still
	// read the listing it was started from.
	if err := e.svc.DeleteListing(ctx, owner, l.ID.String(), 0); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]auth.Principal{"owner": owner, "buyer": buyer, "staff": admin()} {
		reads(name, p, domain.StatusDeleted)
	}
	hidden("party of another listing", outsider)

	// A party may see the listing but not change it; others learn nothing.
	_, err := e.svc.UpdateListing(ctx, buyer, l.ID.String(), domain.ListingFields{Notes: "x"}, []string{"notes"}, 0)
	wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED)
	wantReason(t, e.svc.DeleteListing(ctx, buyer, l.ID.String(), 0), commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED)
	wantReason(t, e.svc.DeleteListing(ctx, outsider, l.ID.String(), 0), commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)

	// A buy-request: the farmer who sells to it is the party.
	wanted := e.create(t, reseller(), domain.KindBuyRequest)
	seller := farmer()
	e.apply(t, newDeal(wanted, seller.UserID, 20_000_000).event(0), domain.Applied)
	if got, err := e.svc.GetListing(ctx, seller, wanted.ID.String()); err != nil || got.Status != domain.StatusSoldOut {
		t.Fatalf("seller reads the sold-out buy-request: %+v, %v", got, err)
	}
	if _, err := e.svc.GetListing(ctx, buyer, wanted.ID.String()); !isReason(err, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND) {
		t.Fatalf("a party of another listing reads the buy-request: %v", err)
	}
	if views := e.views.recorded(); len(views) != 0 {
		t.Fatalf("views = %v, want none (no ACTIVE listing was opened)", views)
	}
}

func isReason(err error, reason commonv1.ErrorReason) bool {
	var ae *apperr.Error
	return errors.As(err, &ae) && ae.Reason == reason
}

// UpdateListing keeps what escrow transactions have committed: the quantity
// cannot drop below it, and the remaining quantity is the difference.
func TestUpdateListingKeepsCommittedQuantity(t *testing.T) {
	e := newEnv()
	owner := farmer()
	l := e.create(t, owner, domain.KindOffer) // 20 t
	e.apply(t, newDeal(l, reseller().UserID, 6_000_000).event(0), domain.Applied)
	wantListing(t, e.listing(l.ID), 14_000_000, domain.StatusActive, 2)
	quantity := func(grams int64) domain.ListingFields {
		return domain.ListingFields{Quantity: domain.Mass{Grams: grams, Unit: domain.UnitTonne}}
	}

	_, err := e.svc.UpdateListing(ctx, owner, l.ID.String(), quantity(5_000_000), []string{"quantity"}, 2)
	ae := wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_INSUFFICIENT_QUANTITY)
	if ae.Code != codes.FailedPrecondition || ae.Metadata["requested_grams"] != "5000000" ||
		ae.Metadata["committed_grams"] != "6000000" || ae.Metadata["listing_id"] != l.ID.String() {
		t.Fatalf("error = %+v", ae)
	}
	wantListing(t, e.listing(l.ID), 14_000_000, domain.StatusActive, 2)

	got, err := e.svc.UpdateListing(ctx, owner, l.ID.String(), quantity(10_000_000), []string{"quantity"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	wantListing(t, got, 4_000_000, domain.StatusActive, 3)
	got, err = e.svc.UpdateListing(ctx, owner, l.ID.String(), domain.ListingFields{Notes: "Qoplarda"}, []string{"notes"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	wantListing(t, got, 4_000_000, domain.StatusActive, 4)

	// Exactly the committed quantity: nothing remains, the listing is SOLD_OUT
	// and leaves the board.
	got, err = e.svc.UpdateListing(ctx, owner, l.ID.String(), quantity(6_000_000), []string{"quantity"}, 4)
	if err != nil {
		t.Fatal(err)
	}
	wantListing(t, got, 0, domain.StatusSoldOut, 5)
	wantListing(t, e.listing(l.ID), 0, domain.StatusSoldOut, 5)
	_, err = e.svc.UpdateListing(ctx, owner, l.ID.String(), domain.ListingFields{Notes: "x"}, []string{"notes"}, 0)
	if ae := wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_LISTING_NOT_ACTIVE); ae.Metadata["status"] != "SOLD_OUT" {
		t.Fatalf("metadata = %v", ae.Metadata)
	}
}

// transactionRecord is a record of order-escrow's transaction stream as it
// publishes it: the deal's event step (see lifecycle) at version step + 1.
func transactionRecord(t *testing.T, d deal, step int) *kgo.Record {
	t.Helper()
	uzs := func(minor int64) *commonv1.Money { return &commonv1.Money{AmountMinor: minor, CurrencyCode: "UZS"} }
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	total := uzs(d.grams / 1000 * 450_000) // 4 500 so'm per kg
	snap := &eventsv1.TransactionSnapshot{
		Id:          d.id.String(),
		ListingId:   d.listing.ID.String(),
		ListingKind: d.listing.Kind,
		FarmerId:    d.farmer.String(),
		ResellerId:  d.reseller.String(),
		Product:     d.listing.Product,
		Quantity:    &commonv1.Mass{Grams: d.grams, DisplayUnit: commonv1.MassUnit_MASS_UNIT_TONNE},
		UnitPrice:   &commonv1.UnitPrice{Amount: uzs(450_000), Unit: commonv1.MassUnit_MASS_UNIT_KILOGRAM},
		Address:     d.listing.Address.Proto(),
		Amounts: &eventsv1.TransactionAmounts{Subtotal: total, Tax: uzs(0), PlatformFee: uzs(0), Total: total,
			PayoutAmount: total},
		State:     lifecycle[step].state,
		Version:   int64(step + 1),
		CreatedAt: timestamppb.New(at.Add(-time.Hour)),
		UpdatedAt: timestamppb.New(at),
	}
	env := &eventsv1.TransactionEvent{Transaction: snap}
	var event proto.Message
	switch step {
	case 0:
		e := &eventsv1.TransactionCreated{}
		env.Event, event = &eventsv1.TransactionEvent_TransactionCreated{TransactionCreated: e}, e
	case 5:
		snap.CompletedAt = timestamppb.New(at)
		e := &eventsv1.PayoutReleased{Amount: total}
		env.Event, event = &eventsv1.TransactionEvent_PayoutReleased{PayoutReleased: e}, e
	default:
		t.Fatalf("transactionRecord: step %d is not built here", step)
	}
	env.Metadata = &eventsv1.EventMetadata{
		EventId:          uuid.Must(uuid.NewV7()).String(),
		EventType:        string(proto.MessageName(event)),
		OccurredAt:       timestamppb.New(at),
		Producer:         "order-escrow",
		AggregateType:    commonv1.ResourceType_RESOURCE_TYPE_TRANSACTION,
		AggregateId:      d.id.String(),
		AggregateVersion: int64(step + 1),
		CorrelationId:    uuid.NewString(),
		Actor:            &commonv1.Actor{Kind: &commonv1.Actor_Service{Service: "order-escrow"}},
	}
	value, err := proto.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return &kgo.Record{
		Topic: events.TopicTransaction,
		Key:   []byte(d.id.String()),
		Value: value,
		Headers: []kgo.RecordHeader{
			{Key: events.HeaderContentType, Value: []byte(events.ContentType)},
			{Key: events.HeaderEventType, Value: []byte(env.GetMetadata().GetEventType())},
			{Key: events.HeaderEventID, Value: []byte(env.GetMetadata().GetEventId())},
		},
	}
}

// End to end through the transaction-stream handler: TransactionCreated for
// the whole quantity makes the offer SOLD_OUT and lets the buyer read it;
// PayoutReleased completes the transaction without committing anything more;
// redeliveries change nothing; an inconsistent record is poison.
func TestTransactionStreamEndToEnd(t *testing.T) {
	e := newEnv()
	owner, buyer, outsider := farmer(), reseller(), reseller()
	l := e.create(t, owner, domain.KindOffer)
	validator, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	handler := events.NewTransactionHandler(e.svc, validator, slog.New(slog.DiscardHandler))
	handle := func(r *kgo.Record) {
		t.Helper()
		if err := handler.HandleRecord(ctx, r); err != nil {
			t.Fatalf("HandleRecord: %v", err)
		}
	}
	d := newDeal(l, buyer.UserID, 20_000_000)
	created, payout := transactionRecord(t, d, 0), transactionRecord(t, d, 5)

	if _, err := e.svc.GetListing(ctx, buyer, l.ID.String()); err != nil { // ACTIVE: public
		t.Fatal(err)
	}
	handle(created)
	wantListing(t, e.listing(l.ID), 0, domain.StatusSoldOut, 2)
	if got, err := e.svc.GetListing(ctx, buyer, l.ID.String()); err != nil || got.Status != domain.StatusSoldOut {
		t.Fatalf("buyer reads the SOLD_OUT offer: %+v, %v", got, err)
	}
	_, err = e.svc.GetListing(ctx, outsider, l.ID.String())
	wantReason(t, err, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)

	handle(payout)
	row := e.store.state().txs[d.id]
	if row.State != domain.StateCompleted || row.Version != 6 || row.QuantityGrams != 20_000_000 ||
		row.FarmerID != owner.UserID || row.ResellerID != buyer.UserID || row.ListingID != l.ID {
		t.Fatalf("projection = %+v", row)
	}
	wantListing(t, e.listing(l.ID), 0, domain.StatusSoldOut, 2)

	// At-least-once delivery: both records again.
	handle(created)
	handle(payout)
	db := e.store.state()
	wantListing(t, db.listings[l.ID], 0, domain.StatusSoldOut, 2)
	if len(db.processed) != 2 || len(db.txs) != 1 || db.txs[d.id].Version != 6 {
		t.Fatalf("inbox rows = %d, transactions = %d (%+v); want 2 and 1 at version 6", len(db.processed), len(db.txs),
			db.txs[d.id])
	}

	// A transaction on the offer that names someone else as its farmer
	// contradicts catalog's listing: poison, nothing written.
	forged := newDeal(l, outsider.UserID, 1_000_000)
	forged.farmer = uuid.New()
	err = handler.HandleRecord(ctx, transactionRecord(t, forged, 0))
	if !events.IsPermanent(err) || !errors.Is(err, domain.ErrInconsistentEvent) {
		t.Fatalf("err = %v, want a permanent inconsistency", err)
	}
	if db := e.store.state(); len(db.processed) != 2 || len(db.txs) != 1 {
		t.Fatalf("an inconsistent record wrote: inbox rows = %d, transactions = %d", len(db.processed), len(db.txs))
	}
}
