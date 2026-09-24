package events

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
)

func newValidator(t *testing.T) protovalidate.Validator {
	t.Helper()
	v, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func sampleProfile() domain.Profile {
	until := testNow.Add(-time.Minute) // Premium ended before the event
	return domain.Profile{
		ID:     uuid.New(),
		Status: domain.ProfileActive,
		ProfileFields: domain.ProfileFields{
			DisplayName: "Alisher Dehqon", AvatarMediaID: uuid.New(), Region: commonv1.Region_REGION_SAMARQAND, District: "Urgut",
		},
		Version: 3, CreatedAt: testNow.Add(-24 * time.Hour), UpdatedAt: testNow,
		FollowerCount: 7, FollowingCount: 2, RatingCount: 2, RatingSum: 9,
		Account: domain.Account{Roles: []commonv1.Role{domain.RoleReseller, domain.RoleFarmer}, IsVerifiedFarmer: true,
			IsPremium: true, PremiumUntil: &until, Version: 4},
	}
}

func TestNewProfileEvent(t *testing.T) {
	p := sampleProfile()
	rater, tx, rating := uuid.New(), uuid.New(), uuid.New()
	m, err := NewProfileEvent(ProfileChange{
		Profile:       p,
		Event:         &eventsv1.RatingUpdated{RatingId: rating.String(), RaterId: rater.String(), TransactionId: tx.String(), Score: 5},
		OccurredAt:    testNow,
		CorrelationID: "req-7",
		Traceparent:   testTraceparent,
		Actor:         &commonv1.Actor{Kind: &commonv1.Actor_UserId{UserId: rater.String()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if RatingUpdatedType != "agrogram.events.v1.RatingUpdated" {
		t.Fatalf("event type = %q", RatingUpdatedType)
	}
	if m.Topic != TopicProfile || m.Key != p.ID.String() || m.EventType != RatingUpdatedType || m.ID.Version() != 7 ||
		!m.CreatedAt.Equal(testNow) {
		t.Fatalf("message = %+v", m)
	}
	for k, v := range map[string]string{
		HeaderContentType: ContentType, HeaderEventType: RatingUpdatedType, HeaderEventID: m.ID.String(),
		HeaderRequestID: "req-7", HeaderTraceparent: testTraceparent,
	} {
		if m.Headers[k] != v {
			t.Fatalf("header %s = %q, want %q", k, m.Headers[k], v)
		}
	}
	var env eventsv1.ProfileEvent
	if err := proto.Unmarshal(m.Value, &env); err != nil {
		t.Fatal(err)
	}
	md, s := env.GetMetadata(), env.GetProfile()
	if md.GetEventId() != m.ID.String() || md.GetProducer() != ProducerName || md.GetAggregateId() != p.ID.String() ||
		md.GetAggregateVersion() != 3 || md.GetAggregateType() != commonv1.ResourceType_RESOURCE_TYPE_USER ||
		md.GetActor().GetUserId() != rater.String() {
		t.Fatalf("metadata = %v", md)
	}
	if s.GetDisplayName() != "Alisher Dehqon" || s.GetVersion() != 3 || s.GetRating().GetCount() != 2 ||
		s.GetRating().GetAverage() != 4.5 || s.GetFollowerCount() != 7 || !s.GetIsVerifiedFarmer() ||
		s.GetIsPremium() || s.GetAvatar().GetMediaId() != p.AvatarMediaID.String() || s.GetRegion() != commonv1.Region_REGION_SAMARQAND {
		t.Fatalf("snapshot = %v (Premium must be evaluated at occurred_at)", s)
	}
	if e := env.GetRatingUpdated(); e.GetRatingId() != rating.String() || e.GetScore() != 5 || e.GetTransactionId() != tx.String() {
		t.Fatalf("event = %v", e)
	}
}

func TestNewProfileEventRejectsInvalid(t *testing.T) {
	pending := sampleProfile()
	pending.DisplayName = "" // a PENDING profile: the snapshot requires a display name
	ev := &eventsv1.RatingUpdated{RatingId: uuid.NewString(), RaterId: uuid.NewString(), TransactionId: uuid.NewString(), Score: 4}
	actor := &commonv1.Actor{Kind: &commonv1.Actor_Service{Service: ProducerName}}
	if _, err := NewProfileEvent(ProfileChange{Profile: pending, Event: ev, OccurredAt: testNow, CorrelationID: "r", Actor: actor}); err == nil {
		t.Fatal("want an error for a profile without a display name")
	}
	if _, err := NewProfileEvent(ProfileChange{Profile: sampleProfile(), OccurredAt: testNow, CorrelationID: "r", Actor: actor}); err == nil {
		t.Fatal("want an error without an event")
	}
	if _, err := NewProfileEvent(ProfileChange{Profile: sampleProfile(), Event: &eventsv1.AccountCreated{},
		OccurredAt: testNow, CorrelationID: "r", Actor: actor}); err == nil {
		t.Fatal("want an error for a message that is not a ProfileEvent case")
	}
}

func TestDecodeAccountRecord(t *testing.T) {
	v := newValidator(t)
	user := uuid.New()
	until := testNow.Add(30 * 24 * time.Hour)
	env := accountEvent(user, 3, &eventsv1.AccountPremiumChanged{IsPremium: true},
		commonv1.Role_ROLE_FARMER, commonv1.Role_ROLE_RESELLER)
	env.Account.PremiumUntil = timestampOf(until)
	u, err := DecodeAccountRecord(record(t, TopicAccount, env, user.String()), v)
	if err != nil {
		t.Fatal(err)
	}
	if u.UserID != user || u.EventID.String() != env.GetMetadata().GetEventId() || u.Topic != TopicAccount ||
		u.EventType != "agrogram.events.v1.AccountPremiumChanged" || !u.OccurredAt.Equal(testNow) ||
		u.Account.Version != 3 || !u.Account.IsPremium || !u.Account.IsVerifiedFarmer ||
		u.Account.PremiumUntil == nil || !u.Account.PremiumUntil.Equal(until) {
		t.Fatalf("update = %+v", u)
	}
	if len(u.Account.Roles) != 2 || u.Account.Roles[0] != commonv1.Role_ROLE_RESELLER || u.Account.Roles[1] != commonv1.Role_ROLE_FARMER {
		t.Fatalf("roles = %v, want sorted", u.Account.Roles)
	}
}

// A oneof case added by a newer identity arrives as an unknown field: the
// record still decodes and the snapshot applies.
func TestDecodeAccountRecordUnknownCase(t *testing.T) {
	user := uuid.New()
	env := accountEvent(user, 5, nil)
	value, err := proto.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	value = protowire.AppendTag(value, 42, protowire.BytesType) // a future `AccountFrozen account_frozen = 42;`
	value = protowire.AppendBytes(value, []byte{})
	r := &kgo.Record{Topic: TopicAccount, Key: []byte(user.String()), Value: value}
	u, err := DecodeAccountRecord(r, newValidator(t))
	if err != nil {
		t.Fatalf("unknown oneof case must not fail: %v", err)
	}
	if u.Account.Version != 5 || u.EventType != "agrogram.events.v1.AccountFrozen" {
		t.Fatalf("update = %+v", u)
	}
}

func TestDecodeAccountRecordPoison(t *testing.T) {
	v := newValidator(t)
	user := uuid.New()
	valid := func() *eventsv1.AccountEvent { return accountEvent(user, 2, &eventsv1.AccountCreated{}) }
	cases := map[string]*kgo.Record{
		"garbage": {Topic: TopicAccount, Value: []byte{0xff, 0xff, 0xff}},
		"content-type": func() *kgo.Record {
			r := record(t, TopicAccount, valid(), user.String())
			r.Headers[0].Value = []byte("application/json")
			return r
		}(),
		"no metadata": func() *kgo.Record {
			env := valid()
			env.Metadata = nil
			return record(t, TopicAccount, env, user.String())
		}(),
		"id mismatch": func() *kgo.Record {
			env := valid()
			env.Account.Id = uuid.NewString()
			return record(t, TopicAccount, env, user.String())
		}(),
		"version mismatch": func() *kgo.Record {
			env := valid()
			env.Metadata.AggregateVersion = 3
			return record(t, TopicAccount, env, user.String())
		}(),
		"invalid role": func() *kgo.Record {
			env := valid()
			env.Account.Roles = []commonv1.Role{commonv1.Role_ROLE_UNSPECIFIED}
			return record(t, TopicAccount, env, user.String())
		}(),
	}
	for name, r := range cases {
		if _, err := DecodeAccountRecord(r, v); err == nil || !IsPermanent(err) {
			t.Errorf("%s: err = %v, want a permanent error", name, err)
		}
	}
}

func TestNewDeadLetter(t *testing.T) {
	r := &kgo.Record{
		Topic: TopicAccount, Partition: 2, Offset: 41, Key: []byte("user-1"), Value: []byte{0xff},
		Headers: []kgo.RecordHeader{{Key: HeaderEventID, Value: []byte("e-1")}},
	}
	first := testNow.Add(-time.Minute)
	long := strings.Repeat("é", maxDeadLetterMessage) // 2 bytes each: must be cut on a rune boundary

	for _, tc := range []struct {
		name     string
		cause    error
		reason   commonv1.ErrorReason
		failure  string
		attempts int
	}{
		{"poison", Permanent(errString("decode AccountEvent: bad wire type")), commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED, "poison", 1},
		{"exhausted", errString(long), commonv1.ErrorReason_ERROR_REASON_INTERNAL, "retries_exhausted", 5},
	} {
		out, err := NewDeadLetter(r, "profilesocial", tc.cause, tc.attempts, first, testNow)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if out.Topic != TopicDeadLetter || string(out.Key) != "user-1" || len(out.Headers) != 1 ||
			string(out.Headers[0].Value) != ContentType {
			t.Fatalf("%s: record = %+v", tc.name, out)
		}
		var dl eventsv1.DeadLetter
		if err := proto.Unmarshal(out.Value, &dl); err != nil {
			t.Fatal(err)
		}
		if dl.GetSourceTopic() != TopicAccount || dl.GetSourcePartition() != 2 || dl.GetSourceOffset() != 41 ||
			string(dl.GetValue()) != "\xff" || string(dl.GetHeaders()[HeaderEventID]) != "e-1" ||
			dl.GetConsumerGroup() != "profilesocial" || dl.GetAttempts() != int32(tc.attempts) ||
			!dl.GetFirstFailedAt().AsTime().Equal(first) || !dl.GetDeadLetteredAt().AsTime().Equal(testNow) {
			t.Fatalf("%s: dead letter = %v", tc.name, &dl)
		}
		e := dl.GetError()
		if e.GetReason() != tc.reason || e.GetDomain() != ProducerName || e.GetMetadata()["failure"] != tc.failure ||
			len(e.GetMessage()) > maxDeadLetterMessage || !utf8.ValidString(e.GetMessage()) {
			t.Fatalf("%s: error = %v", tc.name, e)
		}
	}
}

func TestDecodeTransactionRecord(t *testing.T) {
	v := newValidator(t)
	id, farmer, reseller := uuid.New(), uuid.New(), uuid.New()

	done := transactionEvent(id, farmer, reseller, eventsv1.TransactionState_TRANSACTION_STATE_COMPLETED)
	tx, completed, err := DecodeTransactionRecord(record(t, TopicTransaction, done, id.String()), v)
	if err != nil || !completed {
		t.Fatalf("completed = %v, err = %v", completed, err)
	}
	if tx.ID != id || tx.FarmerID != farmer || tx.ResellerID != reseller || !tx.CompletedAt.Equal(testNow) {
		t.Fatalf("transaction = %+v", tx)
	}

	held := transactionEvent(id, farmer, reseller, eventsv1.TransactionState_TRANSACTION_STATE_PAYMENT_HELD)
	if _, completed, err := DecodeTransactionRecord(record(t, TopicTransaction, held, id.String()), v); err != nil || completed {
		t.Fatalf("payment held: completed = %v, err = %v", completed, err)
	}

	same := transactionEvent(id, farmer, farmer, eventsv1.TransactionState_TRANSACTION_STATE_COMPLETED)
	if _, _, err := DecodeTransactionRecord(record(t, TopicTransaction, same, id.String()), v); !IsPermanent(err) {
		t.Fatalf("one party on both sides: err = %v, want a permanent error", err)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
