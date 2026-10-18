package events

import (
	"bytes"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tbilol/dxrl/services/catalog/internal/domain"
)

var testNow = time.Date(2026, 9, 25, 10, 30, 0, 0, time.UTC)

func sampleListing() domain.Listing {
	return domain.Listing{
		ID:      uuid.Must(uuid.NewV7()),
		Kind:    domain.KindOffer,
		OwnerID: uuid.New(),
		ListingFields: domain.ListingFields{
			Product:  commonv1.ProductType_PRODUCT_TYPE_KARTOSHKA,
			Quantity: domain.Mass{Grams: 20_000_000, Unit: domain.UnitTonne},
			Price:    &domain.Price{AmountMinor: 450_000, Currency: domain.CurrencyUZS, Unit: domain.UnitKilogram},
			Phone:    "+998901234567",
			Address: domain.Address{Region: commonv1.Region_REGION_SAMARQAND, District: "Urgut", Line: "Mahalla 5",
				Location: &domain.GeoPoint{Latitude: 39.4, Longitude: 67.2}},
			Notes:    "Sifatli kartoshka, qoplarda",
			MediaIDs: []uuid.UUID{uuid.New(), uuid.New()},
		},
		Status:          domain.StatusActive,
		Version:         1,
		CreateRequestID: uuid.New(),
		CreatedAt:       testNow,
		UpdatedAt:       testNow,
	}
}

func TestNewListingEvent(t *testing.T) {
	l := sampleListing()
	actor := &commonv1.Actor{Kind: &commonv1.Actor_UserId{UserId: l.OwnerID.String()}}
	tp := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	m, err := NewListingEvent(ListingChange{
		Listing: l, Event: &eventsv1.ListingCreated{}, OccurredAt: testNow,
		CorrelationID: "req-7", Traceparent: tp, Tracestate: "vendor=1", Actor: actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.Topic != TopicListing || m.Key != l.ID.String() || m.EventType != "agrogram.events.v1.ListingCreated" ||
		m.EventType != ListingCreatedType || !m.CreatedAt.Equal(testNow) {
		t.Fatalf("message = %+v", m)
	}
	wantHeaders := map[string]string{
		HeaderContentType: ContentType, HeaderEventType: ListingCreatedType, HeaderEventID: m.ID.String(),
		HeaderRequestID: "req-7", HeaderTraceparent: tp, HeaderTracestate: "vendor=1",
	}
	for k, v := range wantHeaders {
		if m.Headers[k] != v {
			t.Fatalf("header %s = %q, want %q", k, m.Headers[k], v)
		}
	}

	var env eventsv1.ListingEvent
	if err := proto.Unmarshal(m.Value, &env); err != nil {
		t.Fatal(err)
	}
	if err := protovalidate.Validate(&env); err != nil {
		t.Fatalf("envelope invalid: %v", err)
	}
	md := env.GetMetadata()
	if md.GetEventId() != m.ID.String() || md.GetProducer() != "catalog" ||
		md.GetAggregateType() != commonv1.ResourceType_RESOURCE_TYPE_LISTING || md.GetAggregateId() != l.ID.String() ||
		md.GetAggregateVersion() != 1 || md.GetCorrelationId() != "req-7" || md.GetActor().GetUserId() != l.OwnerID.String() ||
		!md.GetOccurredAt().AsTime().Equal(testNow) {
		t.Fatalf("metadata = %v", md)
	}
	if env.GetListingCreated() == nil {
		t.Fatalf("event case = %T, want ListingCreated", env.GetEvent())
	}
	s := env.GetListing()
	if s.GetId() != l.ID.String() || s.GetKind() != domain.KindOffer || s.GetOwnerId() != l.OwnerID.String() ||
		s.GetProduct() != commonv1.ProductType_PRODUCT_TYPE_KARTOSHKA || s.GetQuantity().GetGrams() != 20_000_000 ||
		s.GetQuantity().GetDisplayUnit() != domain.UnitTonne ||
		s.GetPrice().GetAmount().GetAmountMinor() != 450_000 || s.GetPrice().GetAmount().GetCurrencyCode() != "UZS" ||
		s.GetPrice().GetUnit() != domain.UnitKilogram || s.GetAddress().GetDistrict() != "Urgut" ||
		s.GetAddress().GetLocation().GetLatitude() != 39.4 || s.GetNotes() != l.Notes || len(s.GetMedia()) != 2 ||
		s.GetMedia()[0].GetMediaId() != l.MediaIDs[0].String() || s.GetStatus() != domain.StatusActive || s.GetVersion() != 1 {
		t.Fatalf("snapshot = %v", s)
	}
	// metadata.proto: never publish contact details.
	if bytes.Contains(m.Value, []byte(l.Phone)) || bytes.Contains(m.Value, []byte("901234567")) {
		t.Fatal("the contact phone leaked into the event")
	}
}

func TestNewListingEventNegotiableAndErrors(t *testing.T) {
	l := sampleListing()
	l.Price, l.MediaIDs, l.Address.Location = nil, nil, nil
	actor := &commonv1.Actor{Kind: &commonv1.Actor_Service{Service: ProducerName}}
	m, err := NewListingEvent(ListingChange{Listing: l, Event: &eventsv1.ListingCreated{}, OccurredAt: testNow,
		CorrelationID: "req-1", Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	var env eventsv1.ListingEvent
	if err := proto.Unmarshal(m.Value, &env); err != nil || env.GetListing().GetPrice() != nil ||
		len(env.GetListing().GetMedia()) != 0 || m.Headers[HeaderTraceparent] != "" {
		t.Fatalf("negotiable snapshot = %v, %v", env.GetListing(), err)
	}

	if _, err := NewListingEvent(ListingChange{Listing: l, OccurredAt: testNow, CorrelationID: "r", Actor: actor}); err == nil {
		t.Fatal("no event: want an error")
	}
	if _, err := NewListingEvent(ListingChange{Listing: l, Event: &eventsv1.ProfileSnapshot{}, OccurredAt: testNow,
		CorrelationID: "r", Actor: actor}); err == nil {
		t.Fatal("foreign event: want an error")
	}
	bad := l
	bad.Version = 0 // the snapshot requires version >= 1
	if _, err := NewListingEvent(ListingChange{Listing: bad, Event: &eventsv1.ListingCreated{}, OccurredAt: testNow,
		CorrelationID: "r", Actor: actor}); err == nil {
		t.Fatal("invalid snapshot: want a validation error")
	}
}

// The topic names are the contract's (metadata.proto, deploy/kafka/create-topics.sh).
func TestTopics(t *testing.T) {
	for got, want := range map[string]string{
		TopicListing: "agrogram.catalog.listing.v1",
	} {
		if got != want {
			t.Errorf("topic %q, want %q", got, want)
		}
	}
}
