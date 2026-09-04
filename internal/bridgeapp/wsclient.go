package bridgeapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"robloxkit/pkg/bridgeproto"
)

// Wire close reasons. The gateway hub closes revoked connections with
// closeReasonCredentialRevoked; the hub keeps its constants unexported, so the
// Bridge carries byte-identical copies of the shared wire contract.
const (
	closeReasonCredentialRevoked = "credential revoked"
	closeReasonPeerClosed        = "peer closed"
	closeReasonWriteFailed       = "write failed"
	closeReasonSlowConsumer      = "slow consumer"
	closeReasonInvalidEnvelope   = "invalid envelope"
	closeReasonLocalShutdown     = "shutdown"
)

// errTerminalAuth marks every failure that reconnecting cannot fix: the device
// credential itself was rejected by the gateway (HTTP 401 at dial time, or a
// mid-connection revocation close).
var errTerminalAuth = errors.New("bridgeapp: device credential was rejected by the gateway")

var (
	// errSendQueueFull reports that the outbound queue was full; the session
	// is disconnected so a slow consumer cannot wedge the Bridge.
	errSendQueueFull = errors.New("bridgeapp: outbound bridge queue is full")
	// errSessionClosed reports an enqueue on a session that already ended.
	errSessionClosed = errors.New("bridgeapp: bridge connection is closed")
)

const (
	defaultDialConnectTimeout  = 10 * time.Second
	defaultDialWriteTimeout    = 10 * time.Second
	defaultDialQueueDepth      = 64
	defaultDialMaxEnvelopeSize = 1 << 20
	// clientCloseHandshakeWait is how long the close path waits for the peer's
	// close echo before releasing the session goroutines; the close frame
	// itself is written within microseconds of starting the handshake.
	clientCloseHandshakeWait = 250 * time.Millisecond
	// clientCloseGrace bounds how long a wedged close handshake may stall
	// teardown before the TCP connection is force-closed.
	clientCloseGrace = 2 * time.Second
)

// dialConfig bounds one outbound Bridge WSS connection.
type dialConfig struct {
	URL            string
	Credential     string
	DeviceID       string
	HTTPClient     *http.Client
	ConnectTimeout time.Duration
	WriteTimeout   time.Duration
	QueueDepth     int
	Limits         bridgeproto.Limits
}

// dialBridge dials the authenticated gateway /bridge endpoint with the device
// credential in the Authorization bearer header, bounds inbound frames, and
// starts the session's reader and single writer goroutines. The returned
// session is immediately usable: the hub requires a hello envelope as the very
// first outbound message. A 401 handshake is classified as a terminal auth
// failure; every other dial error is transient and retryable.
func dialBridge(ctx context.Context, cfg dialConfig) (*bridgeSession, error) {
	if cfg.URL == "" {
		return nil, errors.New("bridgeapp: gateway URL is required")
	}
	if cfg.Credential == "" {
		return nil, errors.New("bridgeapp: device credential is required")
	}
	if cfg.DeviceID == "" {
		return nil, errors.New("bridgeapp: device ID is required")
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = defaultDialConnectTimeout
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = defaultDialWriteTimeout
	}
	if cfg.QueueDepth <= 0 {
		cfg.QueueDepth = defaultDialQueueDepth
	}
	if cfg.Limits.MaxPayloadBytes <= 0 {
		cfg.Limits.MaxPayloadBytes = defaultDialMaxEnvelopeSize
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+cfg.Credential)

	dialCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	conn, resp, err := websocket.Dial(dialCtx, cfg.URL, &websocket.DialOptions{
		HTTPHeader:      header,
		HTTPClient:      cfg.HTTPClient,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("%w: gateway returned http 401", errTerminalAuth)
		}
		return nil, fmt.Errorf("bridgeapp: dial gateway: %w", err)
	}
	// Enforce the frame bound immediately, before any application data flows.
	conn.SetReadLimit(int64(cfg.Limits.MaxPayloadBytes))

	session := newBridgeSession(conn, cfg)
	session.start()
	return session, nil
}

// bridgeSession is one live Bridge WSS connection. Exactly one reader
// goroutine feeds decoded inbound envelopes and exactly one writer goroutine
// drains a bounded queue; nothing else touches the socket. Enqueue never
// blocks: a full queue disconnects the session instead of wedging a caller.
// finished is closed exactly once when the session ends, and terminalCause
// distinguishes peer-side failures (set) from self-initiated closes (nil).
type bridgeSession struct {
	conn *websocket.Conn

	// ctx bounds writes and the session lifetime; readCtx bounds reads. Both
	// derive from Background and are released by the close path, mirroring the
	// hub's teardown ordering: the close handshake goes out first so the peer
	// observes the close code, then the contexts release the goroutines.
	ctx        context.Context
	cancel     context.CancelFunc
	readCtx    context.Context
	readCancel context.CancelFunc

	sendQ   chan []byte
	inbound chan bridgeproto.Envelope

	limits       bridgeproto.Limits
	writeTimeout time.Duration

	done   chan struct{}
	mu     sync.Mutex
	closed bool
	cause  error
}

