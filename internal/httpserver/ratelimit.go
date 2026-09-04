package httpserver

import (
	"errors"
	"sync"
	"time"
)

// MCPLimiter bounds /mcp request rates and concurrent tool calls per opaque
// key. The gateway enforces it against two key namespaces — one per
// connector grant, one per user — before any request reaches the Bridge.
//
// Memory is bounded three ways: buckets older than one window are swept on
// the next operation, in-flight counters vanish when their slot is released,
// and the total tracked-key count never exceeds MaxKeys — new keys beyond
// the cap are rejected, so the limiter fails closed instead of growing.
type MCPLimiter struct {
	requests    int
	window      time.Duration
	maxInFlight int
	maxKeys     int
	now         func() time.Time

	mu           sync.Mutex
	rate         map[string]*mcpRateBucket
	inflight     map[string]int
	lastSweep    int64 // window index of the last sweep
	lastObserved time.Time
}

// MCPLimiterConfig configures one limiter. Requests is the per-key request
// budget per Window; MaxInFlight is the per-key concurrent slot count.
type MCPLimiterConfig struct {
	// Requests is the maximum number of requests per key per Window.
	Requests int
	// Window is the fixed rate window length.
	Window time.Duration
	// MaxInFlight is the maximum concurrent in-flight tools/call per key.
	MaxInFlight int
	// MaxKeys bounds the total tracked keys; zero selects the default.
	MaxKeys int
	// Now supplies the clock; nil defaults to time.Now. A clock that
	// regresses makes Allow fail closed until it catches up.
	Now func() time.Time
}

const defaultMCPMaxKeys = 4096

// mcpRateBucket is one key's fixed-window counter.
type mcpRateBucket struct {
	window int64
	count  int
}

// NewMCPLimiter validates the configuration and builds the limiter.
func NewMCPLimiter(cfg MCPLimiterConfig) (*MCPLimiter, error) {
	if cfg.Requests <= 0 {
		return nil, errors.New("httpserver: limiter requires a positive request budget")
	}
	if cfg.Window <= 0 {
		return nil, errors.New("httpserver: limiter requires a positive window")
	}
	if cfg.MaxInFlight <= 0 {
		return nil, errors.New("httpserver: limiter requires a positive in-flight bound")
	}
	if cfg.MaxKeys <= 0 {
		cfg.MaxKeys = defaultMCPMaxKeys
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &MCPLimiter{
		requests:    cfg.Requests,
		window:      cfg.Window,
		maxInFlight: cfg.MaxInFlight,
		maxKeys:     cfg.MaxKeys,
		now:         cfg.Now,
		rate:        make(map[string]*mcpRateBucket),
		inflight:    make(map[string]int),
	}, nil
}

// Allow consumes one request of key's per-window burst. It reports false
// when the burst is exhausted, when the key cap is reached, or when the
// clock regresses.
func (l *MCPLimiter) Allow(key string) bool {
	if l == nil {
		return false
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	// Fail closed on clock regression: never resurrect an earlier window's
	// allowance.
	if now.Before(l.lastObserved) {
		return false
	}
	l.lastObserved = now

	index := now.UnixNano() / int64(l.window)
	if index > l.lastSweep {
		l.sweepLocked(index)
		l.lastSweep = index
	}

	bucket, ok := l.rate[key]
	if !ok {
		if len(l.rate)+len(l.inflight) >= l.maxKeys {
			return false
		}
		bucket = &mcpRateBucket{}
		l.rate[key] = bucket
	}
	if bucket.window != index {
		bucket.window = index
		bucket.count = 0
	}
	bucket.count++
	return bucket.count <= l.requests
}

// Acquire reserves one concurrent in-flight slot for key and returns the
// release function. It reports false when the key already holds its maximum
// slots or the key cap is reached. The returned release is idempotent and
// safe for concurrent use.
func (l *MCPLimiter) Acquire(key string) (release func(), ok bool) {
	if l == nil {
		return nil, false
	}
	l.mu.Lock()
	if l.inflight[key] >= l.maxInFlight {
		l.mu.Unlock()
		return nil, false
	}
	if _, tracked := l.rate[key]; !tracked && len(l.rate)+len(l.inflight) >= l.maxKeys {
		l.mu.Unlock()
		return nil, false
	}
	l.inflight[key]++
	l.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.inflight[key] > 0 {
				l.inflight[key]--
			}
			if l.inflight[key] == 0 {
				delete(l.inflight, key)
			}
		})
	}, true
}

// InFlight reports key's currently held slots.
func (l *MCPLimiter) InFlight(key string) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inflight[key]
}

// Window reports the fixed rate window, for Retry-After computation.
func (l *MCPLimiter) Window() time.Duration {
	if l == nil {
		return 0
	}
	return l.window
}

// sweepLocked removes buckets at least one full window old. The caller holds
// l.mu.
func (l *MCPLimiter) sweepLocked(current int64) {
	for key, bucket := range l.rate {
		if bucket.window < current-1 {
			delete(l.rate, key)
		}
	}
}
