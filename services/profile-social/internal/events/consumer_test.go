package events

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
)

var errDBDown = errors.New("postgres: connection refused")

// fakeApplier records applied updates; fail decides per attempt whether the
// update fails (attempt counts from 1 per event id).
type fakeApplier struct {
	mu       sync.Mutex
	applied  []domain.AccountUpdate
	attempts map[uuid.UUID]int
	fail     func(u domain.AccountUpdate, attempt int) error
}

func (f *fakeApplier) ApplyAccountEvent(_ context.Context, u domain.AccountUpdate) (domain.ApplyOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attempts == nil {
		f.attempts = map[uuid.UUID]int{}
	}
	f.attempts[u.EventID]++
	if f.fail != nil {
		if err := f.fail(u, f.attempts[u.EventID]); err != nil {
			return 0, err
		}
	}
	f.applied = append(f.applied, u)
	return domain.Applied, nil
}

func (f *fakeApplier) snapshot() (applied []domain.AccountUpdate, attempts map[uuid.UUID]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	attempts = make(map[uuid.UUID]int, len(f.attempts))
	for k, v := range f.attempts {
		attempts[k] = v
	}
	return append([]domain.AccountUpdate(nil), f.applied...), attempts
}

// fakeDLQ records dead letters; while failing is set, every publish fails.
type fakeDLQ struct {
	mu      sync.Mutex
	records []*kgo.Record
	calls   int
	failing bool
}

func (d *fakeDLQ) ProduceSync(_ context.Context, rs ...*kgo.Record) kgo.ProduceResults {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	res := make(kgo.ProduceResults, len(rs))
	for i, r := range rs {
		res[i] = kgo.ProduceResult{Record: r}
		if d.failing {
			res[i].Err = errors.New("broker down")
			continue
		}
		d.records = append(d.records, r)
	}
	return res
}

func (d *fakeDLQ) snapshot() (records []*kgo.Record, calls int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*kgo.Record(nil), d.records...), d.calls
}

// consumerHarness runs a Consumer on a kfake cluster with one partition per
// topic. Like serve.go it subscribes identity's account stream (applier).
type consumerHarness struct {
	t        *testing.T
	producer *kgo.Client
	client   *kgo.Client
	applier  *fakeApplier
	dlq      *fakeDLQ
	consumer *Consumer
	cancel   context.CancelFunc
	done     chan struct{}
}

func newConsumerHarness(t *testing.T, cfg ConsumerConfig) *consumerHarness {
	t.Helper()
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, TopicAccount, TopicDeadLetter))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	producer, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...), kgo.RequiredAcks(kgo.AllISRAcks()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(producer.Close)
	client, err := kgo.NewClient(
		kgo.SeedBrokers(cluster.ListenAddrs()...),
		kgo.ConsumerGroup("profilesocial"),
		kgo.ConsumeTopics(TopicAccount),
		kgo.DisableAutoCommit(),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.FetchMaxWait(100*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	h := &consumerHarness{t: t, producer: producer, client: client, applier: &fakeApplier{}, dlq: &fakeDLQ{}}
	cfg.Group = "profilesocial"
	cfg.RetryBackoff, cfg.MaxBackoff = time.Millisecond, 5*time.Millisecond
	if cfg.Transient == nil {
		cfg.Transient = func(err error) bool { return errors.Is(err, errDBDown) }
	}
	log, v := slog.New(slog.DiscardHandler), newValidator(t)
	h.consumer = NewConsumer(client, h.dlq, map[string]RecordHandler{
		TopicAccount: NewAccountHandler(h.applier, v, log),
	}, nil, cfg, log)
	return h
}

func (h *consumerHarness) produce(rs ...*kgo.Record) {
	h.t.Helper()
	if err := h.producer.ProduceSync(context.Background(), rs...).FirstErr(); err != nil {
		h.t.Fatal(err)
	}
}

func (h *consumerHarness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan struct{})
	go func() {
		h.consumer.Run(ctx)
		close(h.done)
	}()
	h.t.Cleanup(h.stop)
}

func (h *consumerHarness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		h.t.Fatal("consumer did not stop after cancel")
	}
	h.cancel = nil
}

