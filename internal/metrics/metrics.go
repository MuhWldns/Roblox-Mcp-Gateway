// Package metrics provides the process-local, bounded observability registry.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const latencyWindow = 512

// Registry records aggregate runtime metrics without user or device labels.
type Registry struct {
	started           time.Time
	bridgeOnline      atomic.Int64
	bridgeReconnects  atomic.Uint64
	slowConsumerDrops atomic.Uint64
	mcpInflight       atomic.Int64
	mcpTotal          atomic.Uint64

	mu           sync.Mutex
	latencies    [latencyWindow]time.Duration
	latencyCount uint64
}

// Snapshot is one internally consistent view suitable for APIs and rendering.
type Snapshot struct {
	UptimeSeconds int64 `json:"uptime_seconds"`
	Bridge        struct {
		Online            int64  `json:"online"`
		Reconnects        uint64 `json:"reconnects"`
		SlowConsumerDrops uint64 `json:"slow_consumer_drops"`
	} `json:"bridge"`
	MCP struct {
		Inflight      int64  `json:"inflight"`
		TotalRequests uint64 `json:"total_requests"`
		LatencyMS     struct {
			P50 float64 `json:"p50"`
			P95 float64 `json:"p95"`
			P99 float64 `json:"p99"`
		} `json:"latency_ms"`
	} `json:"mcp"`
	Audit struct {
		QueueDepth    int    `json:"queue_depth"`
		QueueCapacity int    `json:"queue_capacity"`
		Dropped       uint64 `json:"dropped"`
	} `json:"audit"`
}

func New(now time.Time) *Registry { return &Registry{started: now} }
func (r *Registry) BridgeConnected(reconnect bool) {
	if r == nil {
		return
	}
	r.bridgeOnline.Add(1)
	if reconnect {
		r.bridgeReconnects.Add(1)
	}
}
func (r *Registry) BridgeDisconnected() {
	if r != nil {
		r.bridgeOnline.Add(-1)
	}
}
func (r *Registry) SlowConsumerDropped() {
	if r != nil {
		r.slowConsumerDrops.Add(1)
	}
}

// MCPCall returns an idempotent completion callback.
func (r *Registry) MCPCall() func() {
	if r == nil {
		return func() {}
	}
	r.mcpInflight.Add(1)
	r.mcpTotal.Add(1)
	started := time.Now()
	var once sync.Once
	return func() { once.Do(func() { r.mcpInflight.Add(-1); r.observeLatency(time.Since(started)) }) }
}
func (r *Registry) observeLatency(d time.Duration) {
	r.mu.Lock()
	r.latencies[r.latencyCount%latencyWindow] = d
	r.latencyCount++
	r.mu.Unlock()
}

func (r *Registry) Snapshot(now time.Time, auditDepth, auditCapacity int, auditDropped uint64) Snapshot {
	var s Snapshot
	if r == nil {
		return s
	}
	s.UptimeSeconds = max(0, int64(now.Sub(r.started)/time.Second))
	s.Bridge.Online = r.bridgeOnline.Load()
	s.Bridge.Reconnects = r.bridgeReconnects.Load()
	s.Bridge.SlowConsumerDrops = r.slowConsumerDrops.Load()
	s.MCP.Inflight = r.mcpInflight.Load()
	s.MCP.TotalRequests = r.mcpTotal.Load()
	s.Audit.QueueDepth = auditDepth
	s.Audit.QueueCapacity = auditCapacity
	s.Audit.Dropped = auditDropped
	r.mu.Lock()
	n := min(r.latencyCount, latencyWindow)
	values := make([]time.Duration, n)
	if r.latencyCount <= latencyWindow {
		copy(values, r.latencies[:n])
	} else {
		copy(values, r.latencies[:])
	}
	r.mu.Unlock()
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	s.MCP.LatencyMS.P50 = quantile(values, .50)
	s.MCP.LatencyMS.P95 = quantile(values, .95)
	s.MCP.LatencyMS.P99 = quantile(values, .99)
	return s
}
func quantile(v []time.Duration, q float64) float64 {
	if len(v) == 0 {
		return 0
	}
	i := int(q*float64(len(v)-1) + .5)
	return float64(v[i]) / float64(time.Millisecond)
}

// WritePrometheus writes fixed-cardinality Prometheus text exposition.
func WritePrometheus(w io.Writer, s Snapshot) error {
	_, err := fmt.Fprintf(w, "# TYPE robloxkit_uptime_seconds gauge\nrobloxkit_uptime_seconds %d\n# TYPE robloxkit_bridge_online gauge\nrobloxkit_bridge_online %d\n# TYPE robloxkit_bridge_reconnects_total counter\nrobloxkit_bridge_reconnects_total %d\n# TYPE robloxkit_bridge_slow_consumer_drops_total counter\nrobloxkit_bridge_slow_consumer_drops_total %d\n# TYPE robloxkit_mcp_inflight gauge\nrobloxkit_mcp_inflight %d\n# TYPE robloxkit_mcp_requests_total counter\nrobloxkit_mcp_requests_total %d\nrobloxkit_mcp_latency_milliseconds{quantile=\"0.5\"} %.3f\nrobloxkit_mcp_latency_milliseconds{quantile=\"0.95\"} %.3f\nrobloxkit_mcp_latency_milliseconds{quantile=\"0.99\"} %.3f\n# TYPE robloxkit_audit_queue_depth gauge\nrobloxkit_audit_queue_depth %d\n# TYPE robloxkit_audit_queue_capacity gauge\nrobloxkit_audit_queue_capacity %d\n# TYPE robloxkit_audit_dropped_total counter\nrobloxkit_audit_dropped_total %d\n", s.UptimeSeconds, s.Bridge.Online, s.Bridge.Reconnects, s.Bridge.SlowConsumerDrops, s.MCP.Inflight, s.MCP.TotalRequests, s.MCP.LatencyMS.P50, s.MCP.LatencyMS.P95, s.MCP.LatencyMS.P99, s.Audit.QueueDepth, s.Audit.QueueCapacity, s.Audit.Dropped)
	return err
}