func newBridgeSession(conn *websocket.Conn, cfg dialConfig) *bridgeSession {
	ctx, cancel := context.WithCancel(context.Background())
	readCtx, readCancel := context.WithCancel(context.Background())
	return &bridgeSession{
		conn:         conn,
		ctx:          ctx,
		cancel:       cancel,
		readCtx:      readCtx,
		readCancel:   readCancel,
		sendQ:        make(chan []byte, cfg.QueueDepth),
		inbound:      make(chan bridgeproto.Envelope, cfg.QueueDepth),
		limits:       cfg.Limits,
		writeTimeout: cfg.WriteTimeout,
		done:         make(chan struct{}),
	}
}

// start launches the session goroutines: exactly one reader and one writer.
func (s *bridgeSession) start() {
	go s.readLoop()
	go s.writeLoop()
}

// finished is closed exactly once when the session terminates.
func (s *bridgeSession) finished() <-chan struct{} { return s.done }

// terminalCause returns the peer-side reason the session ended, or nil when
// the Bridge closed the connection itself (or the session has not ended).
func (s *bridgeSession) terminalCause() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cause
}

// enqueue hands one envelope to the writer without ever blocking. A full
// queue disconnects the session as a slow consumer.
func (s *bridgeSession) enqueue(env bridgeproto.Envelope) error {
	data, err := bridgeproto.Encode(env, s.limits)
	if err != nil {
		return fmt.Errorf("bridgeapp: encode envelope: %w", err)
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return errSessionClosed
	}
	select {
	case s.sendQ <- data:
		return nil
	default:
		s.fail(errSendQueueFull, websocket.StatusPolicyViolation, closeReasonSlowConsumer)
		return errSendQueueFull
	}
}

// close terminates the session locally with a graceful close handshake. The
// terminal cause stays nil so callers can tell self-initiated shutdown from a
// peer-side failure.
func (s *bridgeSession) close(code websocket.StatusCode, reason string) {
	s.teardown(code, reason, nil)
}

// fail terminates the session because of a peer-side failure, recording cause
// for the reconnect decision.
func (s *bridgeSession) fail(cause error, code websocket.StatusCode, reason string) {
	s.teardown(code, reason, cause)
}

// teardown performs the one-time session teardown.
//
// Ordering matters: canceling the contexts while the websocket library still
// runs operations makes it force-drop the TCP connection, so the peer would
// never receive the close code. The graceful close handshake therefore starts
// first; the contexts are released once the handshake completes, or once the
// close frame has had a short window to reach the wire.
func (s *bridgeSession) teardown(code websocket.StatusCode, reason string, cause error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.cause = cause
	close(s.done)
	s.mu.Unlock()

	go func() {
		handshake := make(chan struct{})
		go func() {
			_ = s.conn.Close(code, reason)
			close(handshake)
		}()
		select {
		case <-handshake:
			// Handshake completed: the peer echoed the close (or the socket
			// already failed). Release everything.
		case <-time.After(clientCloseHandshakeWait):
			// The close frame is on the wire by now; stop waiting on a
			// dead-silent peer and release the session goroutines.
		}
		s.cancel()
		s.readCancel()
		select {
		case <-handshake:
		case <-time.After(clientCloseGrace):
			// A wedged close handshake must not delay teardown further.
			_ = s.conn.CloseNow()
		}
	}()
}

// readLoop decodes inbound envelopes. It also answers the hub's protocol
// pings, which is the client's only required liveness behavior.
func (s *bridgeSession) readLoop() {
	for {
		_, data, err := s.conn.Read(s.readCtx)
		if err != nil {
			s.fail(err, websocket.StatusNormalClosure, closeReasonPeerClosed)
			return
		}
		env, decodeErr := bridgeproto.Decode(data, s.limits)
		if decodeErr != nil {
			s.fail(fmt.Errorf("bridgeapp: invalid inbound envelope: %w", decodeErr), websocket.StatusProtocolError, closeReasonInvalidEnvelope)
			return
		}
		select {
		case s.inbound <- env:
		case <-s.readCtx.Done():
			return
		}
	}
}

// writeLoop is the single writer goroutine. It exits when the session context
// is canceled or a write fails.
func (s *bridgeSession) writeLoop() {
	for {
		select {
		case data := <-s.sendQ:
			ctx, cancel := context.WithTimeout(s.ctx, s.writeTimeout)
			err := s.conn.Write(ctx, websocket.MessageBinary, data)
			cancel()
			if err != nil {
				s.fail(fmt.Errorf("bridgeapp: write envelope: %w", err), websocket.StatusInternalError, closeReasonWriteFailed)
				return
			}
		case <-s.ctx.Done():
			return
		}
	}
}

// isTerminalAuthFailure reports whether cause means the device credential can
// never authenticate again (a dial-time 401 or a mid-connection revocation
// close), so reconnecting must stop permanently.
func isTerminalAuthFailure(cause error) bool {
	if cause == nil {
		return false
	}
	if errors.Is(cause, errTerminalAuth) {
		return true
	}
	var closeErr websocket.CloseError
	if errors.As(cause, &closeErr) {
		return closeErr.Code == websocket.StatusPolicyViolation && closeErr.Reason == closeReasonCredentialRevoked
	}
	return false
}
