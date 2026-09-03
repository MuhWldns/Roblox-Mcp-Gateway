// Package mcpgateway exposes the remote MCP gateway: it authenticates
// connector requests, resolves their Studio targets, correlates in-flight
// requests with Bridge responses, and relays results back. The pending
// registry in this file is the correlation core the transports build on.
package mcpgateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Result is the single outcome delivered to a Register caller. Payload is
// the relayed JSON-RPC response on success; Err is the failure cause on
// deadline expiry, session cancellation, or device failure. Exactly one
// Result is ever delivered per registration.
type Result struct {
	Payload json.RawMessage
	Err     error
}

// Pending is a bounded registry correlating forwarded requests with the
// Bridge responses that complete them. Every in-flight request is keyed by
// a fresh unguessable gateway ID, so two clients sending the same JSON-RPC
// ID never collide. Each registered request is completed exactly once — by
// its response, its deadline, the end of its session, or the failure of its
// device — and its registry entry is removed exactly once. All methods are
// safe for concurrent use.
type Pending struct {
	maxEntries int

	mu       sync.Mutex
	entries  map[string]*pendingEntry
	sessions map[string]map[string]*pendingEntry
	devices  map[string]map[string]*pendingEntry
}

// pendingEntry is one in-flight request. The original JSON-RPC ID is part
// of the correlation record; the waiter also keeps it to rebuild the
// response.
type pendingEntry struct {
	gatewayID  string
	sessionID  string
	deviceID   string
	originalID json.RawMessage
	timer      *time.Timer
	result     chan Result
}

// NewPending builds an empty registry holding at most maxEntries requests.
// A maxEntries of zero or less means unbounded.
func NewPending(maxEntries int) *Pending {
	return &Pending{
		maxEntries: maxEntries,
		entries:    make(map[string]*pendingEntry),
		sessions:   make(map[string]map[string]*pendingEntry),
		devices:    make(map[string]map[string]*pendingEntry),
	}
}

