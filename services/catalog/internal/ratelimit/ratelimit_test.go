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
