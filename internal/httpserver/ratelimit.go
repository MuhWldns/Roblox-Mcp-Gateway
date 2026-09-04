package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
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

// Class names one enforced endpoint family. Every class carries its own
// budget so one endpoint's exhaustion never consumes another's.
type Class string

// The endpoint classes the router limits. The OAuth provider endpoints and
// the /mcp and /bridge transports are mounted outside the browser API
// subtree but enforce their class budgets through the same limiter.
const (
	ClassLogin  Class = "login"
	ClassOAuth  Class = "oauth"
	ClassEnroll Class = "enrollment"
	ClassWSS    Class = "wss"
	ClassAdmin  Class = "admin"
	ClassMCP    Class = "mcp"
)

// Key identifies one rate bucket: the endpoint class plus the opaque
// principal it tracks — a user id, a grant id, a device id, or the remote
// host for unauthenticated endpoints.
type Key struct {
	Class Class
	ID    string
}

// Decision reports one admission outcome. RetryAfter is positive whenever
// Allowed is false, so callers can answer 429 with a Retry-After header.
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration
}

// Budget configures one class's token bucket. Burst is the bucket capacity,
// Refill restores Refill tokens every Interval, and MaxInFlight — when
// positive — bounds the per-key concurrent slots for request-scoped classes
// (slots are held for the request's lifetime and released at its end).
type Budget struct {
	Burst       int
	Refill      int
	Interval    time.Duration
	MaxInFlight int
}

// LimiterConfig configures the general limiter: one budget per class.
type LimiterConfig struct {
	Budgets map[Class]Budget
	// MaxKeys bounds the total tracked buckets; zero selects the default.
	MaxKeys int
}

const defaultLimiterMaxKeys = 4096

// limiterBucket is one key's token bucket.
type limiterBucket struct {
	tokens float64
	last   time.Time
}

// Limiter is the general keyed token-bucket rate limiter serving every
// endpoint class through one Allow interface. Memory is bounded like the
// MCP limiter: idle buckets are swept, released in-flight slots vanish, and
// the tracked-key count never exceeds MaxKeys — beyond the cap the limiter
// fails closed. A clock regression fails closed until the clock catches up.
type Limiter struct {
	budgets map[Class]Budget
	maxKeys int

	mu           sync.Mutex
	buckets      map[Key]*limiterBucket
	inflight     map[Key]int
	lastObserved time.Time
	sweepHorizon time.Duration
}

// NewLimiter validates the configuration and builds the limiter.
func NewLimiter(cfg LimiterConfig) (*Limiter, error) {
	if len(cfg.Budgets) == 0 {
		return nil, errors.New("httpserver: limiter requires at least one class budget")
	}
	budgets := make(map[Class]Budget, len(cfg.Budgets))
	horizon := time.Duration(0)
	for class, budget := range cfg.Budgets {
		if budget.Burst <= 0 || budget.Refill <= 0 || budget.Interval <= 0 {
			return nil, fmt.Errorf("httpserver: class %q requires a positive burst, refill, and interval", class)
		}
		if budget.MaxInFlight < 0 {
			return nil, fmt.Errorf("httpserver: class %q has a negative in-flight bound", class)
		}
		budgets[class] = budget
		full := time.Duration(float64(budget.Interval) * float64(budget.Burst) / float64(budget.Refill))
		if full > horizon {
			horizon = full
		}
	}
	maxKeys := cfg.MaxKeys
	if maxKeys <= 0 {
		maxKeys = defaultLimiterMaxKeys
	}
	return &Limiter{
		budgets:      budgets,
		maxKeys:      maxKeys,
		buckets:      make(map[Key]*limiterBucket),
		inflight:     make(map[Key]int),
		sweepHorizon: horizon,
	}, nil
}