// committed is the group's committed offset of topic's partition 0: the
// offset the group resumes at. -1 means no entry; franz-go reports 0 for a
// partition it polled but never committed, which also resumes at the first
// record.
func (h *consumerHarness) committed(topic string) int64 {
	if o, ok := h.client.CommittedOffsets()[topic][0]; ok {
		return o.Offset
	}
	return -1
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// adminGranted is an AccountRoleChanged that grants ADMIN; invalidRoleChange
// fails protovalidate (an UNSPECIFIED role).
var (
	adminGranted = &eventsv1.AccountRoleChanged{
		Roles: []commonv1.Role{commonv1.Role_ROLE_ADMIN}, GrantedRoles: []commonv1.Role{commonv1.Role_ROLE_ADMIN},
	}
	invalidRoleChange = &eventsv1.AccountRoleChanged{Roles: []commonv1.Role{commonv1.Role_ROLE_UNSPECIFIED}}
)

func accountRecord(t *testing.T, user uuid.UUID, version int64, event proto.Message) *kgo.Record {
	t.Helper()
	var roles []commonv1.Role
	if rc, ok := event.(*eventsv1.AccountRoleChanged); ok {
		roles = rc.GetRoles()
	}
	r := record(t, TopicAccount, accountEvent(user, version, event, roles...), user.String())
	r.Topic = TopicAccount
	return r
}

func TestConsumerAppliesInOrderAndCommits(t *testing.T) {
	h := newConsumerHarness(t, ConsumerConfig{})
	user := uuid.New()
	h.produce(
		accountRecord(t, user, 1, &eventsv1.AccountCreated{}),
		accountRecord(t, user, 1, invalidRoleChange), // invalid: an UNSPECIFIED role -> poison
		accountRecord(t, user, 2, adminGranted),
	)
	h.start()
	waitFor(t, "two applied events", func() bool { applied, _ := h.applier.snapshot(); return len(applied) == 2 })
	waitFor(t, "the commit", func() bool { return h.committed(TopicAccount) == 3 })
	h.stop()

	applied, _ := h.applier.snapshot()
	if applied[0].Account.Version != 1 || applied[1].Account.Version != 2 || len(applied[1].Account.Roles) != 1 {
		t.Fatalf("applied = %+v, want versions 1 then 2", applied)
	}
	dead, _ := h.dlq.snapshot()
	if len(dead) != 1 {
		t.Fatalf("dead letters = %d, want 1 (the invalid AccountRoleChanged)", len(dead))
	}
	var dl eventsv1.DeadLetter
	if err := proto.Unmarshal(dead[0].Value, &dl); err != nil {
		t.Fatal(err)
	}
	if dl.GetSourceOffset() != 1 || dl.GetAttempts() != 1 || dl.GetConsumerGroup() != "profilesocial" ||
		dl.GetError().GetReason() != commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED || string(dead[0].Key) != user.String() {
		t.Fatalf("dead letter = %v", &dl)
	}
}

func TestConsumerDeadLettersUndecodableRecords(t *testing.T) {
	h := newConsumerHarness(t, ConsumerConfig{})
	user := uuid.New()
	h.produce(
		&kgo.Record{Topic: TopicAccount, Key: []byte(user.String()), Value: []byte("not protobuf \xff\xff")},
		accountRecord(t, user, 1, &eventsv1.AccountCreated{}),
	)
	h.start()
	waitFor(t, "the commit past both records", func() bool { return h.committed(TopicAccount) == 2 })
	h.stop()
	if applied, _ := h.applier.snapshot(); len(applied) != 1 {
		t.Fatalf("applied = %d, want 1", len(applied))
	}
	if dead, _ := h.dlq.snapshot(); len(dead) != 1 || dead[0].Topic != TopicDeadLetter {
		t.Fatalf("dead letters = %v", dead)
	}
}

// A database outage must neither dead-letter nor skip the record: it is
// retried beyond the retry budget until the database is back.
func TestConsumerRetriesOutagesWithoutDeadLettering(t *testing.T) {
	h := newConsumerHarness(t, ConsumerConfig{MaxAttempts: 2})
	h.applier.fail = func(_ domain.AccountUpdate, attempt int) error {
		if attempt <= 6 {
			return errDBDown
		}
		return nil
	}
	user := uuid.New()
	rec := accountRecord(t, user, 1, &eventsv1.AccountCreated{})
	h.produce(rec)
	h.start()
	waitFor(t, "the commit", func() bool { return h.committed(TopicAccount) == 1 })
	h.stop()
	applied, attempts := h.applier.snapshot()
	if len(applied) != 1 || attempts[applied[0].EventID] != 7 {
		t.Fatalf("applied = %d, attempts = %v; want one success after 6 outage failures", len(applied), attempts)
	}
	if dead, calls := h.dlq.snapshot(); len(dead) != 0 || calls != 0 {
		t.Fatalf("an outage was dead-lettered: %d records, %d calls", len(dead), calls)
	}
}

func TestConsumerDeadLettersWhenRetryBudgetIsSpent(t *testing.T) {
	h := newConsumerHarness(t, ConsumerConfig{MaxAttempts: 3})
	user := uuid.New()
	bad := accountRecord(t, user, 1, &eventsv1.AccountCreated{})
	var badID string
	for _, hd := range bad.Headers {
		if hd.Key == HeaderEventID {
			badID = string(hd.Value)
		}
	}
	h.applier.fail = func(u domain.AccountUpdate, _ int) error {
		if u.EventID.String() == badID {
			return errors.New(`violates check constraint "profiles_roles_check"`)
		}
		return nil
	}
	h.produce(bad, accountRecord(t, user, 2, adminGranted))
	h.start()
	waitFor(t, "the commit past both records", func() bool { return h.committed(TopicAccount) == 2 })
	h.stop()

	applied, attempts := h.applier.snapshot()
	if len(applied) != 1 || applied[0].Account.Version != 2 || attempts[uuid.MustParse(badID)] != 3 {
		t.Fatalf("applied = %+v, attempts = %v", applied, attempts)
	}
	dead, _ := h.dlq.snapshot()
	if len(dead) != 1 {
		t.Fatalf("dead letters = %d, want 1", len(dead))
	}
	var dl eventsv1.DeadLetter
	if err := proto.Unmarshal(dead[0].Value, &dl); err != nil {
		t.Fatal(err)
	}
	if dl.GetAttempts() != 3 || dl.GetSourceOffset() != 0 || dl.GetError().GetReason() != commonv1.ErrorReason_ERROR_REASON_INTERNAL ||
		dl.GetError().GetMetadata()["failure"] != "retries_exhausted" {
		t.Fatalf("dead letter = %v", &dl)
	}
}

// Stopping during an outage leaves the record in progress uncommitted, so it
// is delivered again after a restart.
func TestConsumerStopsWithoutCommittingTheRecordInProgress(t *testing.T) {
	h := newConsumerHarness(t, ConsumerConfig{})
	user := uuid.New()
	second := accountRecord(t, user, 2, adminGranted)
	h.applier.fail = func(u domain.AccountUpdate, _ int) error {
		if u.Account.Version == 2 {
			return errDBDown
		}
		return nil
	}
	h.produce(accountRecord(t, user, 1, &eventsv1.AccountCreated{}), second)
	h.start()
	waitFor(t, "retries of the second record", func() bool {
		_, attempts := h.applier.snapshot()
		for _, n := range attempts {
			if n >= 3 {
				return true
			}
		}
		return false
	})
	h.stop()
	if got := h.committed(TopicAccount); got != 1 {
		t.Fatalf("committed offset = %d, want 1 (only the first record)", got)
	}
	if dead, _ := h.dlq.snapshot(); len(dead) != 0 {
		t.Fatalf("dead letters = %d, want none", len(dead))
	}
}

// A dead letter that cannot be published keeps the record uncommitted.
func TestConsumerRetriesDeadLetterPublish(t *testing.T) {
	h := newConsumerHarness(t, ConsumerConfig{})
	h.dlq.failing = true
	h.produce(&kgo.Record{Topic: TopicAccount, Key: []byte("k"), Value: []byte{0xff}})
	h.start()
	waitFor(t, "dead-letter publish retries", func() bool { _, calls := h.dlq.snapshot(); return calls >= 3 })
	h.stop()
	if got := h.committed(TopicAccount); got > 0 {
		t.Fatalf("committed offset = %d, want nothing past the start", got)
	}
}

// Both streams in one group, as serve.go wires them: account records are
func TestPermanent(t *testing.T) {
	base := errors.New("bad")
	if Permanent(nil) != nil || !IsPermanent(Permanent(base)) || IsPermanent(base) || !errors.Is(Permanent(base), base) {
		t.Fatal("Permanent / IsPermanent disagree")
	}
}
