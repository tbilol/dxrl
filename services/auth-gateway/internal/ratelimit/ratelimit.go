// Package ratelimit is a fixed-window request counter in Redis.
package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// KeyPrefix namespaces auth-gateway's counters in a shared Redis.
const KeyPrefix = "auth-gateway:ratelimit:"

// The window starts with the first hit: INCR creates the key and PEXPIRE
// gives it the window length. A key that somehow lost its TTL gets it back,
// so a counter can never block a user forever.
var fixedWindow = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
  ttl = tonumber(ARGV[1])
end
return {n, ttl}
`)

// peekWindow reads the counter without touching it: a missing key is 0.
var peekWindow = redis.NewScript(`
local n = redis.call('GET', KEYS[1])
if not n then return {0, 0} end
return {tonumber(n), redis.call('PTTL', KEYS[1])}
`)

// Limiter allows Limit hits per key per Window.
type Limiter struct {
	rdb     redis.Scripter
	limit   int64
	window  time.Duration
	timeout time.Duration
}

// New returns a limiter. Every call is bounded by a short timeout so a slow
// Redis cannot hold up requests (callers fail open on errors).
func New(rdb redis.Scripter, limit int, window time.Duration) *Limiter {
	return &Limiter{rdb: rdb, limit: int64(limit), window: window, timeout: time.Second}
}

// Allow counts a hit for key. When the limit is exceeded it returns false and
// how long until the window resets.
func (l *Limiter) Allow(ctx context.Context, key string) (bool, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	vals, err := fixedWindow.Run(ctx, l.rdb, []string{KeyPrefix + key}, l.window.Milliseconds()).Int64Slice()
	if err != nil {
		return false, 0, fmt.Errorf("rate limit %q: %w", key, err)
	}
	if len(vals) != 2 {
		return false, 0, fmt.Errorf("rate limit %q: unexpected script reply %v", key, vals)
	}
	if vals[0] > l.limit {
		return false, time.Duration(vals[1]) * time.Millisecond, nil
	}
	return true, 0, nil
}

// Exceeded reports whether key is already over its limit WITHOUT counting a
// hit, so a request can be refused before it is attempted. auth-gateway counts
// failed sign-ins only (Allow on failure) and asks this before each attempt,
// which keeps successful sign-ins out of the counter.
func (l *Limiter) Exceeded(ctx context.Context, key string) (bool, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	vals, err := peekWindow.Run(ctx, l.rdb, []string{KeyPrefix + key}).Int64Slice()
	if err != nil {
		return false, 0, fmt.Errorf("rate limit %q: %w", key, err)
	}
	if len(vals) != 2 {
		return false, 0, fmt.Errorf("rate limit %q: unexpected script reply %v", key, vals)
	}
	if vals[0] > l.limit {
		return true, time.Duration(max(vals[1], 0)) * time.Millisecond, nil
	}
	return false, 0, nil
}
