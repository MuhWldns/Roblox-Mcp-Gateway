package httpserver

import (
	"strings"
	"testing"
	"time"
)

// limiterClock is a mutable time source for deterministic window tests.
type limiterClock struct{ now time.Time }

func (c *limiterClock) Now() time.Time { return c.now }

func newTestLimiter(t *testing.T, cfg MCPLimiterConfig) (*MCPLimiter, *limiterClock) {
	t.Helper()
	clock := &limiterClock{now: time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)}
	cfg.Now = clock.Now
	limiter, err := NewMCPLimiter(cfg)
	if err != nil {
		t.Fatalf("NewMCPLimiter: %v", err)
	}
	return limiter, clock
}

func TestRateLimiterAllowsBurstThenRejects(t *testing.T) {
	limiter, _ := newTestLimiter(t, MCPLimiterConfig{Requests: 3, Window: time.Minute, MaxInFlight: 4})
	for i := 0; i < 3; i++ {
		if !limiter.Allow("grant:a") {
			t.Fatalf("request %d within burst rejected", i+1)
		}
	}
	if limiter.Allow("grant:a") {
		t.Fatal("request beyond burst allowed")
	}
}

func TestRateLimiterWindowResetsBurst(t *testing.T) {
	limiter, clock := newTestLimiter(t, MCPLimiterConfig{Requests: 1, Window: time.Minute, MaxInFlight: 4})
	if !limiter.Allow("user:u") {
		t.Fatal("first request in window rejected")
	}
	if limiter.Allow("user:u") {
		t.Fatal("second request in same window allowed")
	}
	clock.now = clock.now.Add(2 * time.Minute)
	if !limiter.Allow("user:u") {
		t.Fatal("request in a fresh window rejected")
	}
}

func TestRateLimiterKeysAreIndependent(t *testing.T) {
	limiter, _ := newTestLimiter(t, MCPLimiterConfig{Requests: 1, Window: time.Minute, MaxInFlight: 4})
	if !limiter.Allow("grant:a") {
		t.Fatal("grant a first request rejected")
	}
	if !limiter.Allow("grant:b") {
		t.Fatal("grant b first request rejected by grant a's exhaustion")
	}
	if limiter.Allow("grant:a") {
		t.Fatal("grant a second request allowed")
	}
}

func TestRateLimiterConcurrentInFlightExhaustion(t *testing.T) {
	limiter, _ := newTestLimiter(t, MCPLimiterConfig{Requests: 100, Window: time.Minute, MaxInFlight: 2})
	first, ok := limiter.Acquire("grant:a")
	if !ok {
		t.Fatal("first in-flight slot rejected")
	}
	second, ok := limiter.Acquire("grant:a")
	if !ok {
		t.Fatal("second in-flight slot rejected")
	}
	if _, ok := limiter.Acquire("grant:a"); ok {
		t.Fatal("third concurrent in-flight slot allowed")
	}
	second()
	if release, ok := limiter.Acquire("grant:a"); !ok {
		t.Fatal("released slot not reusable")
	} else {
		release()
	}
	first()
	first() // release must be idempotent
	if release, ok := limiter.Acquire("grant:a"); !ok {
		t.Fatal("slot not freed after releases")
	} else {
		release()
	}
	if got := limiter.InFlight("grant:a"); got != 0 {
		t.Fatalf("InFlight after full release = %d, want 0", got)
	}
}

func TestRateLimiterInFlightKeysAreIndependent(t *testing.T) {
	limiter, _ := newTestLimiter(t, MCPLimiterConfig{Requests: 100, Window: time.Minute, MaxInFlight: 1})
	release, ok := limiter.Acquire("user:one")
	if !ok {
		t.Fatal("user one slot rejected")
	}
	if _, ok := limiter.Acquire("user:two"); !ok {
		t.Fatal("user two rejected because user one holds a slot")
	}
	release()
}

func TestRateLimiterRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  MCPLimiterConfig
	}{
		{"zero requests", MCPLimiterConfig{Window: time.Minute, MaxInFlight: 1}},
		{"negative requests", MCPLimiterConfig{Requests: -1, Window: time.Minute, MaxInFlight: 1}},
		{"zero window", MCPLimiterConfig{Requests: 1, MaxInFlight: 1}},
		{"zero in-flight", MCPLimiterConfig{Requests: 1, Window: time.Minute}},
	}
	for _, tt := range tests {
		if _, err := NewMCPLimiter(tt.cfg); err == nil {
			t.Errorf("%s: NewMCPLimiter unexpectedly succeeded", tt.name)
		}
	}
}

func TestRateLimiterBoundedKeyCountFailsClosed(t *testing.T) {
	limiter, _ := newTestLimiter(t, MCPLimiterConfig{Requests: 10, Window: time.Minute, MaxInFlight: 2, MaxKeys: 2})
	if !limiter.Allow("grant:a") || !limiter.Allow("grant:b") {
		t.Fatal("keys within the cap rejected")
	}
	if limiter.Allow("grant:c") {
		t.Fatal("key beyond the cap allowed; the limiter must fail closed")
	}
	if _, ok := limiter.Acquire("user:c"); ok {
		t.Fatal("in-flight key beyond the cap allowed; the limiter must fail closed")
	}
}

func TestRateLimiterPrunesStaleWindows(t *testing.T) {
	limiter, clock := newTestLimiter(t, MCPLimiterConfig{Requests: 1, Window: time.Minute, MaxInFlight: 2, MaxKeys: 2})
	if !limiter.Allow("grant:a") {
		t.Fatal("first key rejected")
	}
	clock.now = clock.now.Add(3 * time.Minute)
	// The stale grant:a bucket is swept on the next operation, freeing room
	// for a new key without exceeding the key cap.
	if !limiter.Allow("grant:b") {
		t.Fatal("new key rejected because stale buckets were not pruned")
	}
	if !limiter.Allow("grant:c") {
		t.Fatal("key after pruning rejected")
	}
}

func TestRateLimiterRejectsNegativeClockRegression(t *testing.T) {
	limiter, clock := newTestLimiter(t, MCPLimiterConfig{Requests: 1, Window: time.Minute, MaxInFlight: 1})
	if !limiter.Allow("grant:a") {
		t.Fatal("request rejected")
	}
	clock.now = clock.now.Add(-2 * time.Minute)
	if limiter.Allow("grant:a") {
		t.Fatal("clock regression resurrected a stale window allowance")
	}
}

func TestRateLimiterKeysAreWellFormed(t *testing.T) {
	// Callers construct the key namespace; the limiter itself must treat
	// keys opaquely, which this test pins by exercising a path-like key.
	limiter, _ := newTestLimiter(t, MCPLimiterConfig{Requests: 1, Window: time.Minute, MaxInFlight: 1})
	key := "grant:" + strings.Repeat("d", 36)
	if !limiter.Allow(key) {
		t.Fatal("opaque key rejected")
	}
}
