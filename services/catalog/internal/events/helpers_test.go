package events

import (
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func newValidator(t *testing.T) protovalidate.Validator {
	t.Helper()
	v, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// testTx is an escrow transaction: a reseller buys 5 t of a farmer's offer.
type testTx struct {
	id, listing, farmer, reseller uuid.UUID
}

func newTestTx() testTx {
	return testTx{id: uuid.New(), listing: uuid.New(), farmer: uuid.New(), reseller: uuid.New()}
}

// txStates are the states after each event of a transaction, in the order
// order-escrow publishes them (transaction.proto): TransactionCreated,
// PaymentHeld, ReceiptSubmitted, ProductHandover, ReceiptConfirmed,
// PayoutReleased, BonusAwarded. The version after event i is i + 1.
var txStates = []eventsv1.TransactionState{
	eventsv1.TransactionState_TRANSACTION_STATE_AWAITING_PAYMENT,
	eventsv1.TransactionState_TRANSACTION_STATE_PAYMENT_HELD,
	eventsv1.TransactionState_TRANSACTION_STATE_RECEIPT_SUBMITTED,
	eventsv1.TransactionState_TRANSACTION_STATE_HANDED_OVER,
	eventsv1.TransactionState_TRANSACTION_STATE_RECEIVED,
	eventsv1.TransactionState_TRANSACTION_STATE_COMPLETED,
	eventsv1.TransactionState_TRANSACTION_STATE_COMPLETED,
}

func uzs(minor int64) *commonv1.Money {
	return &commonv1.Money{AmountMinor: minor, CurrencyCode: "UZS"}
}

// transactionEvent is a valid TransactionEvent as order-escrow publishes it:
// event step (0 TransactionCreated ... 6 BonusAwarded, see txStates) of tx
// at version step + 1, with a fresh event id.
func transactionEvent(tx testTx, step int) *eventsv1.TransactionEvent {
	total := uzs(2_250_000_000) // 5 000 kg x 4 500 so'm
	snap := &eventsv1.TransactionSnapshot{
		Id:          tx.id.String(),
		ListingId:   tx.listing.String(),
		ListingKind: commonv1.ListingKind_LISTING_KIND_OFFER,
		FarmerId:    tx.farmer.String(),
		ResellerId:  tx.reseller.String(),
		Product:     commonv1.ProductType_PRODUCT_TYPE_KARTOSHKA,
		Quantity:    &commonv1.Mass{Grams: 5_000_000, DisplayUnit: commonv1.MassUnit_MASS_UNIT_TONNE},
		UnitPrice:   &commonv1.UnitPrice{Amount: uzs(450_000), Unit: commonv1.MassUnit_MASS_UNIT_KILOGRAM},
		Address:     &commonv1.Address{Region: commonv1.Region_REGION_SAMARQAND, District: "Urgut"},
		Amounts: &eventsv1.TransactionAmounts{Subtotal: total, Tax: uzs(0), PlatformFee: uzs(0), Total: total,
			PayoutAmount: total},
		State:     txStates[step],
		Version:   int64(step + 1),
		CreatedAt: timestamppb.New(testNow.Add(-time.Hour)),
		UpdatedAt: timestamppb.New(testNow),
	}
	if step >= 1 {
		snap.PaymentHeldAt = timestamppb.New(testNow.Add(-50 * time.Minute))
	}
	if step >= 2 {
		snap.Receipt = &eventsv1.WeighbridgeReceipt{TicketNumber: "T-1042",
			VerificationStatus: eventsv1.ReceiptVerificationStatus_RECEIPT_VERIFICATION_STATUS_VERIFIED}
	}
	if step >= 5 {
		snap.CompletedAt = timestamppb.New(testNow)
	}
	env := &eventsv1.TransactionEvent{Transaction: snap}
	var event proto.Message
	switch step {
	case 0:
		e := &eventsv1.TransactionCreated{}
		env.Event, event = &eventsv1.TransactionEvent_TransactionCreated{TransactionCreated: e}, e
	case 1:
		e := &eventsv1.PaymentHeld{Amount: total}
		env.Event, event = &eventsv1.TransactionEvent_PaymentHeld{PaymentHeld: e}, e
	case 2:
		e := &eventsv1.ReceiptSubmitted{Receipt: snap.GetReceipt()}
		env.Event, event = &eventsv1.TransactionEvent_ReceiptSubmitted{ReceiptSubmitted: e}, e
	case 3:
		e := &eventsv1.ProductHandover{}
		env.Event, event = &eventsv1.TransactionEvent_ProductHandover{ProductHandover: e}, e
	case 4:
		e := &eventsv1.ReceiptConfirmed{}
		env.Event, event = &eventsv1.TransactionEvent_ReceiptConfirmed{ReceiptConfirmed: e}, e
	case 5:
		e := &eventsv1.PayoutReleased{Amount: total}
		env.Event, event = &eventsv1.TransactionEvent_PayoutReleased{PayoutReleased: e}, e
	default:
		e := &eventsv1.BonusAwarded{RecipientId: tx.reseller.String(), Points: 1}
		env.Event, event = &eventsv1.TransactionEvent_BonusAwarded{BonusAwarded: e}, e
		snap.BonusPoints = 1
	}
	env.Metadata = &eventsv1.EventMetadata{
		EventId:          uuid.Must(uuid.NewV7()).String(),
		EventType:        string(proto.MessageName(event)),
		OccurredAt:       timestamppb.New(testNow),
		Producer:         "order-escrow",
		AggregateType:    commonv1.ResourceType_RESOURCE_TYPE_TRANSACTION,
		AggregateId:      tx.id.String(),
		AggregateVersion: int64(step + 1),
		CorrelationId:    "req-9",
		Actor:            &commonv1.Actor{Kind: &commonv1.Actor_UserId{UserId: tx.reseller.String()}},
	}
	return env
}

// txRecord wraps env into a record of the transaction stream the way
// order-escrow's relay publishes it, keyed by key (normally the transaction
// id).
func txRecord(t *testing.T, env *eventsv1.TransactionEvent, key string) *kgo.Record {
	t.Helper()
	value, err := proto.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	md := env.GetMetadata()
	return &kgo.Record{
		Topic: TopicTransaction,
		Key:   []byte(key),
		Value: value,
		Headers: []kgo.RecordHeader{
			{Key: HeaderContentType, Value: []byte(ContentType)},
			{Key: HeaderEventType, Value: []byte(md.GetEventType())},
			{Key: HeaderEventID, Value: []byte(md.GetEventId())},
		},
	}
}

// txStep is txRecord of transactionEvent(tx, step), keyed by the
// transaction id.
func txStep(t *testing.T, tx testTx, step int) *kgo.Record {
	t.Helper()
	return txRecord(t, transactionEvent(tx, step), tx.id.String())
}

// eventIDOf returns the event-id header of r.
func eventIDOf(r *kgo.Record) uuid.UUID { return uuid.MustParse(header(r, HeaderEventID)) }
