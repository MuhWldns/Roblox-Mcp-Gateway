package metrics

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestSnapshotAndPrometheusExposeAggregatePressure(t *testing.T) {
	started := time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)
	registry := New(started)
	registry.BridgeConnected(false)
	registry.BridgeConnected(true)
	registry.BridgeDisconnected()
	registry.SlowConsumerDropped()
	finish := registry.MCPCall()
	finish()
	finish()

	snapshot := registry.Snapshot(started.Add(90*time.Second), 8, 32, 3)
	if snapshot.UptimeSeconds != 90 || snapshot.Bridge.Online != 1 || snapshot.Bridge.Reconnects != 1 {
		t.Fatalf("unexpected lifecycle snapshot: %+v", snapshot)
	}
	if snapshot.Bridge.SlowConsumerDrops != 1 || snapshot.MCP.Inflight != 0 || snapshot.MCP.TotalRequests != 1 {
		t.Fatalf("unexpected pressure snapshot: %+v", snapshot)
	}
	if snapshot.Audit.QueueDepth != 8 || snapshot.Audit.QueueCapacity != 32 || snapshot.Audit.Dropped != 3 {
		t.Fatalf("unexpected audit snapshot: %+v", snapshot.Audit)
	}

	var output bytes.Buffer
	if err := WritePrometheus(&output, snapshot); err != nil {
		t.Fatalf("write exposition: %v", err)
	}
	for _, metric := range []string{
		"robloxkit_bridge_online 1",
		"robloxkit_mcp_requests_total 1",
		"robloxkit_audit_queue_depth 8",
		"robloxkit_audit_dropped_total 3",
	} {
		if !strings.Contains(output.String(), metric) {
			t.Errorf("exposition missing %q", metric)
		}
	}
}
