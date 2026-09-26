package service_test

import (
	"errors"
	"log/slog"
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

	"github.com/tbilol/dxrl/services/profile-social/internal/auth"
	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
	"github.com/tbilol/dxrl/services/profile-social/internal/events"
)

const testTraceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

// completion is a decoded COMPLETED record of the transaction stream for tx:
// eventType ("PayoutReleased", "BonusAwarded") at version, with a fresh
// event id.
func completion(tx domain.CompletedTransaction, eventType string, version int64) domain.TransactionUpdate {
	return domain.TransactionUpdate{
		EventID:     uuid.Must(uuid.NewV7()),
		EventType:   "agrogram.events.v1." + eventType,
		Topic:       events.TopicTransaction,
		OccurredAt:  start,
		Version:     version,
		Transaction: tx,
	}
}

// completed records a completed transaction between farmer and reseller the
// way the transaction-stream consumer does on PayoutReleased
// (RecordCompletedTransaction).
func (h *harness) completed(farmer, reseller uuid.UUID) uuid.UUID {
	h.t.Helper()
	tx := domain.CompletedTransaction{ID: uuid.New(), FarmerID: farmer, ResellerID: reseller, CompletedAt: start}
	outcome, err := h.svc.RecordCompletedTransaction(ctx, completion(tx, "PayoutReleased", 6))
	if err != nil || outcome != domain.Applied {
		h.t.Fatalf("RecordCompletedTransaction = %s, %v", outcome, err)
	}
	return tx.ID
}

func TestRateUserNeedsACompletedTransaction(t *testing.T) {
	h := newHarness(t)
	rater, farmer, stranger := user(), user(), user()
	h.create(farmer, "Farmer")

	// No completed transaction with that id: the rating fails closed.
	_, err := h.svc.RateUser(ctx, rater, uuid.NewString(), uuid.NewString(), 5, "")
	ae := wantErr(t, err, codes.FailedPrecondition, commonv1.ErrorReason_ERROR_REASON_RATING_NOT_ALLOWED)
	if ae.Metadata["transaction_id"] == "" {
		t.Fatalf("metadata = %v", ae.Metadata)
	}
	tx := h.completed(farmer.UserID, rater.UserID)
	_, err = h.svc.RateUser(ctx, stranger, uuid.NewString(), tx.String(), 5, "")
	wantErr(t, err, codes.FailedPrecondition, commonv1.ErrorReason_ERROR_REASON_RATING_NOT_ALLOWED)
	_, err = h.svc.RateUser(ctx, auth.Principal{}, uuid.NewString(), tx.String(), 5, "")
	wantErr(t, err, codes.Unauthenticated, commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED)
	if len(h.store.snapshot().ratings) != 0 {
		t.Fatal("no rating may be stored")
	}
}

