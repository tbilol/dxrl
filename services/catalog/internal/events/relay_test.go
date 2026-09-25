package events

import (
	"bytes"
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
)

// memOutbox is an in-memory OutboxStore.
type memOutbox struct {
	mu        sync.Mutex
	pending   []Message
	published []Message
	attempts  map[uuid.UUID]int
}

func newMemOutbox(msgs ...Message) *memOutbox {
	return &memOutbox{pending: msgs, attempts: map[uuid.UUID]int{}}
}

func (s *memOutbox) ProcessOutbox(ctx context.Context, limit int, publish func(context.Context, []Message) error) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch := s.pending[:min(limit, len(s.pending))]
	if len(batch) == 0 {
		return 0, nil
	}
	if err := publish(ctx, batch); err != nil {
		for _, m := range batch {
			s.attempts[m.ID]++
		}
		return len(batch), err
	}
	s.published = append(s.published, batch...)
	s.pending = s.pending[len(batch):]
	return len(batch), nil
}

func (s *memOutbox) PurgePublished(context.Context, time.Time) (int64, error) { return 0, nil }

func (s *memOutbox) counts() (pending, published int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending), len(s.published)
}

type failingProducer struct{}

func (failingProducer) ProduceSync(_ context.Context, rs ...*kgo.Record) kgo.ProduceResults {
	res := make(kgo.ProduceResults, len(rs))
	for i, r := range rs {
		res[i] = kgo.ProduceResult{Record: r, Err: errors.New("broker down")}
	}
	return res
}

func messages(t *testing.T, n int) []Message {
	t.Helper()
	out := make([]Message, n)
	for i := range out {
		m, err := NewListingEvent(ListingChange{
			Listing:       sampleListing(),
			Event:         &eventsv1.ListingCreated{},
			OccurredAt:    testNow,
			CorrelationID: "req-1",
			Actor:         &commonv1.Actor{Kind: &commonv1.Actor_Service{Service: ProducerName}},
		})
		if err != nil {
			t.Fatal(err)
		}
		out[i] = m
	}
	return out
}

func newKafka(t *testing.T) (*kfake.Cluster, *kgo.Client) {
	t.Helper()
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(3, TopicListing))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	producer, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...), kgo.RequiredAcks(kgo.AllISRAcks()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(producer.Close)
	return cluster, producer
}

func consumeAll(t *testing.T, cluster *kfake.Cluster, want int, topics ...string) map[string]*kgo.Record {
	t.Helper()
	if len(topics) == 0 {
		topics = []string{TopicListing}
	}
	consumer, err := kgo.NewClient(
		kgo.SeedBrokers(cluster.ListenAddrs()...),
		kgo.ConsumeTopics(topics...),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := map[string]*kgo.Record{}
	for len(got) < want {
		fetches := consumer.PollFetches(ctx)
		if ctx.Err() != nil {
			t.Fatalf("consumed %d of %d records before the timeout", len(got), want)
		}
		fetches.EachRecord(func(r *kgo.Record) {
			for _, h := range r.Headers {
				if h.Key == HeaderEventID {
					got[string(h.Value)] = r
				}
			}
		})
	}
	return got
}

func TestRelayPublishesOutboxToKafka(t *testing.T) {
	cluster, producer := newKafka(t)
	msgs := messages(t, 3)
	store := newMemOutbox(msgs...)
	relay := NewRelay(store, producer, RelayConfig{BatchSize: 2}, slog.New(slog.DiscardHandler))

	if n, err := relay.RunOnce(context.Background()); err != nil || n != 2 {
		t.Fatalf("first batch: n=%d err=%v", n, err)
	}
	if n, err := relay.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("second batch: n=%d err=%v", n, err)
	}
	if pending, published := store.counts(); pending != 0 || published != 3 {
		t.Fatalf("pending=%d published=%d", pending, published)
	}

	records := consumeAll(t, cluster, 3)
	for _, m := range msgs {
		r := records[m.ID.String()]
		if r == nil {
			t.Fatalf("event %s not in Kafka", m.ID)
		}
		if string(r.Key) != m.Key || !bytes.Equal(r.Value, m.Value) || r.Topic != TopicListing {
			t.Fatalf("record %s: key=%s topic=%s", m.ID, r.Key, r.Topic)
		}
		headers := map[string]string{}
		for _, h := range r.Headers {
			headers[h.Key] = string(h.Value)
		}
		for k, v := range m.Headers {
			if headers[k] != v {
				t.Fatalf("record header %s = %q, want %q", k, headers[k], v)
			}
		}
		if headers[HeaderContentType] != ContentType || headers[HeaderEventType] != ListingCreatedType {
			t.Fatalf("headers = %v", headers)
		}
	}
}

func TestRelayKeepsMessagesWhenKafkaFails(t *testing.T) {
	msgs := messages(t, 2)
	store := newMemOutbox(msgs...)
	relay := NewRelay(store, failingProducer{}, RelayConfig{}, slog.New(slog.DiscardHandler))

	n, err := relay.RunOnce(context.Background())
	if err == nil || n != 2 {
		t.Fatalf("n=%d err=%v, want a publish error", n, err)
	}
	if pending, published := store.counts(); pending != 2 || published != 0 || store.attempts[msgs[0].ID] != 1 {
		t.Fatalf("pending=%d published=%d attempts=%v", pending, published, store.attempts)
	}
}

func TestRelayRunLoop(t *testing.T) {
	_, producer := newKafka(t)
	store := newMemOutbox(messages(t, 5)...)
	relay := NewRelay(store, producer, RelayConfig{Interval: 10 * time.Millisecond, BatchSize: 2}, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		relay.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, published := store.counts(); published == 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("relay did not publish everything")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not stop after cancel")
	}
}