// Allow consumes cost tokens from key's bucket, refilling by the elapsed
// time first. Exhausted bursts, unknown classes, non-positive costs, key
// overflows, and clock regressions are denied with a positive RetryAfter.
func (l *Limiter) Allow(now time.Time, key Key, cost int) Decision {
	if l == nil {
		return Decision{}
	}
	budget, known := l.budgets[key.Class]
	if !known || cost <= 0 {
		// Fail closed; the retry hint falls back to one second.
		return Decision{RetryAfter: fallbackRetryAfter}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Fail closed on clock regression: spent tokens are never resurrected.
	if !l.lastObserved.IsZero() && now.Before(l.lastObserved) {
		return Decision{RetryAfter: budget.Interval}
	}
	l.lastObserved = now
	l.sweepLocked(now, budget)

	bucket, ok := l.buckets[key]
	if !ok {
		if len(l.buckets)+len(l.inflight) >= l.maxKeys {
			return Decision{RetryAfter: budget.Interval}
		}
		bucket = &limiterBucket{tokens: float64(budget.Burst), last: now}
		l.buckets[key] = bucket
	}
	if elapsed := now.Sub(bucket.last); elapsed > 0 {
		bucket.tokens = min(float64(budget.Burst),
			bucket.tokens+elapsed.Seconds()*float64(budget.Refill)/budget.Interval.Seconds())
		bucket.last = now
	}
	if bucket.tokens >= float64(cost) {
		bucket.tokens -= float64(cost)
		return Decision{Allowed: true}
	}
	deficit := float64(cost) - bucket.tokens
	retry := time.Duration(deficit * float64(budget.Interval) / float64(budget.Refill))
	if retry <= 0 {
		retry = time.Millisecond
	}
	return Decision{RetryAfter: retry}
}

// Acquire reserves one concurrent in-flight slot for key. Classes without
// an in-flight budget never grant slots. The returned release is idempotent
// and safe for concurrent use.
func (l *Limiter) Acquire(key Key) (release func(), ok bool) {
	if l == nil {
		return nil, false
	}
	budget, known := l.budgets[key.Class]
	if !known || budget.MaxInFlight <= 0 {
		return nil, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight[key] >= budget.MaxInFlight {
		return nil, false
	}
	if _, tracked := l.buckets[key]; !tracked && len(l.buckets)+len(l.inflight) >= l.maxKeys {
		return nil, false
	}
	l.inflight[key]++

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
func (l *Limiter) InFlight(key Key) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inflight[key]
}

// Middleware enforces one class's budget around a handler. keyOf derives the
// bucket principal per request; denied requests answer 429 with a
// Retry-After header and the sanitized body — never the key. A nil limiter
// passes through untouched, so compositions without limits keep working.
func (l *Limiter) Middleware(class Class, keyOf func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if l == nil || keyOf == nil {
				next.ServeHTTP(w, r)
				return
			}
			decision := l.Allow(time.Now(), Key{Class: class, ID: keyOf(r)}, 1)
			if !decision.Allowed {
				writeRateLimited(w, decision.RetryAfter)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

const fallbackRetryAfter = time.Second

// sweepLocked drops buckets idle for at least one full refill horizon: they
// are as good as fresh, so deleting them cannot grant extra allowance. The
// caller holds l.mu.
func (l *Limiter) sweepLocked(now time.Time, budget Budget) {
	if l.sweepHorizon <= 0 {
		return
	}
	for key, bucket := range l.buckets {
		if now.Sub(bucket.last) >= l.sweepHorizon {
			delete(l.buckets, key)
		}
	}
}

// RemotePrincipal derives the bucket principal from the verified client
// address installed by NewTrustedClientAddressMiddleware. When used outside
// that middleware, it falls back to the canonical direct RemoteAddr host.
func RemotePrincipal(r *http.Request) string {
	if addr, ok := clientAddressFromContext(r.Context()); ok {
		return addr.String()
	}
	return canonicalRemoteAddress(r.RemoteAddr)
}

// SessionPrincipal derives the bucket principal from the authenticated
// session user, falling back to the remote host when no session user is in
// the context yet.
func SessionPrincipal(r *http.Request) string {
	if userID, err := sessionUserID(r); err == nil && userID != "" {
		return userID
	}
	return RemotePrincipal(r)
}

// writeRateLimited answers with the sanitized 429: a fixed message, a
// Retry-After rounded up to whole seconds, and no echo of the rate key.
func writeRateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	w.Header().Set("Content-Type", "application/json")
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int((retryAfter+time.Second-1)/time.Second)))
	}
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "rate limit exceeded, retry later"})
}
