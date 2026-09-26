package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// RecordHandler processes one record of the topic it is registered for. It
// returns nil once the record is processed (duplicates and stale events
// included), a Permanent error for a poison record, and any other error to
// have the record retried.
type RecordHandler interface {
	HandleRecord(ctx context.Context, r *kgo.Record) error
}

// GroupConsumer is the part of *kgo.Client the Consumer uses: a group
// consumer created with kgo.DisableAutoCommit.
type GroupConsumer interface {
	PollRecords(ctx context.Context, maxPollRecords int) kgo.Fetches
	CommitRecords(ctx context.Context, rs ...*kgo.Record) error
}

// InboxStore is the consumer's view of the de-duplication table.
type InboxStore interface {
	// PurgeProcessedEvents deletes inbox rows processed before the cutoff.
	PurgeProcessedEvents(ctx context.Context, before time.Time) (int64, error)
}

// PermanentError marks a record that can never be processed (a poison
// record: undecodable, failing protovalidate, or contradicting catalog's
// data). It is dead-lettered at once, without retries.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }

func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent wraps err as a PermanentError (nil stays nil).
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether err is (or wraps) a PermanentError.
func IsPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}

// ConsumerConfig tunes the consumer loop.
type ConsumerConfig struct {
	// Group is the consumer group, also DeadLetter.consumer_group.
	Group string
	// MaxAttempts is the retry budget of a record that fails for a reason
	// that is neither poison nor an outage; then it is dead-lettered.
	MaxAttempts int
	// RetryBackoff is the first retry delay, doubled per attempt up to
	// MaxBackoff.
	RetryBackoff time.Duration
	MaxBackoff   time.Duration
	// HandleTimeout bounds one attempt. An attempt in flight at shutdown may
	// finish within it.
	HandleTimeout time.Duration
	// BatchSize is the maximum number of records per poll.
	BatchSize int
	// Transient reports an infrastructure outage (Postgres down, a timeout):
	// such failures are retried with backoff for as long as they last, never
	// count against MaxAttempts and never dead-letter the record, so an
	// outage does not dead-letter the stream. nil: none is transient.
	Transient func(error) bool
	// InboxRetention: inbox rows older than this are purged hourly (0 keeps
	// them).
	InboxRetention time.Duration
}

// Consumer reads its topics in a consumer group and hands each record to the
// topic's RecordHandler, one at a time in partition order.
//
// Delivery is at-least-once: a record's offset is committed only after it
// was processed or dead-lettered, after each poll batch. Handlers must be
// idempotent (the transaction handler de-duplicates on event_id). A
// rebalance or a crash between processing and the commit delivers records
// again.
//
// Failures:
//
//   - Permanent (poison) errors: a DeadLetter at once (attempts 1).
//   - Transient errors (ConsumerConfig.Transient): retried with exponential
//     backoff until they stop; the record is neither dead-lettered nor
//     skipped, and the partition does not advance.
//   - Any other error: retried with backoff up to MaxAttempts, then a
//     DeadLetter, and the consumer moves on.
//
// A DeadLetter that cannot be published (Kafka down) is retried until it is;
// until then the record's offset is not committed.
type Consumer struct {
	client   GroupConsumer
	dlq      Producer
	handlers map[string]RecordHandler
	inbox    InboxStore
	cfg      ConsumerConfig
	log      *slog.Logger
	now      func() time.Time
}

// NewConsumer returns a consumer. client must be subscribed to exactly the
// topics of handlers; dlq publishes dead letters (the relay's producer is
// fine). inbox may be nil (no purge).
func NewConsumer(client GroupConsumer, dlq Producer, handlers map[string]RecordHandler, inbox InboxStore,
	cfg ConsumerConfig, log *slog.Logger,
) *Consumer {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = 500 * time.Millisecond
	}
	if cfg.MaxBackoff < cfg.RetryBackoff {
		cfg.MaxBackoff = cfg.RetryBackoff
	}
	if cfg.HandleTimeout <= 0 {
		cfg.HandleTimeout = 10 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	return &Consumer{client: client, dlq: dlq, handlers: handlers, inbox: inbox, cfg: cfg, log: log, now: time.Now}
}

// Run consumes until ctx is canceled. Records processed before that are
// committed on the way out; the record in progress is not.
func (c *Consumer) Run(ctx context.Context) {
	var (
		lastPurge time.Time
		idle      time.Duration // backoff after polls that returned only errors
	)
	for {
		fetches := c.client.PollRecords(ctx, c.cfg.BatchSize)
		if ctx.Err() != nil || fetches.IsClientClosed() {
			return
		}
		fetchFailed := false
		fetches.EachError(func(topic string, partition int32, err error) {
			fetchFailed = true
			c.log.WarnContext(ctx, "kafka fetch failed", "topic", topic, "partition", partition, "error", err)
		})

		var done []*kgo.Record
		stopped := false
		for it := fetches.RecordIter(); !it.Done(); {
			r := it.Next()
			if err := c.process(ctx, r); err != nil {
				stopped = true // ctx ended: r stays uncommitted
				break
			}
			done = append(done, r)
		}
		c.commit(ctx, done)
		if stopped || ctx.Err() != nil {
			return
		}
		c.purge(ctx, &lastPurge)

		if fetchFailed && len(done) == 0 {
			idle = min(max(2*idle, c.cfg.RetryBackoff), c.cfg.MaxBackoff)
			if !sleep(ctx, idle) {
				return
			}
		} else {
			idle = 0
		}
	}
}

