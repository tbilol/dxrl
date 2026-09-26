package events

import (
	"testing"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	testNow         = time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	testTraceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
)

// accountEvent is a valid AccountEvent as identity publishes it: the snapshot
// of user at version, with event as the oneof case (nil: no case, as a
// consumer on an older schema sees a newer one).
func accountEvent(user uuid.UUID, version int64, event proto.Message, roles ...commonv1.Role) *eventsv1.AccountEvent {
	if len(roles) == 0 {
		roles = []commonv1.Role{commonv1.Role_ROLE_RESELLER}
	}
	eventType := "agrogram.events.v1.AccountFrozen" // a case newer than this code
	if event != nil {
		eventType = string(proto.MessageName(event))
	}
	env := &eventsv1.AccountEvent{
		Metadata: &eventsv1.EventMetadata{
			EventId:          uuid.Must(uuid.NewV7()).String(),
			EventType:        eventType,
			OccurredAt:       timestamppb.New(testNow),
			Producer:         "identity",
			AggregateType:    commonv1.ResourceType_RESOURCE_TYPE_USER,
			AggregateId:      user.String(),
			AggregateVersion: version,
			CorrelationId:    "req-1",
			Traceparent:      testTraceparent,
			Actor:            &commonv1.Actor{Kind: &commonv1.Actor_UserId{UserId: user.String()}},
		},
		Account: &eventsv1.AccountSnapshot{
			Id:               user.String(),
			Roles:            roles,
			IsVerifiedFarmer: containsRole(roles, commonv1.Role_ROLE_FARMER),
			Version:          version,
			CreatedAt:        timestamppb.New(testNow.Add(-time.Hour)),
			UpdatedAt:        timestamppb.New(testNow),
		},
	}
	switch e := event.(type) {
	case *eventsv1.AccountCreated:
		env.Event = &eventsv1.AccountEvent_AccountCreated{AccountCreated: e}
	case *eventsv1.AccountRoleChanged:
		env.Event = &eventsv1.AccountEvent_AccountRoleChanged{AccountRoleChanged: e}
	case *eventsv1.AccountPremiumChanged:
		env.Event = &eventsv1.AccountEvent_AccountPremiumChanged{AccountPremiumChanged: e}
		env.Account.IsPremium = e.GetIsPremium()
	case *eventsv1.FarmerVerified:
		env.Event = &eventsv1.AccountEvent_FarmerVerified{FarmerVerified: e}
	}
	return env
}

func containsRole(roles []commonv1.Role, r commonv1.Role) bool {
	for _, x := range roles {
		if x == r {
			return true
		}
	}
	return false
}

// record wraps an envelope into a Kafka record the way the relays publish it.
func record(t *testing.T, topic string, env proto.Message, key string) *kgo.Record {
	t.Helper()
	value, err := proto.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return &kgo.Record{
		Topic: topic,
		Key:   []byte(key),
		Value: value,
		Headers: []kgo.RecordHeader{
			{Key: HeaderContentType, Value: []byte(ContentType)},
			{Key: HeaderEventID, Value: []byte(eventID(env))},
		},
	}
}

func eventID(env proto.Message) string {
	switch e := env.(type) {
	case *eventsv1.AccountEvent:
		return e.GetMetadata().GetEventId()
	case *eventsv1.TransactionEvent:
		return e.GetMetadata().GetEventId()
	}
	return ""
}

func uzs(minor int64) *commonv1.Money {
	return &commonv1.Money{AmountMinor: minor, CurrencyCode: "UZS"}
}

// transactionEvent is a valid TransactionEvent in the given state.
func transactionEvent(id, farmer, reseller uuid.UUID, state eventsv1.TransactionState) *eventsv1.TransactionEvent {
	snap := &eventsv1.TransactionSnapshot{
		Id:          id.String(),
		ListingId:   uuid.NewString(),
		ListingKind: commonv1.ListingKind_LISTING_KIND_OFFER,
		FarmerId:    farmer.String(),
		ResellerId:  reseller.String(),
		Product:     commonv1.ProductType_PRODUCT_TYPE_BUGDOY,
		Quantity:    &commonv1.Mass{Grams: 1_000_000, DisplayUnit: commonv1.MassUnit_MASS_UNIT_TONNE},
		UnitPrice:   &commonv1.UnitPrice{Amount: uzs(400_000), Unit: commonv1.MassUnit_MASS_UNIT_KILOGRAM},
		Address:     &commonv1.Address{Region: commonv1.Region_REGION_SAMARQAND},
		Amounts: &eventsv1.TransactionAmounts{Subtotal: uzs(400_000_000), Tax: uzs(0), PlatformFee: uzs(0),
			Total: uzs(400_000_000), PayoutAmount: uzs(400_000_000)},
		State:     state,
		Version:   6,
		CreatedAt: timestamppb.New(testNow.Add(-48 * time.Hour)),
		UpdatedAt: timestamppb.New(testNow),
	}
	env := &eventsv1.TransactionEvent{
		Metadata: &eventsv1.EventMetadata{
			EventId:          uuid.Must(uuid.NewV7()).String(),
			EventType:        "agrogram.events.v1.PayoutReleased",
			OccurredAt:       timestamppb.New(testNow),
			Producer:         "order-escrow",
			AggregateType:    commonv1.ResourceType_RESOURCE_TYPE_TRANSACTION,
			AggregateId:      id.String(),
			AggregateVersion: 6,
			CorrelationId:    "req-2",
			Actor:            &commonv1.Actor{Kind: &commonv1.Actor_Service{Service: "order-escrow"}},
		},
		Transaction: snap,
	}
	if state == eventsv1.TransactionState_TRANSACTION_STATE_COMPLETED {
		snap.CompletedAt = timestamppb.New(testNow)
		env.Event = &eventsv1.TransactionEvent_PayoutReleased{PayoutReleased: &eventsv1.PayoutReleased{Amount: uzs(400_000_000)}}
	} else {
		env.Metadata.EventType = "agrogram.events.v1.PaymentHeld"
		env.Event = &eventsv1.TransactionEvent_PaymentHeld{PaymentHeld: &eventsv1.PaymentHeld{Amount: uzs(400_000_000)}}
	}
	return env
}

// bonusAwarded is the BonusAwarded order-escrow publishes right after the
// PayoutReleased done: the same COMPLETED transaction at the next version
// (bonus_points changed), with its own event id.
func bonusAwarded(done *eventsv1.TransactionEvent) *eventsv1.TransactionEvent {
	env := proto.CloneOf(done)
	env.Metadata.EventId = uuid.Must(uuid.NewV7()).String()
	env.Metadata.EventType = "agrogram.events.v1.BonusAwarded"
	env.Metadata.CausationId = done.GetMetadata().GetEventId()
	env.Metadata.AggregateVersion++
	env.Transaction.Version++
	env.Transaction.BonusPoints = 1
	env.Event = &eventsv1.TransactionEvent_BonusAwarded{BonusAwarded: &eventsv1.BonusAwarded{
		RecipientId: env.GetTransaction().GetResellerId(), Points: 1}}
	return env
}

func timestampOf(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }
