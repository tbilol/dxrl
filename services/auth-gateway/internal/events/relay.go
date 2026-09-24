package events

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// OutboxStore is the relay's view of the outbox table.
type OutboxStore interface {
	// ProcessOutbox locks up to limit unpublished messages in outbox order,
	// skipping rows other relays hold, and calls publish with them. When
	// publish succeeds the messages are marked published; otherwise their
	// attempt count and last error are recorded and publish's error is
	// returned. It returns how many messages were handed to publish.
	ProcessOutbox(ctx context.Context, limit int, publish func(context.Context, []Message) error) (int, error)
	// PurgePublished deletes messages published before the cutoff.
	PurgePublished(ctx context.Context, before time.Time) (int64, error)
}

// Producer is the part of *kgo.Client the relay uses.
type Producer interface {
	ProduceSync(ctx context.Context, rs ...*kgo.Record) kgo.ProduceResults
}

// RelayConfig tunes the relay loop.
type RelayConfig struct {
	Interval       time.Duration // idle poll interval
	BatchSize      int
	MaxBackoff     time.Duration // cap of the error backoff
	PublishTimeout time.Duration // per batch
	Retention      time.Duration // published rows older than this are purged
}

// Relay publishes outbox messages to Kafka: at-least-once, in outbox order
// per relay, with an exponential backoff while Kafka or Postgres fail.
type Relay struct {
	store    OutboxStore
	producer Producer
	cfg      RelayConfig
	log      *slog.Logger
}

// NewRelay returns a relay. The producer should be an idempotent client with
// acks=all (the franz-go defaults).
func NewRelay(store OutboxStore, producer Producer, cfg RelayConfig, log *slog.Logger) *Relay {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.MaxBackoff < cfg.Interval {
		cfg.MaxBackoff = cfg.Interval
	}
	if cfg.PublishTimeout <= 0 {
		cfg.PublishTimeout = 15 * time.Second
	}
	return &Relay{store: store, producer: producer, cfg: cfg, log: log}
}

// Run relays until ctx is canceled. A batch in flight when ctx is canceled is
// allowed to finish (bounded by PublishTimeout).
func (r *Relay) Run(ctx context.Context) {
	var backoff time.Duration
	lastPurge := time.Time{}
	for {
		batchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.PublishTimeout+5*time.Second)
		n, err := r.RunOnce(batchCtx)
		cancel()

		wait := r.cfg.Interval
		switch {
		case err != nil:
			backoff = min(max(2*backoff, r.cfg.Interval), r.cfg.MaxBackoff)
			wait = backoff
			r.log.WarnContext(ctx, "outbox relay failed; backing off", "error", err, "backoff", backoff)
		case n >= r.cfg.BatchSize:
			backoff, wait = 0, 0 // more is waiting
		default:
			backoff = 0
		}
		if ctx.Err() != nil {
			return
		}
		if r.cfg.Retention > 0 && time.Since(lastPurge) > time.Hour {
			lastPurge = time.Now()
			if purged, err := r.store.PurgePublished(ctx, lastPurge.Add(-r.cfg.Retention)); err != nil {
				r.log.WarnContext(ctx, "outbox purge failed", "error", err)
			} else if purged > 0 {
				r.log.InfoContext(ctx, "outbox purged", "rows", purged)
			}
		}
		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}
}

// RunOnce publishes one batch and returns its size.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	return r.store.ProcessOutbox(ctx, r.cfg.BatchSize, r.publish)
}

func (r *Relay) publish(ctx context.Context, msgs []Message) error {
	ctx, cancel := context.WithTimeout(ctx, r.cfg.PublishTimeout)
	defer cancel()
	records := make([]*kgo.Record, len(msgs))
	for i, m := range msgs {
		records[i] = Record(m)
	}
	if err := r.producer.ProduceSync(ctx, records...).FirstErr(); err != nil {
		return err
	}
	r.log.DebugContext(ctx, "outbox batch published", "messages", len(msgs))
	return nil
}

// Record converts an outbox message into a Kafka record.
func Record(m Message) *kgo.Record {
	keys := make([]string, 0, len(m.Headers))
	for k := range m.Headers {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	headers := make([]kgo.RecordHeader, len(keys))
	for i, k := range keys {
		headers[i] = kgo.RecordHeader{Key: k, Value: []byte(m.Headers[k])}
	}
	return &kgo.Record{
		Topic:   m.Topic,
		Key:     []byte(m.Key),
		Value:   m.Value,
		Headers: headers,
	}
}
