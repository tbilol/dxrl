package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/tbilol/dxrl/services/catalog/internal/domain"
)

var errDBDown = errors.New("postgres: connection refused")

// fakeApplier mimics the service's idempotency: an inbox of event ids and the
// newest version per transaction. fail decides per attempt whether an update
// fails (attempt counts from 1 per event id).
type fakeApplier struct {
	mu       sync.Mutex
	calls    []domain.TransactionUpdate // updates that did not fail, in order
	outcomes []domain.ApplyOutcome
	attempts map[uuid.UUID]int
	inbox    map[uuid.UUID]bool
	versions map[uuid.UUID]int64
	fail     func(u domain.TransactionUpdate, attempt int) error
}

func (f *fakeApplier) ApplyTransactionEvent(_ context.Context, u domain.TransactionUpdate) (domain.ApplyOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attempts == nil {
		f.attempts, f.inbox, f.versions = map[uuid.UUID]int{}, map[uuid.UUID]bool{}, map[uuid.UUID]int64{}
	}
	f.attempts[u.EventID]++
	if f.fail != nil {
		if err := f.fail(u, f.attempts[u.EventID]); err != nil {
			return 0, err
		}
	}
	outcome := domain.Applied
	switch {
	case f.inbox[u.EventID]:
		outcome = domain.Duplicate
	case u.Version <= f.versions[u.TransactionID]:
		outcome = domain.Stale
	default:
		f.versions[u.TransactionID] = u.Version
	}
	f.inbox[u.EventID] = true
	f.calls = append(f.calls, u)
	f.outcomes = append(f.outcomes, outcome)
	return outcome, nil
}

func (f *fakeApplier) snapshot() (calls []domain.TransactionUpdate, outcomes []domain.ApplyOutcome, attempts map[uuid.UUID]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	attempts = make(map[uuid.UUID]int, len(f.attempts))
	for k, v := range f.attempts {
		attempts[k] = v
	}
	return append([]domain.TransactionUpdate(nil), f.calls...), append([]domain.ApplyOutcome(nil), f.outcomes...), attempts
}

// Every event of the life cycle decodes into the transaction's terms, its
// state and version: catalog needs all of them, whatever the state.
func TestDecodeTransactionRecord(t *testing.T) {
	v := newValidator(t)
	tx := newTestTx()
	names := []string{"TransactionCreated", "PaymentHeld", "ReceiptSubmitted", "ProductHandover", "ReceiptConfirmed",
		"PayoutReleased", "BonusAwarded"}
	for step, name := range names {
		t.Run(name, func(t *testing.T) {
			env := transactionEvent(tx, step)
			u, err := DecodeTransactionRecord(txRecord(t, env, tx.id.String()), v)
			if err != nil {
				t.Fatal(err)
			}
			want := domain.TransactionUpdate{
				EventID:       uuid.MustParse(env.GetMetadata().GetEventId()),
				EventType:     "agrogram.events.v1." + name,
				Topic:         TopicTransaction,
				OccurredAt:    testNow,
				Version:       int64(step + 1),
				TransactionID: tx.id,
				ListingID:     tx.listing,
				ListingKind:   domain.KindOffer,
				FarmerID:      tx.farmer,
				ResellerID:    tx.reseller,
				QuantityGrams: 5_000_000,
				State:         txStates[step],
				CreatedAt:     testNow.Add(-time.Hour),
			}
			if u != want {
				t.Fatalf("update =\n%+v\nwant\n%+v", u, want)
			}
		})
	}

	// A buy-request: the reseller owns the listing, the farmer started the
	// transaction; the decoder passes the kind through for the service to check.
	env := transactionEvent(tx, 0)
	env.Transaction.ListingKind = commonv1.ListingKind_LISTING_KIND_BUY_REQUEST
	if u, err := DecodeTransactionRecord(txRecord(t, env, tx.id.String()), v); err != nil || u.ListingKind != domain.KindBuyRequest {
		t.Fatalf("buy-request: update = %+v, err = %v", u, err)
	}
}

// A oneof case added by a newer order-escrow arrives as an unknown field: the
// record still decodes, from the snapshot alone.
func TestDecodeTransactionRecordUnknownCase(t *testing.T) {
	tx := newTestTx()
	env := transactionEvent(tx, 1)
	env.Event = nil
	env.Metadata.EventType = "agrogram.events.v1.TransactionDisputed" // a case newer than this code
	value, err := proto.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	value = protowire.AppendTag(value, 42, protowire.BytesType) // a future `TransactionDisputed transaction_disputed = 42;`
	value = protowire.AppendBytes(value, []byte{})
	r := &kgo.Record{Topic: TopicTransaction, Key: []byte(tx.id.String()), Value: value}
	u, err := DecodeTransactionRecord(r, newValidator(t))
	if err != nil {
		t.Fatalf("unknown oneof case must not fail: %v", err)
	}
	if u.Version != 2 || u.State != domain.StatePaymentHeld || u.QuantityGrams != 5_000_000 ||
		u.EventType != "agrogram.events.v1.TransactionDisputed" {
		t.Fatalf("update = %+v", u)
	}
}