// Register records a forwarded request under a fresh gateway ID and
// returns a channel yielding exactly one Result: the response relayed by
// Resolve, or a deadline, cancellation, or device-failure result. The
// request's deadline timer bounds the wait; a deadline already in the past
// completes immediately. Register fails with ErrTooManyPending when the
// registry is full and with ErrInvalidRequest when the session or the
// JSON-RPC ID is empty.
func (p *Pending) Register(sessionID string, originalID json.RawMessage, deadline time.Time) (string, <-chan Result, error) {
	if sessionID == "" || len(originalID) == 0 {
		return "", nil, fmt.Errorf("%w: session and JSON-RPC id are required", ErrInvalidRequest)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.maxEntries > 0 && len(p.entries) >= p.maxEntries {
		return "", nil, ErrTooManyPending
	}
	gatewayID, err := p.freshIDLocked()
	if err != nil {
		return "", nil, err
	}

	entry := &pendingEntry{
		gatewayID:  gatewayID,
		sessionID:  sessionID,
		originalID: append(json.RawMessage(nil), originalID...),
		result:     make(chan Result, 1),
	}
	p.entries[gatewayID] = entry
	session, ok := p.sessions[sessionID]
	if !ok {
		session = make(map[string]*pendingEntry)
		p.sessions[sessionID] = session
	}
	session[gatewayID] = entry

	// Arm the deadline while holding the lock, so the callback can never
	// deliver before the timer is recorded on the entry.
	entry.timer = time.AfterFunc(time.Until(deadline), func() {
		p.deliver(gatewayID, Result{Err: ErrDeadlineExceeded})
	})

	return gatewayID, entry.result, nil
}

// Associate binds a registered request to the device it is forwarded to so
// FailDevice can fan failures out to it. Callers must associate before
// delivering the request to the device. It fails with ErrUnknownCorrelation
// when the request is unknown or already completed.
func (p *Pending) Associate(gatewayID, deviceID string) error {
	if gatewayID == "" || deviceID == "" {
		return fmt.Errorf("%w: gateway and device ids are required", ErrInvalidRequest)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	entry, ok := p.entries[gatewayID]
	if !ok {
		return ErrUnknownCorrelation
	}
	if entry.deviceID != deviceID {
		if entry.deviceID != "" {
			if set := p.devices[entry.deviceID]; set != nil {
				delete(set, gatewayID)
				if len(set) == 0 {
					delete(p.devices, entry.deviceID)
				}
			}
		}
		entry.deviceID = deviceID
	}
	set, ok := p.devices[deviceID]
	if !ok {
		set = make(map[string]*pendingEntry)
		p.devices[deviceID] = set
	}
	set[gatewayID] = entry
	return nil
}

// Resolve relays result to the waiter of the matching registration and
// removes the registry entry. It fails with ErrUnknownCorrelation when no
// pending request matches — the response is late (the deadline already
// passed) or a duplicate (the request already completed).
func (p *Pending) Resolve(gatewayID string, result Result) error {
	if !p.deliver(gatewayID, result) {
		return ErrUnknownCorrelation
	}
	return nil
}

// CancelSession completes every pending request of sessionID with a
// cancellation result. Each waiter observes exactly one Result; cancelling
// again, or after completion, delivers nothing.
func (p *Pending) CancelSession(sessionID string) {
	p.mu.Lock()
	set := p.sessions[sessionID]
	delete(p.sessions, sessionID)
	p.mu.Unlock()

	// Iterating a nil map is a no-op.
	for _, entry := range set {
		p.deliver(entry.gatewayID, Result{Err: ErrCancelled})
	}
}

// FailDevice completes every pending request forwarded to deviceID with
// Result{Err: cause}, or ErrDeviceFailed when cause is nil. Each waiter
// observes exactly one Result; failing the device again, or after
// completion, delivers nothing.
func (p *Pending) FailDevice(deviceID string, cause error) {
	p.mu.Lock()
	set := p.devices[deviceID]
	delete(p.devices, deviceID)
	p.mu.Unlock()

	if cause == nil {
		cause = ErrDeviceFailed
	}
	for _, entry := range set {
		p.deliver(entry.gatewayID, Result{Err: cause})
	}
}

// deliver removes the entry for gatewayID and hands result to its waiter.
// It returns false when the request already completed, so every caller
// delivers at most once. The result channel has capacity one and only
// deliver sends on it, so the send never blocks and the channel is never
// closed — there is no close to double-run.
func (p *Pending) deliver(gatewayID string, result Result) bool {
	p.mu.Lock()
	entry, ok := p.entries[gatewayID]
	if !ok {
		p.mu.Unlock()
		return false
	}
	p.removeLocked(entry)
	if entry.timer != nil {
		entry.timer.Stop()
	}
	p.mu.Unlock()

	entry.result <- result
	return true
}

// removeLocked deletes the entry from every index. The caller must hold
// p.mu.
func (p *Pending) removeLocked(entry *pendingEntry) {
	delete(p.entries, entry.gatewayID)
	if set := p.sessions[entry.sessionID]; set != nil {
		delete(set, entry.gatewayID)
		if len(set) == 0 {
			delete(p.sessions, entry.sessionID)
		}
	}
	if entry.deviceID != "" {
		if set := p.devices[entry.deviceID]; set != nil {
			delete(set, entry.gatewayID)
			if len(set) == 0 {
				delete(p.devices, entry.deviceID)
			}
		}
	}
}

// freshIDLocked returns an unused unguessable gateway ID. Gateway IDs cross
// the wire to the device and back, so they must not be guessable: a device
// must not be able to forge a response for a request it did not receive.
// IDs are therefore 128 random bits. The caller must hold p.mu.
func (p *Pending) freshIDLocked() (string, error) {
	var buf [16]byte
	for attempt := 0; attempt < 8; attempt++ {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", fmt.Errorf("mcpgateway: gateway id generation failed: %w", err)
		}
		id := "gw_" + hex.EncodeToString(buf[:])
		if _, exists := p.entries[id]; !exists {
			return id, nil
		}
	}
	return "", fmt.Errorf("mcpgateway: gateway id generation failed: persistent collisions")
}
