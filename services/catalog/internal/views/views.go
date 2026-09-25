// Package views counts listing views off the request path. GetListing calls
// Record, which only queues the view (never blocking, never failing the
// read); a background loop drains the queue every interval, drops repeat
// views of the same viewer within the de-duplication window (Redis, when
// configured), and adds the rest to listings.views_count in one statement per
// batch, without bumping the listing version.
//
// Counting is best effort: a full queue, a Redis failure (every view then
// counts: fail open) or a failed write loses or over-counts a few views, and
// nothing else.
package views

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// KeyPrefix namespaces the de-duplication keys in a shared Redis.
const KeyPrefix = "catalog:views:"

// Store adds view counts to listings (postgres.Store).
type Store interface {
	AddViews(ctx context.Context, counts map[uuid.UUID]int64) error
}

// Deduper reports, for each key, whether it was not seen within its window
// (and remembers it).
type Deduper interface {
	First(ctx context.Context, keys []string) ([]bool, error)
}

// Config tunes a Counter.
type Config struct {
	FlushInterval time.Duration // how often queued views are written
	BufferSize    int           // queued views; more are dropped until the next flush
	WriteTimeout  time.Duration // per flush
}

type view struct{ listing, viewer uuid.UUID }

// Counter queues views and writes them in batches.
type Counter struct {
	queue   chan view
	store   Store
	dedup   Deduper // nil: every view counts
	cfg     Config
	log     *slog.Logger
	dropped atomic.Int64
}

// NewCounter returns a counter. dedup may be nil (no de-duplication).
func NewCounter(store Store, dedup Deduper, cfg Config, log *slog.Logger) *Counter {
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = time.Second
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 10_000
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 5 * time.Second
	}
	return &Counter{queue: make(chan view, cfg.BufferSize), store: store, dedup: dedup, cfg: cfg, log: log}
}

// Record queues a view of listing by viewer (uuid.Nil: anonymous). It never
// blocks: when the queue is full the view is dropped (and reported at the
// next flush).
func (c *Counter) Record(listing, viewer uuid.UUID) {
	select {
	case c.queue <- view{listing: listing, viewer: viewer}:
	default:
		c.dropped.Add(1)
	}
}

// Run flushes every FlushInterval until ctx is canceled, then flushes what
// is still queued once more (bounded by WriteTimeout).
func (c *Counter) Run(ctx context.Context) {
	t := time.NewTicker(c.cfg.FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			c.Flush(context.WithoutCancel(ctx))
			return
		case <-t.C:
			c.Flush(ctx)
		}
	}
}

// Flush writes the queued views: at most one queue's worth, so a steady
// stream of views cannot keep it busy forever.
func (c *Counter) Flush(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
	defer cancel()
	if n := c.dropped.Swap(0); n > 0 {
		c.log.WarnContext(ctx, "listing views dropped: the view queue was full", "views", n)
	}
	batch := c.drain()
	if len(batch) == 0 {
		return
	}
	counts := c.count(ctx, batch)
	if len(counts) == 0 {
		return
	}
	err := c.store.AddViews(ctx, counts)
	if err != nil { // one retry: a lost deadlock or a dropped connection
		err = c.store.AddViews(ctx, counts)
	}
	if err != nil {
		c.log.WarnContext(ctx, "listing views lost", "listings", len(counts), "error", err)
	}
}

func (c *Counter) drain() []view {
	var out []view
	for len(out) < c.cfg.BufferSize {
		select {
		case v := <-c.queue:
			out = append(out, v)
		default:
			return out
		}
	}
	return out
}

// count aggregates views per listing, skipping the repeat views the deduper
// reports. Anonymous views always count. When the deduper fails, every view
// counts (fail open).
func (c *Counter) count(ctx context.Context, batch []view) map[uuid.UUID]int64 {
	first := make([]bool, len(batch))
	for i := range first {
		first[i] = true
	}
	if c.dedup != nil {
		var (
			keys []string
			idx  []int
		)
		for i, v := range batch {
			if v.viewer != uuid.Nil {
				keys = append(keys, v.listing.String()+":"+v.viewer.String())
				idx = append(idx, i)
			}
		}
		if len(keys) > 0 {
			seen, err := c.dedup.First(ctx, keys)
			if err != nil || len(seen) != len(keys) {
				c.log.WarnContext(ctx, "view de-duplication unavailable; counting every view (fail open)", "error", err)
			} else {
				for j, i := range idx {
					first[i] = seen[j]
				}
			}
		}
	}
	counts := make(map[uuid.UUID]int64)
	for i, v := range batch {
		if first[i] {
			counts[v.listing]++
		}
	}
	return counts
}

// RedisDeduper remembers (listing, viewer) pairs for a window with SET NX,
// so a viewer counts once per listing per window.
type RedisDeduper struct {
	rdb     redis.Cmdable
	window  time.Duration
	timeout time.Duration
}

// NewRedisDeduper returns a deduper with the given window.
func NewRedisDeduper(rdb redis.Cmdable, window time.Duration) *RedisDeduper {
	return &RedisDeduper{rdb: rdb, window: window, timeout: time.Second}
}

// First sets every key (with KeyPrefix) unless it exists, in one pipeline,
// and reports which were new.
func (d *RedisDeduper) First(ctx context.Context, keys []string) ([]bool, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	cmds := make([]*redis.BoolCmd, len(keys))
	_, err := d.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, k := range keys {
			cmds[i] = p.SetNX(ctx, KeyPrefix+k, 1, d.window)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]bool, len(keys))
	for i, cmd := range cmds {
		out[i] = cmd.Val()
	}
	return out, nil
}
