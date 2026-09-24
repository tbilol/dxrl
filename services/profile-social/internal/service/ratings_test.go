package service_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/tbilol/dxrl/services/profile-social/internal/auth"
	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
	"github.com/tbilol/dxrl/services/profile-social/internal/events"
)

const testTraceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

// completed seeds a completed transaction between farmer and reseller the
// way the phase-3 transaction consumer will (RecordCompletedTransaction).
func (h *harness) completed(farmer, reseller uuid.UUID) uuid.UUID {
	h.t.Helper()
	id := uuid.New()
	ok, err := h.svc.RecordCompletedTransaction(ctx, domain.CompletedTransaction{
		ID: id, FarmerID: farmer, ResellerID: reseller, CompletedAt: start,
	})
	if err != nil || !ok {
		h.t.Fatalf("RecordCompletedTransaction = %v, %v", ok, err)
	}
	return id
}

func TestRateUserNeedsACompletedTransaction(t *testing.T) {
	h := newHarness(t)
	rater, farmer, stranger := user(), user(), user()
	h.create(farmer, "Farmer")

	// Nothing is recorded before phase 3: every rating fails closed.
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

func TestRecordCompletedTransactionIsIdempotent(t *testing.T) {
	h := newHarness(t)
	farmer, reseller := uuid.New(), uuid.New()
	tx := domain.CompletedTransaction{ID: uuid.New(), FarmerID: farmer, ResellerID: reseller, CompletedAt: start}
	for i, want := range []bool{true, false} {
		got, err := h.svc.RecordCompletedTransaction(ctx, tx)
		if err != nil || got != want {
			t.Fatalf("call %d = %v, %v; want %v", i+1, got, err, want)
		}
	}
	if db := h.store.snapshot(); db.profiles[farmer].DealsCount != 1 || db.profiles[reseller].DealsCount != 1 {
		t.Fatalf("deals = %d / %d", db.profiles[farmer].DealsCount, db.profiles[reseller].DealsCount)
	}
	if _, err := h.svc.RecordCompletedTransaction(ctx, domain.CompletedTransaction{ID: uuid.New(), FarmerID: farmer, ResellerID: farmer}); err == nil {
		t.Fatal("one party on both sides must be refused")
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
