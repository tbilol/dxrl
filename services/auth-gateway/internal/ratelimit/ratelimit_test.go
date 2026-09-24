package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestFixedWindow(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	l := New(rdb, 2, time.Hour)
	ctx := context.Background()

	for i := range 2 {
		ok, _, err := l.Allow(ctx, "user-a")
		if err != nil || !ok {
			t.Fatalf("hit %d: ok=%v err=%v", i+1, ok, err)
		}
	}
	ok, retryAfter, err := l.Allow(ctx, "user-a")
	if err != nil || ok {
		t.Fatalf("third hit: ok=%v err=%v", ok, err)
	}
	if retryAfter <= 59*time.Minute || retryAfter > time.Hour {
		t.Fatalf("retry after %s, want about an hour", retryAfter)
	}
	if ttl := mr.TTL(KeyPrefix + "user-a"); ttl <= 0 || ttl > time.Hour {
		t.Fatalf("key TTL %s", ttl)
	}

	// Keys are independent.
	if ok, _, err := l.Allow(ctx, "user-b"); err != nil || !ok {
		t.Fatalf("other key: ok=%v err=%v", ok, err)
	}

	// A new window starts once the old one expires.
	mr.FastForward(time.Hour + time.Second)
	if ok, _, err := l.Allow(ctx, "user-a"); err != nil || !ok {
		t.Fatalf("after the window: ok=%v err=%v", ok, err)
	}
}

func TestRedisDownIsAnError(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.Close()
	if _, _, err := New(rdb, 5, time.Hour).Allow(context.Background(), "user-a"); err == nil {
		t.Fatal("want an error when Redis is down (the service then fails open)")
	}
}

// Exceeded must answer without counting a hit: auth-gateway asks it before
// every sign-in and only counts the failures.
func TestExceededDoesNotCount(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	l := New(rdb, 2, time.Hour)
	ctx := context.Background()

	// An unknown key is never over the limit, and asking does not create it.
	for range 5 {
		if over, _, err := l.Exceeded(ctx, "user-a"); err != nil || over {
			t.Fatalf("Exceeded = %v, %v", over, err)
		}
	}
	if mr.Exists(KeyPrefix + "user-a") {
		t.Fatal("Exceeded created the key")
	}

	for range 3 { // two allowed, the third goes over
		if _, _, err := l.Allow(ctx, "user-a"); err != nil {
			t.Fatal(err)
		}
	}
	over, retryAfter, err := l.Exceeded(ctx, "user-a")
	if err != nil || !over {
		t.Fatalf("Exceeded = %v, %v", over, err)
	}
	if retryAfter <= 59*time.Minute || retryAfter > time.Hour {
		t.Fatalf("retry after %s, want about an hour", retryAfter)
	}

	// A new window clears it.
	mr.FastForward(time.Hour + time.Second)
	if over, _, err := l.Exceeded(ctx, "user-a"); err != nil || over {
		t.Fatalf("after the window: Exceeded = %v, %v", over, err)
	}
}

func TestExceededRedisDownIsAnError(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.Close()
	if _, _, err := New(rdb, 5, time.Hour).Exceeded(context.Background(), "user-a"); err == nil {
		t.Fatal("want an error when Redis is down (the service then fails open)")
	}
}