// Poison is decided before anything reaches the service, in every state.
func TestDecodeTransactionRecordPoison(t *testing.T) {
	v := newValidator(t)
	tx := newTestTx()
	mutated := func(step int, mutate func(*eventsv1.TransactionEvent)) *kgo.Record {
		env := transactionEvent(tx, step)
		mutate(env)
		return txRecord(t, env, tx.id.String())
	}
	cases := map[string]*kgo.Record{
		"garbage": {Topic: TopicTransaction, Key: []byte(tx.id.String()), Value: []byte{0xff, 0xff, 0xff}},
		"content-type": func() *kgo.Record {
			r := txStep(t, tx, 0)
			r.Headers[0].Value = []byte("application/json")
			return r
		}(),
		"no metadata":             mutated(0, func(e *eventsv1.TransactionEvent) { e.Metadata = nil }),
		"no snapshot":             mutated(0, func(e *eventsv1.TransactionEvent) { e.Transaction = nil }),
		"no state":                mutated(1, func(e *eventsv1.TransactionEvent) { e.Transaction.State = 0 }),
		"listing id not a uuid":   mutated(0, func(e *eventsv1.TransactionEvent) { e.Transaction.ListingId = "listing-7" }),
		"one party on both sides": mutated(0, func(e *eventsv1.TransactionEvent) { e.Transaction.ResellerId = tx.farmer.String() }),
		"zero quantity":           mutated(0, func(e *eventsv1.TransactionEvent) { e.Transaction.Quantity.Grams = 0 }),
		"wrong aggregate type": mutated(0, func(e *eventsv1.TransactionEvent) {
			e.Metadata.AggregateType = commonv1.ResourceType_RESOURCE_TYPE_LISTING
		}),
		"aggregate id mismatch":     mutated(3, func(e *eventsv1.TransactionEvent) { e.Metadata.AggregateId = uuid.NewString() }),
		"snapshot id mismatch":      mutated(5, func(e *eventsv1.TransactionEvent) { e.Transaction.Id = uuid.NewString() }),
		"aggregate version too new": mutated(0, func(e *eventsv1.TransactionEvent) { e.Metadata.AggregateVersion = 2 }),
		"snapshot version too old":  mutated(6, func(e *eventsv1.TransactionEvent) { e.Transaction.Version = 6 }),
	}
	for name, r := range cases {
		if u, err := DecodeTransactionRecord(r, v); !IsPermanent(err) {
			t.Errorf("%s: update = %+v, err = %v; want a permanent error", name, u, err)
		}
	}
}

// The handler applies a record keyed by something else than its transaction
// id (it only warns), wraps an applier failure with the event and transaction
// ids and leaves it retryable, and turns an inconsistent event into poison.
func TestTransactionHandler(t *testing.T) {
	tx := newTestTx()
	applier := &fakeApplier{}
	h := NewTransactionHandler(applier, newValidator(t), slog.New(slog.DiscardHandler))
	if err := h.HandleRecord(context.Background(), txRecord(t, transactionEvent(tx, 0), "not-the-transaction-id")); err != nil {
		t.Fatal(err)
	}
	if calls, outcomes, _ := applier.snapshot(); len(calls) != 1 || outcomes[0] != domain.Applied || calls[0].TransactionID != tx.id {
		t.Fatalf("calls = %+v, outcomes = %v", calls, outcomes)
	}

	held := txStep(t, tx, 1)
	applier.fail = func(domain.TransactionUpdate, int) error { return errDBDown }
	err := h.HandleRecord(context.Background(), held)
	if !errors.Is(err, errDBDown) || IsPermanent(err) || !strings.Contains(err.Error(), tx.id.String()) ||
		!strings.Contains(err.Error(), eventIDOf(held).String()) {
		t.Fatalf("err = %v, want the applier's error with the event and transaction ids, retryable", err)
	}

	applier.fail = func(domain.TransactionUpdate, int) error {
		return fmt.Errorf("apply transaction event: %w: listing is owned by someone else", domain.ErrInconsistentEvent)
	}
	err = h.HandleRecord(context.Background(), held)
	if !IsPermanent(err) || !errors.Is(err, domain.ErrInconsistentEvent) || !strings.Contains(err.Error(), tx.id.String()) {
		t.Fatalf("err = %v, want a permanent error wrapping ErrInconsistentEvent", err)
	}

	// Poison never reaches the applier.
	_, _, before := applier.snapshot()
	if err := h.HandleRecord(context.Background(), &kgo.Record{Topic: TopicTransaction, Value: []byte{0xff}}); !IsPermanent(err) {
		t.Fatalf("garbage: err = %v, want a permanent error", err)
	}
	if _, _, after := applier.snapshot(); len(after) != len(before) {
		t.Fatalf("attempts = %v after poison, want %v", after, before)
	}
}
