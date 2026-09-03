package bridgehub

import (
	"context"
	"sync"

	"github.com/coder/websocket"

	"robloxkit/pkg/bridgeproto"
)

// Registry tracks live device connections. A device has at most one live
// connection; a new registration atomically replaces the old one. All methods
// are safe for concurrent use.
type Registry struct {
	mu    sync.Mutex
	conns map[string]*Connection
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{conns: make(map[string]*Connection)}
}

// Register associates deviceID with conn and returns the replaced connection,
// if any. The caller is responsible for closing the replaced connection.
func (r *Registry) Register(deviceID string, conn *Connection) (replaced *Connection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns == nil {
		r.conns = make(map[string]*Connection)
	}
	if previous, ok := r.conns[deviceID]; ok {
		replaced = previous
	}
	r.conns[deviceID] = conn
	return replaced
}

// Get returns the device's live connection.
func (r *Registry) Get(deviceID string) (*Connection, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	conn, ok := r.conns[deviceID]
	return conn, ok
}

// Len reports the number of live connections.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}

// Send delivers env to the device's live connection. It never blocks the
// caller: a full writer queue disconnects the slow consumer instead.
func (r *Registry) Send(ctx context.Context, deviceID string, env bridgeproto.Envelope) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	conn, ok := r.Get(deviceID)
	if !ok {
		return ErrDeviceOffline
	}
	return conn.enqueue(env)
}

// Disconnect closes the device's live connection with a sanitized reason.
func (r *Registry) Disconnect(deviceID, safeReason string) {
	r.mu.Lock()
	conn, ok := r.conns[deviceID]
	if ok {
		delete(r.conns, deviceID)
	}
	r.mu.Unlock()
	if ok {
		conn.close(websocket.StatusPolicyViolation, safeReason)
	}
}

// CloseAll closes every live connection with the given code and reason.
func (r *Registry) CloseAll(code websocket.StatusCode, reason string) {
	r.mu.Lock()
	conns := make([]*Connection, 0, len(r.conns))
	for _, conn := range r.conns {
		conns = append(conns, conn)
	}
	r.mu.Unlock()
	for _, conn := range conns {
		conn.close(code, reason)
	}
}

// removeIfCurrent removes the registry entry only when it still points at
// conn. This lets a replaced connection clean itself up without evicting its
// replacement.
func (r *Registry) removeIfCurrent(deviceID string, conn *Connection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.conns[deviceID]; ok && current == conn {
		delete(r.conns, deviceID)
	}
}
