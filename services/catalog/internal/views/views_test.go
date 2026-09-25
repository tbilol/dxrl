package views

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// memStore records AddViews calls; fail makes the next n calls fail.
type memStore struct {
	mu     sync.Mutex
	counts map[uuid.UUID]int64
	calls  int
	fail   int
}

func (s *memStore) AddViews(_ context.Context, counts map[uuid.UUID]int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.fail > 0 {
		s.fail--
		return errors.New("deadlock detected")
	}
	if s.counts == nil {
		s.counts = map[uuid.UUID]int64{}
	}
	for id, n := range counts {
		s.counts[id] += n
	}
	return nil
}

func (s *memStore) snapshot() (map[uuid.UUID]int64, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.counts), s.calls
}

// setDeduper remembers keys in memory (or fails).
type setDeduper struct {
	seen map[string]bool
	err  error
}

func (d *setDeduper) First(_ context.Context, keys []string) ([]bool, error) {
	if d.err != nil {
		return nil, d.err
	}
	out := make([]bool, len(keys))
	for i, k := range keys {
		out[i] = !d.seen[k]
		d.seen[k] = true
	}
	return out, nil
}

var discard = slog.New(slog.DiscardHandler)

func TestFlushAggregatesAndDeduplicates(t *testing.T) {
	store := &memStore{}
	c := NewCounter(store, &setDeduper{seen: map[string]bool{}}, Config{}, discard)
	a, b := uuid.New(), uuid.New()
	alice, bob := uuid.New(), uuid.New()
	for _, v := range [][2]uuid.UUID{{a, alice}, {a, alice}, {a, bob}, {a, uuid.Nil}, {a, uuid.Nil}, {b, alice}} {
		c.Record(v[0], v[1])
	}
	c.Flush(context.Background())
	counts, calls := store.snapshot()
	// alice counts once on a, anonymous views always count.
	if calls != 1 || counts[a] != 4 || counts[b] != 1 {
		t.Fatalf("counts = %v after %d calls", counts, calls)
	}
	// A later repeat by alice is not counted; nothing queued means no write.
	c.Record(a, alice)
	c.Flush(context.Background())
	c.Flush(context.Background())
	if counts, calls = store.snapshot(); calls != 1 || counts[a] != 4 {
		t.Fatalf("counts = %v after %d calls", counts, calls)
	}
}

func TestFlushFailsOpenAndRetriesOnce(t *testing.T) {
	store := &memStore{fail: 1}
	c := NewCounter(store, &setDeduper{err: errors.New("redis down")}, Config{}, discard)
	a, alice := uuid.New(), uuid.New()
	c.Record(a, alice)
	c.Record(a, alice)
	c.Flush(context.Background())
	// The deduper failed: both views count; the first write failed and was retried.
	if counts, calls := store.snapshot(); calls != 2 || counts[a] != 2 {
		t.Fatalf("counts = %v after %d calls", counts, calls)
	}
	// Two failures lose the batch (logged), nothing else happens.
	store.fail = 2
	c.Record(a, uuid.Nil)
	c.Flush(context.Background())
	if counts, calls := store.snapshot(); calls != 4 || counts[a] != 2 {
		t.Fatalf("counts = %v after %d calls", counts, calls)
	}
}

func TestRecordNeverBlocks(t *testing.T) {
	store := &memStore{}
	c := NewCounter(store, nil, Config{BufferSize: 2}, discard)
	a := uuid.New()
	for range 5 {
		c.Record(a, uuid.New()) // the last three are dropped
	}
	if c.dropped.Load() != 3 {
		t.Fatalf("dropped = %d", c.dropped.Load())
	}
	c.Flush(context.Background())
	if counts, _ := store.snapshot(); counts[a] != 2 || c.dropped.Load() != 0 {
		t.Fatalf("counts = %v, dropped = %d", counts, c.dropped.Load())
	}
}

func TestRunFlushesPeriodicallyAndOnStop(t *testing.T) {
	store := &memStore{}
	c := NewCounter(store, nil, Config{FlushInterval: 10 * time.Millisecond}, discard)
	a := uuid.New()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	c.Record(a, uuid.Nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if counts, _ := store.snapshot(); counts[a] == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the view was not flushed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Views queued at shutdown are written before Run returns.
	c2 := NewCounter(store, nil, Config{FlushInterval: time.Hour}, discard)
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() {
		c2.Run(ctx2)
		close(done2)
	}()
	c2.Record(a, uuid.Nil)
	cancel2()
	<-done2
	if counts, _ := store.snapshot(); counts[a] != 2 {
		t.Fatalf("counts after stop = %v", counts)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestRedisDeduper(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	d := NewRedisDeduper(rdb, time.Hour)
	ctx := context.Background()

	got, err := d.First(ctx, []string{"l1:u1", "l1:u2", "l1:u1"})
	if err != nil || len(got) != 3 || !got[0] || !got[1] || got[2] {
		t.Fatalf("first = %v, %v", got, err)
	}
	if ttl := mr.TTL(KeyPrefix + "l1:u1"); ttl != time.Hour {
		t.Fatalf("ttl = %s", ttl)
	}
	mr.FastForward(time.Hour + time.Second)
	if got, err := d.First(ctx, []string{"l1:u1"}); err != nil || !got[0] {
		t.Fatalf("after the window = %v, %v", got, err)
	}
	mr.Close()
	if _, err := d.First(ctx, []string{"l1:u3"}); err == nil {
		t.Fatal("redis down: want an error (the counter then fails open)")
	}
}