func TestRateUser(t *testing.T) {
	h := newHarness(t)
	farmer, reseller := user(), user()
	h.create(farmer, "Farmer")
	h.create(reseller, "Reseller")
	tx := h.completed(farmer.UserID, reseller.UserID)
	reseller.Traceparent = testTraceparent

	h.clock.Advance(time.Minute)
	reqID := uuid.NewString()
	r, err := h.svc.RateUser(ctx, reseller, reqID, tx.String(), 4, "  Sifatli bug'doy  ")
	if err != nil {
		t.Fatal(err)
	}
	if r.RatedUserID != farmer.UserID || r.RaterID != reseller.UserID || r.TransactionID != tx || r.Score != 4 ||
		r.Comment != "Sifatli bug'doy" || !r.CreatedAt.Equal(start.Add(time.Minute)) {
		t.Fatalf("rating = %+v", r)
	}
	row := h.row(farmer.UserID)
	if row.RatingCount != 1 || row.RatingSum != 4 || row.Version != 2 || !row.UpdatedAt.Equal(r.CreatedAt) {
		t.Fatalf("rated profile = %+v", row)
	}

	// RatingUpdated for the rated user, in the same transaction.
	outbox := h.store.snapshot().outbox
	if len(outbox) != 1 {
		t.Fatalf("outbox = %d messages, want 1", len(outbox))
	}
	m := outbox[0]
	if m.Topic != events.TopicProfile || m.Key != farmer.UserID.String() || m.EventType != events.RatingUpdatedType {
		t.Fatalf("message = %+v", m)
	}
	var env eventsv1.ProfileEvent
	if err := proto.Unmarshal(m.Value, &env); err != nil {
		t.Fatal(err)
	}
	if md := env.GetMetadata(); md.GetAggregateVersion() != 2 || md.GetActor().GetUserId() != reseller.UserID.String() ||
		md.GetCorrelationId() != reseller.RequestID || md.GetTraceparent() != testTraceparent {
		t.Fatalf("metadata = %v", md)
	}
	if s := env.GetProfile(); s.GetRating().GetCount() != 1 || s.GetRating().GetAverage() != 4 || s.GetDisplayName() != "Farmer" {
		t.Fatalf("snapshot = %v", s)
	}
	if e := env.GetRatingUpdated(); e.GetRatingId() != r.ID.String() || e.GetRaterId() != reseller.UserID.String() ||
		e.GetTransactionId() != tx.String() || e.GetScore() != 4 {
		t.Fatalf("event = %v", e)
	}

	// A retry returns the stored rating and publishes nothing more.
	again, err := h.svc.RateUser(ctx, reseller, reqID, tx.String(), 1, "changed my mind")
	if err != nil || again.ID != r.ID || again.Score != 4 {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	// Once per transaction and rater.
	_, err = h.svc.RateUser(ctx, reseller, uuid.NewString(), tx.String(), 2, "")
	wantErr(t, err, codes.AlreadyExists, commonv1.ErrorReason_ERROR_REASON_ALREADY_RATED)
	// The other party rates back.
	if _, err := h.svc.RateUser(ctx, farmer, uuid.NewString(), tx.String(), 5, ""); err != nil {
		t.Fatal(err)
	}
	if db := h.store.snapshot(); len(db.outbox) != 2 || len(db.ratings) != 2 || db.profiles[farmer.UserID].RatingCount != 1 {
		t.Fatalf("outbox = %d, ratings = %d", len(db.outbox), len(db.ratings))
	}
	// A second deal adds to the aggregate.
	tx2 := h.completed(farmer.UserID, reseller.UserID)
	if _, err := h.svc.RateUser(ctx, reseller, uuid.NewString(), tx2.String(), 5, ""); err != nil {
		t.Fatal(err)
	}
	if prof, _ := h.svc.GetProfile(ctx, reseller, farmer.UserID.String()); prof.RatingAverage() != 4.5 || prof.RatingCount != 2 ||
		prof.DealsCount != 2 {
		t.Fatalf("farmer = %+v", prof)
	}
}

// A user without a created profile can be rated: the rating and aggregate
// are kept on the PENDING row, but no RatingUpdated (no display name).
func TestRateUserWithoutProfile(t *testing.T) {
	h := newHarness(t)
	farmer, reseller := user(), user()
	tx := h.completed(farmer.UserID, reseller.UserID)
	if _, err := h.svc.RateUser(ctx, reseller, uuid.NewString(), tx.String(), 3, ""); err != nil {
		t.Fatal(err)
	}
	db := h.store.snapshot()
	if row := db.profiles[farmer.UserID]; row.Status != domain.ProfilePending || row.RatingCount != 1 || len(db.outbox) != 0 {
		t.Fatalf("row = %+v, outbox = %d", row, len(db.outbox))
	}
	if prof := h.create(farmer, "Late Farmer"); prof.RatingCount != 1 || prof.RatingAverage() != 3 || prof.DealsCount != 1 {
		t.Fatalf("created = %+v", prof)
	}
}

// The first COMPLETED event records the transaction; its redelivery is a
// duplicate, and the BonusAwarded after it (a new event id, same transaction)
// is stale: the deal counts once.
func TestRecordCompletedTransactionIsIdempotent(t *testing.T) {
	h := newHarness(t)
	farmer, reseller := uuid.New(), uuid.New()
	tx := domain.CompletedTransaction{ID: uuid.New(), FarmerID: farmer, ResellerID: reseller, CompletedAt: start}
	payout := completion(tx, "PayoutReleased", 6)
	for _, step := range []struct {
		name string
		u    domain.TransactionUpdate
		want domain.ApplyOutcome
	}{
		{"PayoutReleased", payout, domain.Applied},
		{"PayoutReleased again", payout, domain.Duplicate},
		{"BonusAwarded", completion(tx, "BonusAwarded", 7), domain.Stale},
	} {
		if got, err := h.svc.RecordCompletedTransaction(ctx, step.u); err != nil || got != step.want {
			t.Fatalf("%s = %s, %v; want %s", step.name, got, err, step.want)
		}
	}
	db := h.store.snapshot()
	if db.profiles[farmer].DealsCount != 1 || db.profiles[reseller].DealsCount != 1 || len(db.txs) != 1 {
		t.Fatalf("deals = %d / %d, transactions = %d", db.profiles[farmer].DealsCount, db.profiles[reseller].DealsCount, len(db.txs))
	}
	// Both events are in the inbox, so a redelivery of either is a duplicate.
	if len(db.processed) != 2 {
		t.Fatalf("inbox rows = %d, want 2", len(db.processed))
	}
	if e := db.processed[payout.EventID]; e.Topic != events.TopicTransaction || e.EventType != payout.EventType ||
		e.AggregateID != tx.ID || e.AggregateVersion != 6 || !e.ProcessedAt.Equal(start) {
		t.Fatalf("inbox row = %+v", e)
	}

	// Invalid input is refused before any database work.
	other := domain.CompletedTransaction{ID: uuid.New(), FarmerID: farmer, ResellerID: reseller, CompletedAt: start}
	for name, mutate := range map[string]func(*domain.TransactionUpdate){
		"one party on both sides": func(u *domain.TransactionUpdate) { u.Transaction.ResellerID = farmer },
		"no transaction id":       func(u *domain.TransactionUpdate) { u.Transaction.ID = uuid.Nil },
		"no party":                func(u *domain.TransactionUpdate) { u.Transaction.FarmerID = uuid.Nil },
		"no event id":             func(u *domain.TransactionUpdate) { u.EventID = uuid.Nil },
	} {
		u := completion(other, "PayoutReleased", 6)
		mutate(&u)
		if _, err := h.svc.RecordCompletedTransaction(ctx, u); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if db := h.store.snapshot(); len(db.processed) != 2 || len(db.txs) != 1 {
		t.Fatalf("refused input changed the database: inbox rows = %d, transactions = %d", len(db.processed), len(db.txs))
	}
}

// A failed database transaction records nothing, not even the inbox row, so
// the consumer's retry records the transaction.
func TestRecordCompletedTransactionDatabaseDown(t *testing.T) {
	h := newHarness(t)
	down := errors.New("connection refused")
	h.store.update(func(db *fakeDB) { db.failWith = down })
	u := completion(domain.CompletedTransaction{ID: uuid.New(), FarmerID: uuid.New(), ResellerID: uuid.New(), CompletedAt: start},
		"PayoutReleased", 6)
	if _, err := h.svc.RecordCompletedTransaction(ctx, u); !errors.Is(err, down) {
		t.Fatalf("err = %v, want the database error", err)
	}
	h.store.update(func(db *fakeDB) { db.failWith = nil })
	if got, err := h.svc.RecordCompletedTransaction(ctx, u); err != nil || got != domain.Applied {
		t.Fatalf("retry = %s, %v; want applied", got, err)
	}
}

// transactionRecord is one record of order-escrow's transaction stream as it
// publishes it: the snapshot of txID in state at version, with event as the
// oneof case.
func transactionRecord(t *testing.T, txID, farmer, reseller uuid.UUID, state eventsv1.TransactionState, version int64,
	event proto.Message,
) *kgo.Record {
	t.Helper()
	uzs := func(minor int64) *commonv1.Money { return &commonv1.Money{AmountMinor: minor, CurrencyCode: "UZS"} }
	env := &eventsv1.TransactionEvent{
		Metadata: &eventsv1.EventMetadata{
			EventId:          uuid.Must(uuid.NewV7()).String(),
			EventType:        string(proto.MessageName(event)),
			OccurredAt:       timestamppb.New(start),
			Producer:         "order-escrow",
			AggregateType:    commonv1.ResourceType_RESOURCE_TYPE_TRANSACTION,
			AggregateId:      txID.String(),
			AggregateVersion: version,
			CorrelationId:    uuid.NewString(),
			Actor:            &commonv1.Actor{Kind: &commonv1.Actor_Service{Service: "order-escrow"}},
		},
		Transaction: &eventsv1.TransactionSnapshot{
			Id:          txID.String(),
			ListingId:   uuid.NewString(),
			ListingKind: commonv1.ListingKind_LISTING_KIND_OFFER,
			FarmerId:    farmer.String(),
			ResellerId:  reseller.String(),
			Product:     commonv1.ProductType_PRODUCT_TYPE_BUGDOY,
			Quantity:    &commonv1.Mass{Grams: 2_000_000, DisplayUnit: commonv1.MassUnit_MASS_UNIT_TONNE},
			UnitPrice:   &commonv1.UnitPrice{Amount: uzs(450_000), Unit: commonv1.MassUnit_MASS_UNIT_KILOGRAM},
			Address:     &commonv1.Address{Region: commonv1.Region_REGION_JIZZAX, District: "Zomin"},
			Amounts: &eventsv1.TransactionAmounts{Subtotal: uzs(900_000_000), Tax: uzs(0), PlatformFee: uzs(0),
				Total: uzs(900_000_000), PayoutAmount: uzs(900_000_000)},
			State:     state,
			Version:   version,
			CreatedAt: timestamppb.New(start.Add(-72 * time.Hour)),
			UpdatedAt: timestamppb.New(start),
		},
	}
	if state == eventsv1.TransactionState_TRANSACTION_STATE_COMPLETED {
		env.Transaction.CompletedAt = timestamppb.New(start)
	}
	switch e := event.(type) {
	case *eventsv1.PaymentHeld:
		env.Event = &eventsv1.TransactionEvent_PaymentHeld{PaymentHeld: e}
	case *eventsv1.PayoutReleased:
		env.Event = &eventsv1.TransactionEvent_PayoutReleased{PayoutReleased: e}
	case *eventsv1.BonusAwarded:
		env.Event = &eventsv1.TransactionEvent_BonusAwarded{BonusAwarded: e}
		env.Transaction.BonusPoints = e.GetPoints()
	}
	value, err := proto.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	md := env.GetMetadata()
	return &kgo.Record{
		Topic: events.TopicTransaction,
		Key:   []byte(txID.String()),
		Value: value,
		Headers: []kgo.RecordHeader{
			{Key: events.HeaderContentType, Value: []byte(events.ContentType)},
			{Key: events.HeaderEventType, Value: []byte(md.GetEventType())},
			{Key: events.HeaderEventID, Value: []byte(md.GetEventId())},
		},
	}
}

// End to end through the transaction-stream handler: RateUser is locked until
// order-escrow's PayoutReleased is consumed, then both parties may rate each
// other once; a redelivered PayoutReleased and the BonusAwarded after it
// change nothing.
func TestTransactionStreamUnlocksRatings(t *testing.T) {
	h := newHarness(t)
	farmer, reseller, outsider := user(domain.RoleReseller, domain.RoleFarmer), user(), user()
	h.create(farmer, "Farmer")
	h.create(reseller, "Reseller")
	validator, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	handler := events.NewTransactionHandler(h.svc, validator, slog.New(slog.DiscardHandler))
	txID := uuid.New()
	record := func(state eventsv1.TransactionState, version int64, event proto.Message) *kgo.Record {
		return transactionRecord(t, txID, farmer.UserID, reseller.UserID, state, version, event)
	}
	handle := func(r *kgo.Record) {
		t.Helper()
		if err := handler.HandleRecord(ctx, r); err != nil {
			t.Fatalf("HandleRecord: %v", err)
		}
	}
	notAllowed := func(p auth.Principal) {
		t.Helper()
		_, err := h.svc.RateUser(ctx, p, uuid.NewString(), txID.String(), 5, "")
		wantErr(t, err, codes.FailedPrecondition, commonv1.ErrorReason_ERROR_REASON_RATING_NOT_ALLOWED)
	}

	// Before COMPLETED: locked, and the skipped record left no trace.
	notAllowed(reseller)
	handle(record(eventsv1.TransactionState_TRANSACTION_STATE_PAYMENT_HELD, 2,
		&eventsv1.PaymentHeld{Amount: &commonv1.Money{AmountMinor: 900_000_000, CurrencyCode: "UZS"}}))
	notAllowed(reseller)
	if db := h.store.snapshot(); len(db.processed) != 0 || len(db.txs) != 0 {
		t.Fatalf("a record before COMPLETED touched the database: inbox rows = %d, transactions = %d",
			len(db.processed), len(db.txs))
	}

	payout := record(eventsv1.TransactionState_TRANSACTION_STATE_COMPLETED, 6,
		&eventsv1.PayoutReleased{Amount: &commonv1.Money{AmountMinor: 900_000_000, CurrencyCode: "UZS"}})
	handle(payout)

	// Both parties rate each other; outsiders cannot, and nobody twice.
	if r, err := h.svc.RateUser(ctx, farmer, uuid.NewString(), txID.String(), 5, "Ishonchli xaridor"); err != nil ||
		r.RatedUserID != reseller.UserID {
		t.Fatalf("farmer rates reseller = %+v, %v", r, err)
	}
	if r, err := h.svc.RateUser(ctx, reseller, uuid.NewString(), txID.String(), 4, ""); err != nil || r.RatedUserID != farmer.UserID {
		t.Fatalf("reseller rates farmer = %+v, %v", r, err)
	}
	notAllowed(outsider)
	_, err = h.svc.RateUser(ctx, reseller, uuid.NewString(), txID.String(), 1, "")
	wantErr(t, err, codes.AlreadyExists, commonv1.ErrorReason_ERROR_REASON_ALREADY_RATED)

	// A redelivery and the BonusAwarded after the payout count nothing twice.
	handle(payout)
	handle(record(eventsv1.TransactionState_TRANSACTION_STATE_COMPLETED, 7,
		&eventsv1.BonusAwarded{RecipientId: reseller.UserID.String(), Points: 1}))
	db := h.store.snapshot()
	for _, p := range []auth.Principal{farmer, reseller} {
		if row := db.profiles[p.UserID]; row.DealsCount != 1 || row.RatingCount != 1 {
			t.Fatalf("profile %s: deals = %d, ratings = %d; want 1 and 1", p.UserID, row.DealsCount, row.RatingCount)
		}
	}
	if len(db.txs) != 1 || len(db.processed) != 2 || len(db.ratings) != 2 || len(db.outbox) != 2 {
		t.Fatalf("transactions = %d, inbox rows = %d, ratings = %d, outbox = %d; want 1, 2, 2, 2",
			len(db.txs), len(db.processed), len(db.ratings), len(db.outbox))
	}
}

func TestListRatings(t *testing.T) {
	h := newHarness(t)
	farmer := user()
	h.create(farmer, "Farmer")
	for _, score := range []int32{5, 4, 3} {
		reseller := user()
		tx := h.completed(farmer.UserID, reseller.UserID)
		h.clock.Advance(time.Second)
		if _, err := h.svc.RateUser(ctx, reseller, uuid.NewString(), tx.String(), score, ""); err != nil {
			t.Fatal(err)
		}
	}
	first, err := h.svc.ListRatings(ctx, auth.Principal{}, farmer.UserID.String(), domain.PageRequest{Size: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Ratings) != 2 || first.Ratings[0].Score != 3 || first.Ratings[1].Score != 4 || first.Count != 3 ||
		first.Average != 4 || first.NextPageToken == "" {
		t.Fatalf("first page = %+v", first)
	}
	second, err := h.svc.ListRatings(ctx, auth.Principal{}, farmer.UserID.String(), domain.PageRequest{Size: 2, Token: first.NextPageToken})
	if err != nil || len(second.Ratings) != 1 || second.Ratings[0].Score != 5 || second.NextPageToken != "" {
		t.Fatalf("second page = %+v, %v", second, err)
	}
	_, err = h.svc.ListRatings(ctx, auth.Principal{}, uuid.NewString(), domain.PageRequest{})
	wantErr(t, err, codes.NotFound, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND)
}
