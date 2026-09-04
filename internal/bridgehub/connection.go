package bridgehub

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"

	"robloxkit/pkg/bridgeproto"
)

// Close reasons are fixed, sanitized strings safe for wire transmission.
const (
	reasonSuperseded        = "superseded"
	reasonHelloTimeout      = "hello timeout"
	reasonHeartbeatTimeout  = "heartbeat timeout"
	reasonSlowConsumer      = "slow consumer"
	reasonDeviceMismatch    = "device mismatch"
	reasonCredentialRevoked = "credential revoked"
	reasonServerShutdown    = "server shutdown"
	reasonInvalidEnvelope   = "invalid envelope"
	reasonExpectedHello     = "expected hello"
	reasonWriteFailed       = "write failed"
	reasonPeerClosed        = "peer closed"
)

// closeGrace bounds how long a wedged close handshake may stall before the TCP
// connection is force-closed.
const closeGrace = 2 * time.Second

// closeHandshakeWait is how long close waits for the peer's close echo before
// releasing contexts; the close frame itself is written within microseconds of
// starting the handshake.
const closeHandshakeWait = 250 * time.Millisecond

// Registry-level errors surfaced to callers of Registry operations.
var (
	// ErrDeviceOffline reports that the device has no live connection.
	ErrDeviceOffline = errors.New("bridgehub: device is not connected")
	// ErrSlowConsumer reports that a connection's writer queue was full and
	// the connection has been disconnected.
	ErrSlowConsumer = errors.New("bridgehub: device writer queue is full")
	// ErrConnectionClosed reports that the target connection already closed.
	ErrConnectionClosed = errors.New("bridgehub: connection is closed")
)

// connectionOptions bounds one connection's resources.
type connectionOptions struct {
	queueDepth        int
	maxEnvelopeBytes  int
	writeTimeout      time.Duration
	heartbeatInterval time.Duration
	heartbeatTimeout  time.Duration
}

// Connection wraps one upgraded Bridge WebSocket with a bounded writer queue
// drained by a single writer goroutine. Nothing else writes to the socket.
type Connection struct {
	device Device
	ws     *websocket.Conn

	send              chan []byte
	limits            bridgeproto.Limits
	writeTimeout      time.Duration
	heartbeatInterval time.Duration
	heartbeatTimeout  time.Duration

	ctx        context.Context
	cancel     context.CancelFunc
	readCtx    context.Context
	readCancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

// newConnection builds the wrapper. parent must outlive the HTTP request; it
// is the connection's own lifetime context and never derives from the
// hijacked request.
func newConnection(parent context.Context, ws *websocket.Conn, device Device, opts connectionOptions) *Connection {
	if parent == nil {
		parent = context.Background()
	}
	if opts.queueDepth <= 0 {
		opts.queueDepth = defaultQueueDepth
	}
	if opts.maxEnvelopeBytes <= 0 {
		opts.maxEnvelopeBytes = defaultMaxEnvelopeBytes
	}
	if opts.writeTimeout <= 0 {
		opts.writeTimeout = defaultWriteTimeout
	}
	ctx, cancel := context.WithCancel(parent)
	readCtx, readCancel := context.WithCancel(parent)
	return &Connection{
		device:            device,
		ws:                ws,
		send:              make(chan []byte, opts.queueDepth),
		limits:            bridgeproto.Limits{MaxPayloadBytes: opts.maxEnvelopeBytes},
		writeTimeout:      opts.writeTimeout,
		heartbeatInterval: opts.heartbeatInterval,
		heartbeatTimeout:  opts.heartbeatTimeout,
		ctx:               ctx,
		cancel:            cancel,
		readCtx:           readCtx,
		readCancel:        readCancel,
		done:              make(chan struct{}),
	}
}

// start launches the connection's own goroutines: exactly one writer and, when
// heartbeats are enabled, one ping loop.
func (c *Connection) start() {
	go c.writeLoop()
	if c.heartbeatInterval > 0 {
		go c.pingLoop()
	}
}

// DeviceID returns the authenticated device identifier.
func (c *Connection) DeviceID() string { return c.device.DeviceID }

// UserID returns the owning internal user.
func (c *Connection) UserID() string { return c.device.UserID }

// Done is closed exactly once when the connection terminates.
func (c *Connection) Done() <-chan struct{} { return c.done }

// Closed reports whether the connection has been closed.
func (c *Connection) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// enqueue encodes the envelope and hands it to the writer without ever
// blocking. A full queue disconnects the slow consumer.
func (c *Connection) enqueue(env bridgeproto.Envelope) error {
	data, err := bridgeproto.Encode(env, c.limits)
	if err != nil {
		return fmt.Errorf("bridgehub: encode envelope: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrConnectionClosed
	}
	select {
	case c.send <- data:
		return nil
	default:
		c.closeLocked(websocket.StatusPolicyViolation, reasonSlowConsumer)
		return ErrSlowConsumer
	}
}

// close terminates the connection with a sanitized reason. It is idempotent
// and safe from any goroutine.
func (c *Connection) close(code websocket.StatusCode, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked(code, reason)
}

// closeLocked performs the one-time teardown. Callers hold c.mu.
//
// Ordering matters: canceling the connection context while the websocket
// library still runs operations on it makes the library force-drop the TCP
// connection (its ctx watchers call close), so the peer would never receive
// the close code. The graceful close handshake therefore starts first; the
// contexts are released once the handshake completes, or once the close frame
// has had a short window to reach the wire, so teardown never waits on a
// dead-silent peer longer than that.
func (c *Connection) closeLocked(code websocket.StatusCode, reason string) {
	if c.closed {
		return
	}
	c.closed = true
	close(c.done)
	go func() {
		handshake := make(chan struct{})
		go func() {
			_ = c.ws.Close(code, reason)
			close(handshake)
		}()
		select {
		case <-handshake:
			// Handshake completed: the peer echoed the close (or the socket
			// already failed). Release everything.
			c.cancel()
			c.readCancel()
			return
		case <-time.After(closeHandshakeWait):
			// The close frame is on the wire by now; stop waiting for a
			// dead-silent peer and release the connection's goroutines.
		}
		c.cancel()
		c.readCancel()
		select {
		case <-handshake:
		case <-time.After(closeGrace):
			// A wedged close handshake (blocked writer) must not delay
			// teardown past the grace period.
			_ = c.ws.CloseNow()
		}
	}()
}

// writeLoop is the single writer goroutine. It exits when the connection
// context is canceled or a write fails.
func (c *Connection) writeLoop() {
	for {
		select {
		case data := <-c.send:
			ctx, cancel := context.WithTimeout(c.ctx, c.writeTimeout)
			err := c.ws.Write(ctx, websocket.MessageBinary, data)
			cancel()
			if err != nil {
				c.close(websocket.StatusInternalError, reasonWriteFailed)
				return
			}
		case <-c.ctx.Done():
			return
		}
	}
}

// pingLoop enforces heartbeat liveness: if no pong arrives for a ping within
// the heartbeat timeout, the connection is closed with a policy violation.
func (c *Connection) pingLoop() {
	ticker := time.NewTicker(c.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(c.ctx, c.heartbeatTimeout)
			err := c.ws.Ping(ctx)
			cancel()
			if err != nil {
				c.close(websocket.StatusPolicyViolation, reasonHeartbeatTimeout)
				return
			}
		}
	}
}
