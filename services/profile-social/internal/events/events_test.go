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
)

func newValidator(t *testing.T) protovalidate.Validator {
	t.Helper()
	v, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestDecodeAccountRecord(t *testing.T) {
	v := newValidator(t)
	user := uuid.New()
	env := accountEvent(user, 3, &eventsv1.AccountRoleChanged{
		Roles: []commonv1.Role{commonv1.Role_ROLE_ADMIN}, GrantedRoles: []commonv1.Role{commonv1.Role_ROLE_ADMIN},
	}, commonv1.Role_ROLE_ADMIN)
	u, err := DecodeAccountRecord(record(t, TopicAccount, env, user.String()), v)
	if err != nil {
		t.Fatal(err)
	}
	if u.UserID != user || u.EventID.String() != env.GetMetadata().GetEventId() || u.Topic != TopicAccount ||
		u.EventType != "agrogram.events.v1.AccountRoleChanged" || !u.OccurredAt.Equal(testNow) ||
		u.Account.Version != 3 {
		t.Fatalf("update = %+v", u)
	}
	if len(u.Account.Roles) != 1 || u.Account.Roles[0] != commonv1.Role_ROLE_ADMIN {
		t.Fatalf("roles = %v, want [ADMIN]", u.Account.Roles)
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

type errString string

func (e errString) Error() string { return string(e) }
