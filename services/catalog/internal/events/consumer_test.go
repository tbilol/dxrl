package events

import (
	"context"
	"errors"
	"fmt"
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

	"github.com/tbilol/dxrl/services/catalog/internal/domain"
)

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
// topic, wired like serve.go: group "catalog" on the transaction stream.
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
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, TopicTransaction, TopicDeadLetter))
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
		kgo.ConsumerGroup("catalog"),
		kgo.ConsumeTopics(TopicTransaction),
		kgo.DisableAutoCommit(),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.FetchMaxWait(100*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	h := &consumerHarness{t: t, producer: producer, client: client, applier: &fakeApplier{}, dlq: &fakeDLQ{}}
	cfg.Group = "catalog"
	cfg.RetryBackoff, cfg.MaxBackoff = time.Millisecond, 5*time.Millisecond
	if cfg.Transient == nil {
		cfg.Transient = func(err error) bool { return errors.Is(err, errDBDown) }
	}
	log := slog.New(slog.DiscardHandler)
	h.consumer = NewConsumer(client, h.dlq, map[string]RecordHandler{
		TopicTransaction: NewTransactionHandler(h.applier, newValidator(t), log),
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

// committed is the group's committed offset of the transaction topic's
// partition 0: the offset the group resumes at. -1 means no entry; franz-go
// reports 0 for a partition it polled but never committed, which also resumes
// at the first record.
func (h *consumerHarness) committed() int64 {
	if o, ok := h.client.CommittedOffsets()[TopicTransaction][0]; ok {
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

func deadLetter(t *testing.T, r *kgo.Record) *eventsv1.DeadLetter {
	t.Helper()
	var dl eventsv1.DeadLetter
	if err := proto.Unmarshal(r.Value, &dl); err != nil {
		t.Fatal(err)
	}
	return &dl
}

func TestConsumerAppliesInOrderAndCommits(t *testing.T) {
	h := newConsumerHarness(t, ConsumerConfig{})
	tx := newTestTx()
	poison := transactionEvent(tx, 1)
	poison.Metadata.AggregateVersion = 3 // contradicts the snapshot (version 2)
	h.produce(
		txStep(t, tx, 0),
		txRecord(t, poison, tx.id.String()),
		txStep(t, tx, 1),
		txStep(t, tx, 5),
	)
	h.start()
	waitFor(t, "three applied events", func() bool { calls, _, _ := h.applier.snapshot(); return len(calls) == 3 })
	waitFor(t, "the commit", func() bool { return h.committed() == 4 })
	h.stop()

	calls, outcomes, _ := h.applier.snapshot()
	for i, want := range []struct {
		version int64
		state   eventsv1.TransactionState
	}{{1, domain.StateAwaitingPayment}, {2, domain.StatePaymentHeld}, {6, domain.StateCompleted}} {
		if c := calls[i]; c.Version != want.version || c.State != want.state || c.TransactionID != tx.id ||
			c.Topic != TopicTransaction || outcomes[i] != domain.Applied {
			t.Fatalf("call %d = %+v, %s; want version %d, %s, applied", i+1, c, outcomes[i], want.version, want.state)
		}
	}
	dead, _ := h.dlq.snapshot()
	if len(dead) != 1 || dead[0].Topic != TopicDeadLetter || string(dead[0].Key) != tx.id.String() {
		t.Fatalf("dead letters = %v, want the poison record, keyed by the transaction id", dead)
	}
	dl := deadLetter(t, dead[0])
	if dl.GetSourceTopic() != TopicTransaction || dl.GetSourceOffset() != 1 || dl.GetAttempts() != 1 ||
		dl.GetConsumerGroup() != "catalog" || dl.GetError().GetReason() != commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED ||
		dl.GetError().GetMetadata()["failure"] != "poison" || dl.GetError().GetDomain() != ProducerName {
		t.Fatalf("dead letter = %v", dl)
	}
}

func TestConsumerDeadLettersUndecodableRecords(t *testing.T) {
	h := newConsumerHarness(t, ConsumerConfig{})
	tx := newTestTx()
	h.produce(
		&kgo.Record{Topic: TopicTransaction, Key: []byte(tx.id.String()), Value: []byte("not protobuf \xff\xff")},
		txStep(t, tx, 0),
	)
	h.start()
	waitFor(t, "the commit past both records", func() bool { return h.committed() == 2 })
	h.stop()
	if calls, _, _ := h.applier.snapshot(); len(calls) != 1 {
		t.Fatalf("applied = %d, want 1", len(calls))
	}
	if dead, _ := h.dlq.snapshot(); len(dead) != 1 || dead[0].Topic != TopicDeadLetter ||
		deadLetter(t, dead[0]).GetSourceOffset() != 0 {
		t.Fatalf("dead letters = %v", dead)
	}
}

// An event that contradicts catalog's data (the service's
// ErrInconsistentEvent) is dead-lettered at once, like a poison record, and
// the consumer moves on.
func TestConsumerDeadLettersInconsistentEvents(t *testing.T) {
	h := newConsumerHarness(t, ConsumerConfig{MaxAttempts: 5})
	bad, good := newTestTx(), newTestTx()
	h.applier.fail = func(u domain.TransactionUpdate, _ int) error {
		if u.TransactionID == bad.id {
			return fmt.Errorf("apply: %w: listing %s does not exist", domain.ErrInconsistentEvent, u.ListingID)
		}
		return nil
	}
	badRec := txStep(t, bad, 0)
	h.produce(badRec, txStep(t, good, 0))
	h.start()
	waitFor(t, "the commit past both records", func() bool { return h.committed() == 2 })
	h.stop()

	calls, _, attempts := h.applier.snapshot()
	if len(calls) != 1 || calls[0].TransactionID != good.id || attempts[eventIDOf(badRec)] != 1 {
		t.Fatalf("calls = %+v, attempts = %v; want the good record applied and one attempt of the bad one", calls, attempts)
	}
	dead, _ := h.dlq.snapshot()
	if len(dead) != 1 || string(dead[0].Key) != bad.id.String() {
		t.Fatalf("dead letters = %v", dead)
	}
	if dl := deadLetter(t, dead[0]); dl.GetAttempts() != 1 || dl.GetConsumerGroup() != "catalog" ||
		dl.GetSourceTopic() != TopicTransaction || dl.GetError().GetMetadata()["failure"] != "poison" {
		t.Fatalf("dead letter = %v", dl)
	}
}

// A database outage must neither dead-letter nor skip the record: it is
// retried beyond the retry budget until the database is back.
func TestConsumerRetriesOutagesWithoutDeadLettering(t *testing.T) {
	h := newConsumerHarness(t, ConsumerConfig{MaxAttempts: 2})
	h.applier.fail = func(_ domain.TransactionUpdate, attempt int) error {
		if attempt <= 6 {
			return errDBDown
		}
		return nil
	}
	tx := newTestTx()
	h.produce(txStep(t, tx, 0))
	h.start()
	waitFor(t, "the commit", func() bool { return h.committed() == 1 })
	h.stop()
	calls, _, attempts := h.applier.snapshot()
	if len(calls) != 1 || attempts[calls[0].EventID] != 7 {
		t.Fatalf("applied = %d, attempts = %v; want one success after 6 outage failures", len(calls), attempts)
	}
	if dead, n := h.dlq.snapshot(); len(dead) != 0 || n != 0 {
		t.Fatalf("an outage was dead-lettered: %d records, %d calls", len(dead), n)
	}
}

func TestConsumerDeadLettersWhenRetryBudgetIsSpent(t *testing.T) {
	h := newConsumerHarness(t, ConsumerConfig{MaxAttempts: 3})
	tx := newTestTx()
	bad := txStep(t, tx, 0)
	h.applier.fail = func(u domain.TransactionUpdate, _ int) error {
		if u.EventID == eventIDOf(bad) {
			return errors.New(`new row violates check constraint "listing_transactions_quantity_grams_check"`)
		}
		return nil
	}
	h.produce(bad, txStep(t, tx, 1))
	h.start()
	waitFor(t, "the commit past both records", func() bool { return h.committed() == 2 })
	h.stop()

	calls, _, attempts := h.applier.snapshot()
	if len(calls) != 1 || calls[0].Version != 2 || attempts[eventIDOf(bad)] != 3 {
		t.Fatalf("calls = %+v, attempts = %v", calls, attempts)
	}
	dead, _ := h.dlq.snapshot()
	if len(dead) != 1 {
		t.Fatalf("dead letters = %d, want 1", len(dead))
	}
	if dl := deadLetter(t, dead[0]); dl.GetAttempts() != 3 || dl.GetSourceOffset() != 0 ||
		dl.GetError().GetReason() != commonv1.ErrorReason_ERROR_REASON_INTERNAL ||
		dl.GetError().GetMetadata()["failure"] != "retries_exhausted" || dl.GetConsumerGroup() != "catalog" ||
		dl.GetSourceTopic() != TopicTransaction {
		t.Fatalf("dead letter = %v", dl)
	}
}

// Stopping during an outage leaves the record in progress uncommitted, so it
// is delivered again after a restart.
func TestConsumerStopsWithoutCommittingTheRecordInProgress(t *testing.T) {
	h := newConsumerHarness(t, ConsumerConfig{})
	tx := newTestTx()
	h.applier.fail = func(u domain.TransactionUpdate, _ int) error {
		if u.Version == 2 {
			return errDBDown
		}
		return nil
	}
	h.produce(txStep(t, tx, 0), txStep(t, tx, 1))
	h.start()
	waitFor(t, "retries of the second record", func() bool {
		_, _, attempts := h.applier.snapshot()
		for _, n := range attempts {
			if n >= 3 {
				return true
			}
		}
		return false
	})
	h.stop()
	if got := h.committed(); got != 1 {
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
	h.produce(&kgo.Record{Topic: TopicTransaction, Key: []byte("k"), Value: []byte{0xff}})
	h.start()
	waitFor(t, "dead-letter publish retries", func() bool { _, calls := h.dlq.snapshot(); return calls >= 3 })
	h.stop()
	if got := h.committed(); got > 0 {
		t.Fatalf("committed offset = %d, want nothing past the start", got)
	}
}

// A redelivered record is handed over again and the applier's inbox absorbs
// it; an older event after a newer one is stale. Both are processed, so the
// offsets move on.
func TestConsumerHandsRedeliveriesToTheApplier(t *testing.T) {
	h := newConsumerHarness(t, ConsumerConfig{})
	tx := newTestTx()
	created, held := txStep(t, tx, 0), txStep(t, tx, 1)
	h.produce(created, held, created, txStep(t, tx, 0))
	h.start()
	waitFor(t, "the commit", func() bool { return h.committed() == 4 })
	h.stop()
	_, outcomes, _ := h.applier.snapshot()
	want := []domain.ApplyOutcome{domain.Applied, domain.Applied, domain.Duplicate, domain.Stale}
	if fmt.Sprint(outcomes) != fmt.Sprint(want) {
		t.Fatalf("outcomes = %v, want %v", outcomes, want)
	}
	if dead, _ := h.dlq.snapshot(); len(dead) != 0 {
		t.Fatalf("dead letters = %d, want none", len(dead))
	}
}

func TestPermanent(t *testing.T) {
	base := errors.New("bad")
	if Permanent(nil) != nil || !IsPermanent(Permanent(base)) || IsPermanent(base) || !errors.Is(Permanent(base), base) ||
		!IsPermanent(fmt.Errorf("wrapped: %w", Permanent(base))) {
		t.Fatal("Permanent / IsPermanent disagree")
	}
	if id := uuid.New(); !IsPermanent(Permanent(fmt.Errorf("%w: %s", domain.ErrInconsistentEvent, id))) {
		t.Fatal("a permanent inconsistency is not permanent")
	}
}
