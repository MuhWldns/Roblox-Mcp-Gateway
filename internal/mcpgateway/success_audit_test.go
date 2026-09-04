package mcpgateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"robloxkit/internal/audit"
)

// recordingUsage is the test double for the UsageRecorder contract.
type recordingUsage struct {
	records []usageRecordCall
}

type usageRecordCall struct {
	GatewayRequestID string
	Usage            audit.Usage
}

func (r *recordingUsage) Increment(_ context.Context, gatewayRequestID string, usage audit.Usage) error {
	r.records = append(r.records, usageRecordCall{GatewayRequestID: gatewayRequestID, Usage: usage})
	return nil
}

func TestRelayCallDetailedReturnsTrace(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	relay := fx.newTestRelay(t, time.Hour)
	device := fx.connectDevice(t, nil)
	awaitRelayStatus(t, relay, fx, true, 1)

	payload, trace, err := relay.CallDetailed(context.Background(), "trace-session", fx.testGrant(),
		"tools/call", relayPayload(t, "tools/call"))
	if err != nil {
		t.Fatalf("CallDetailed: %v", err)
	}
	if len(payload) == 0 {
		t.Fatal("CallDetailed returned an empty payload")
	}
	if trace.GatewayRequestID == "" {
		t.Fatal("trace missing gateway request id")
	}
	requests := device.requests()
	if len(requests) != 1 {
		t.Fatalf("device requests = %d, want 1", len(requests))
	}
	if trace.GatewayRequestID != requests[0].GatewayRequestID {
		t.Fatalf("trace gateway id = %q, want the envelope's %q", trace.GatewayRequestID, requests[0].GatewayRequestID)
	}
	if trace.DeviceID != fx.deviceID {
		t.Fatalf("trace device = %q, want %q", trace.DeviceID, fx.deviceID)
	}
}

// TestSuccessToolCallAuditUsesQueue proves the success path audits through
// the bounded queue: the event is persisted at flush time and carries only
// safe identifiers.
func TestSuccessToolCallAuditUsesQueue(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	fx.connectDevice(t, nil)
	session := fx.openSession(t)

	envelope := session.call("tools/call", `{"name":"get_instance_tree","arguments":{}}`)
	if _, hasErr := envelope["error"]; hasErr {
		t.Fatalf("allowed tools/call failed: %v", envelope)
	}

	fx.gateway.FlushSuccessAudit(context.Background())
	rows := fx.auditRows(t, auditActionToolCall)
	if len(rows) != 1 {
		t.Fatalf("tool-call audit rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Reason != "success" {
		t.Fatalf("tool-call audit reason = %q, want success", row.Reason)
	}
	if !row.UserID.Valid || row.UserID.String != fx.userID {
		t.Fatalf("tool-call audit user = %+v, want %q", row.UserID, fx.userID)
	}
	if !row.TargetID.Valid || row.TargetID.String != fx.grantID {
		t.Fatalf("tool-call audit target = %+v, want grant %q", row.TargetID, fx.grantID)
	}
	if !row.Metadata.Valid || !strings.Contains(row.Metadata.String, "get_instance_tree") {
		t.Fatalf("tool-call audit metadata = %+v, want the safe tool name", row.Metadata)
	}
	if got := fx.gateway.SuccessAuditDropped(); got != 0 {
		t.Fatalf("success audit drops = %d, want 0", got)
	}
}

// TestDenialsRemainSynchronousWhileSuccessIsQueued pins the split: denial
// events land immediately, success events wait for the queue.
func TestDenialsRemainSynchronousWhileSuccessIsQueued(t *testing.T) {
	fx := newGatewayFixture(t, nil)

	// A request without a bearer token is denied synchronously.
	res := fx.post(t, "", "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "")
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("missing-bearer status = %d, want 401", res.StatusCode)
	}
	rows := fx.auditRows(t, auditActionDenied)
	if len(rows) != 1 {
		t.Fatalf("denial audit rows without flush = %d, want 1 (denials stay synchronous)", len(rows))
	}
	if got := fx.gateway.SuccessAuditDropped(); got != 0 {
		t.Fatalf("success audit drops = %d, want 0", got)
	}
}

// TestSuccessQueueDropsWhenSaturated pins the bound: overflowing a bounded
// queue drops the newest successes, counts them, and never blocks.
func TestSuccessQueueDropsWhenSaturated(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	queue, err := audit.NewQueue(audit.NewService(fx.auditStore), 2)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	gateway := &Gateway{cfg: Config{Audit: audit.NewService(fx.auditStore)}, success: queue}
	granted := fx.testGrant()
	for range 1000 {
		gateway.recordToolSuccess(context.Background(), CallTrace{GatewayRequestID: "gw-sat"},
			granted, "get_instance_tree")
	}
	if got := gateway.SuccessAuditDropped(); got == 0 {
		t.Fatal("saturated queue reported zero drops")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gateway.FlushSuccessAudit(ctx)
	if got := queue.Pending(); got != 0 {
		t.Fatalf("pending after flush = %d, want 0", got)
	}
}

// TestUsageRecordedOnSuccessfulToolCall proves the full relay path accounts
// one usage record per completed tools/call, keyed by the gateway request id.
func TestUsageRecordedOnSuccessfulToolCall(t *testing.T) {
	recorder := &recordingUsage{}
	fx := newGatewayFixture(t, func(spec *gatewaySpec) { spec.usage = recorder })
	fx.connectDevice(t, nil)
	session := fx.openSession(t)
	envelope := session.call("tools/call", `{"name":"get_instance_tree","arguments":{}}`)
	if _, hasErr := envelope["error"]; hasErr {
		t.Fatalf("allowed tools/call failed: %v", envelope)
	}

	if len(recorder.records) != 1 {
		t.Fatalf("usage records = %d, want 1", len(recorder.records))
	}
	call := recorder.records[0]
	if call.GatewayRequestID == "" {
		t.Fatal("usage record missing gateway request id")
	}
	usage := call.Usage
	if usage.UserID != fx.userID {
		t.Fatalf("usage user = %q, want %q", usage.UserID, fx.userID)
	}
	if usage.DeviceID != fx.deviceID {
		t.Fatalf("usage device = %q, want %q", usage.DeviceID, fx.deviceID)
	}
	if usage.Operation != "tools/call" {
		t.Fatalf("usage operation = %q, want tools/call", usage.Operation)
	}
	if usage.Outcome != "success" {
		t.Fatalf("usage outcome = %q, want success", usage.Outcome)
	}
	if usage.Units != 1 {
		t.Fatalf("usage units = %d, want 1", usage.Units)
	}
}

func TestRecordToolUsageWithoutRecorderIsNoop(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	gateway := &Gateway{cfg: Config{Audit: audit.NewService(fx.auditStore)}}
	// A nil recorder must never break the relay path.
	gateway.recordToolUsage(CallTrace{GatewayRequestID: "gw-noop"}, fx.testGrant(), "success")
}