// process handles r until it is processed or dead-lettered. It returns an
// error only when ctx ends first; r must not be committed then.
func (c *Consumer) process(ctx context.Context, r *kgo.Record) error {
	h := c.handlers[r.Topic]
	if h == nil {
		c.log.WarnContext(ctx, "record from a topic without a handler; skipped", recordAttrs(r)...)
		return nil
	}
	var (
		attempts, failures int
		firstFailed        time.Time
		backoff            time.Duration
	)
	for {
		attempts++
		err := c.handle(ctx, h, r)
		if err == nil {
			return nil
		}
		if firstFailed.IsZero() {
			firstFailed = c.now()
		}
		permanent := IsPermanent(err)
		transient := !permanent && c.cfg.Transient != nil && c.cfg.Transient(err)
		if !transient {
			failures++
		}
		if permanent || failures >= c.cfg.MaxAttempts {
			return c.deadLetter(ctx, r, err, attempts, firstFailed)
		}
		backoff = min(max(2*backoff, c.cfg.RetryBackoff), c.cfg.MaxBackoff)
		c.log.WarnContext(ctx, "record failed; retrying", append(recordAttrs(r),
			"attempt", attempts, "transient", transient, "backoff", backoff, "error", err)...)
		if !sleep(ctx, backoff) {
			return ctx.Err()
		}
	}
}

// handle runs one attempt. It may outlive ctx by up to HandleTimeout, so a
// record in flight at shutdown is not cut off half-way; a panic counts as a
// failed attempt.
func (c *Consumer) handle(ctx context.Context, h RecordHandler, r *kgo.Record) (err error) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.HandleTimeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic in record handler: %v", p)
		}
	}()
	return h.HandleRecord(actx, r)
}

// deadLetter publishes r as a DeadLetter, retrying until Kafka takes it or
// ctx ends.
func (c *Consumer) deadLetter(ctx context.Context, r *kgo.Record, cause error, attempts int, firstFailed time.Time) error {
	rec, err := NewDeadLetter(r, c.cfg.Group, cause, attempts, firstFailed, c.now())
	if err != nil {
		// Cannot happen with a non-empty group; never block the partition on it.
		c.log.ErrorContext(ctx, "cannot build a dead letter; record skipped", append(recordAttrs(r),
			"error", err, "cause", cause)...)
		return nil
	}
	var backoff time.Duration
	for {
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		err := c.dlq.ProduceSync(pctx, rec).FirstErr()
		cancel()
		if err == nil {
			c.log.ErrorContext(ctx, "record dead-lettered", append(recordAttrs(r),
				"dead_letter_topic", rec.Topic, "attempts", attempts, "poison", IsPermanent(cause), "error", cause)...)
			return nil
		}
		backoff = min(max(2*backoff, c.cfg.RetryBackoff), c.cfg.MaxBackoff)
		c.log.WarnContext(ctx, "dead-letter publish failed; retrying", append(recordAttrs(r),
			"backoff", backoff, "error", err)...)
		if !sleep(ctx, backoff) {
			return ctx.Err()
		}
	}
}

// commit commits the processed records' offsets (the highest per partition).
func (c *Consumer) commit(ctx context.Context, rs []*kgo.Record) {
	if len(rs) == 0 {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := c.client.CommitRecords(cctx, rs...); err != nil {
		c.log.WarnContext(ctx, "offset commit failed; the records will be delivered again (processing is idempotent)",
			"records", len(rs), "error", err)
	}
}

// purge deletes old inbox rows at most once an hour.
func (c *Consumer) purge(ctx context.Context, last *time.Time) {
	if c.inbox == nil || c.cfg.InboxRetention <= 0 || time.Since(*last) < time.Hour {
		return
	}
	*last = time.Now()
	if n, err := c.inbox.PurgeProcessedEvents(ctx, last.Add(-c.cfg.InboxRetention)); err != nil {
		c.log.WarnContext(ctx, "inbox purge failed", "error", err)
	} else if n > 0 {
		c.log.InfoContext(ctx, "inbox purged", "rows", n)
	}
}

func recordAttrs(r *kgo.Record) []any {
	return []any{"topic", r.Topic, "partition", r.Partition, "offset", r.Offset,
		"key", string(r.Key), "event_id", header(r, HeaderEventID)}
}

// sleep waits d or until ctx ends; false means ctx ended.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
