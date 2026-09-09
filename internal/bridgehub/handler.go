package bridgehub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"robloxkit/internal/entitlement"
	"robloxkit/pkg/bridgeproto"
)

// Config bounds hub resources and policy timers. Zero values fall back to the
// defaults; negative values are rejected so misconfiguration cannot disable a
// bound.
type Config struct {
	// Store reads device credentials, devices, bindings, and identities.
	Store Store
	// Entitlements is the frozen entitlement service gating WSS access.
	Entitlements *entitlement.Service
	// Pepper keys device credential digests.
	Pepper []byte
	// HelloTimeout bounds the wait for the first bridgeproto hello envelope.
	HelloTimeout time.Duration
	// HeartbeatInterval controls the server ping cadence.
	HeartbeatInterval time.Duration
	// HeartbeatTimeout bounds how long a ping may wait for its pong.
	HeartbeatTimeout time.Duration
	// ReauthInterval revalidates the credential mid-connection. Zero or
	// negative disables live revalidation.
	ReauthInterval time.Duration
	// MaxEnvelopeBytes caps both the socket read limit and encoded envelopes.
	MaxEnvelopeBytes int
	// QueueDepth caps buffered outbound envelopes per connection.
	QueueDepth int
	// WriteTimeout bounds each outbound envelope write.
	WriteTimeout time.Duration
	// OriginPatterns authorizes browser origins for the upgrade. Absent
	// Origin headers (the Bridge client) are always allowed.
	OriginPatterns []string
	// BaseContext parents all connection lifetimes. The hijacked HTTP request
	// context is never retained after the upgrade. Defaults to Background.
	BaseContext context.Context
	// Now supplies the clock for credential expiry checks. Defaults to time.Now.
	Now func() time.Time
	// OnEnvelope receives validated inbound envelopes after the hello
	// handshake. Heartbeat envelopes are consumed by the hub itself.
	OnEnvelope func(ctx context.Context, device Device, env bridgeproto.Envelope)
}

const (
	defaultHelloTimeout      = 10 * time.Second
	defaultHeartbeatInterval = 15 * time.Second
	defaultHeartbeatTimeout  = 45 * time.Second
	defaultReauthInterval    = 30 * time.Second
	defaultMaxEnvelopeBytes  = 1 << 20
	defaultQueueDepth        = 64
	defaultWriteTimeout      = 10 * time.Second
)

// Hub serves /bridge: it authenticates device credentials, upgrades to
// WebSocket, registers live connections, and enforces protocol policy
// (hello handshake, heartbeat liveness, read limits, and bounded queues).
type Hub struct {
	cfg      Config
	auth     *Authenticator
	registry *Registry
	ctx      context.Context
}

// NewHub validates the configuration and builds the hub.
func NewHub(cfg Config) (*Hub, error) {
	if cfg.Store == nil {
		return nil, errors.New("bridgehub: store is required")
	}
	if cfg.Entitlements == nil {
		return nil, errors.New("bridgehub: entitlement service is required")
	}
	if len(cfg.Pepper) == 0 {
		return nil, errors.New("bridgehub: credential pepper is required")
	}
	for name, d := range map[string]time.Duration{
		"hello timeout":      cfg.HelloTimeout,
		"heartbeat interval": cfg.HeartbeatInterval,
		"heartbeat timeout":  cfg.HeartbeatTimeout,
		"write timeout":      cfg.WriteTimeout,
	} {
		if d < 0 {
			return nil, fmt.Errorf("bridgehub: %s must not be negative", name)
		}
	}
	if cfg.MaxEnvelopeBytes < 0 {
		return nil, errors.New("bridgehub: max envelope bytes must not be negative")
	}
	if cfg.QueueDepth < 0 {
		return nil, errors.New("bridgehub: queue depth must not be negative")
	}

	if cfg.HelloTimeout == 0 {
		cfg.HelloTimeout = defaultHelloTimeout
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = defaultHeartbeatInterval
	}
	if cfg.HeartbeatTimeout == 0 {
		cfg.HeartbeatTimeout = defaultHeartbeatTimeout
	}
	if cfg.ReauthInterval == 0 {
		cfg.ReauthInterval = defaultReauthInterval
	}
	if cfg.MaxEnvelopeBytes == 0 {
		cfg.MaxEnvelopeBytes = defaultMaxEnvelopeBytes
	}
	if cfg.QueueDepth == 0 {
		cfg.QueueDepth = defaultQueueDepth
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = defaultWriteTimeout
	}
	if cfg.HeartbeatTimeout < cfg.HeartbeatInterval {
		return nil, errors.New("bridgehub: heartbeat timeout must be at least the heartbeat interval")
	}
	if cfg.BaseContext == nil {
		cfg.BaseContext = context.Background()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Hub{
		cfg:      cfg,
		auth:     NewAuthenticator(cfg.Store, cfg.Entitlements, cfg.Pepper, cfg.Now),
		registry: NewRegistry(),
		ctx:      cfg.BaseContext,
	}, nil
}

// Registry exposes the live connection registry for delivery and admin
// disconnects.
func (h *Hub) Registry() *Registry { return h.registry }

// Shutdown closes every live connection with a normal closure code and a
// safe reason.
func (h *Hub) Shutdown() {
	h.registry.CloseAll(websocket.StatusNormalClosure, reasonServerShutdown)
}

// ServeHTTP handles the /bridge endpoint. Authentication happens before the
// upgrade; after the upgrade the hijacked request context is never used again.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	device, err := h.auth.Authenticate(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="bridge"`)
		http.Error(w, "invalid device credential", http.StatusUnauthorized)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns:  h.cfg.OriginPatterns,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		// Accept already wrote the handshake failure response.
		return
	}
	// Enforce the frame bound immediately, before any application data.
	ws.SetReadLimit(int64(h.cfg.MaxEnvelopeBytes))
	conn := h.newConnection(ws, device)
	if replaced := h.registry.Register(device.DeviceID, conn); replaced != nil {
		replaced.close(websocket.StatusPolicyViolation, reasonSuperseded)
	}

	telemetryQueue := make(chan func(), 64)
	var telemetryMu sync.Mutex
	telemetryClosed := false
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		for action := range telemetryQueue {
			action()
		}
	}()

	nowFn := h.cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	queueTelemetry := func(action func()) {
		telemetryMu.Lock()
		defer telemetryMu.Unlock()
		if telemetryClosed {
			return
		}
		// Non-blocking send; if buffer is full, drop non-critical update.
		select {
		case telemetryQueue <- action:
		default:
		}
	}

	var connectionID string
	recordConnectOnce := func() {
		dbCtx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
		defer cancel()
		currConn, isCurrent := h.registry.Get(device.DeviceID)
		if !isCurrent || currConn != conn {
			return
		}
		var err error
		connectionID, err = h.cfg.Store.RecordConnect(dbCtx, device.UserID, device.DeviceID, nowFn())
		_ = err
	}

	conn.onPong = func() {
		pongTime := nowFn()
		queueTelemetry(func() {
			currConn, isCurrent := h.registry.Get(device.DeviceID)
			if !isCurrent || currConn != conn {
				return
			}
			dbCtx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
			defer cancel()
			_ = h.cfg.Store.RecordHeartbeat(dbCtx, device.UserID, device.DeviceID, pongTime)
		})
	}

	conn.start()
	go h.reauthLoop(conn)

	defer func() {
		telemetryMu.Lock()
		telemetryClosed = true
		close(telemetryQueue)
		telemetryMu.Unlock()
		<-workerDone
		if connectionID != "" {
			dbCtx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
			defer cancel()
			_ = h.cfg.Store.RecordDisconnect(dbCtx, connectionID, nowFn(), "")
		}
	}()

	h.serve(conn, queueTelemetry, recordConnectOnce)
}

func (h *Hub) newConnection(ws *websocket.Conn, device Device) *Connection {
	return newConnection(h.ctx, ws, device, connectionOptions{
		queueDepth:        h.cfg.QueueDepth,
		maxEnvelopeBytes:  h.cfg.MaxEnvelopeBytes,
		writeTimeout:      h.cfg.WriteTimeout,
		heartbeatInterval: h.cfg.HeartbeatInterval,
		heartbeatTimeout:  h.cfg.HeartbeatTimeout,
	})
}

// serve is the per-connection read loop. It blocks the HTTP goroutine until
// the connection ends, then removes the connection from the registry.
func (h *Hub) serve(conn *Connection, queueTelemetry func(func()), recordConnectOnce func()) {
	defer h.registry.removeIfCurrent(conn.device.DeviceID, conn)

	hello := make(chan struct{})
	go func() {
		select {
		case <-hello:
		case <-time.After(h.cfg.HelloTimeout):
			conn.close(websocket.StatusPolicyViolation, reasonHelloTimeout)
		}
	}()

	helloSeen := false
	nowFn := h.cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	for {
		_, data, err := conn.ws.Read(conn.readCtx)
		if err != nil {
			// Read-limit violations have already been closed 1009 by the
			// websocket layer; every other error is terminal here.
			conn.close(websocket.StatusNormalClosure, reasonPeerClosed)
			return
		}
		env, decodeErr := bridgeproto.Decode(data, conn.limits)
		if decodeErr != nil {
			conn.close(websocket.StatusPolicyViolation, reasonInvalidEnvelope)
			return
		}
		if env.DeviceID != conn.device.DeviceID {
			conn.close(websocket.StatusPolicyViolation, reasonDeviceMismatch)
			return
		}
		if !helloSeen {
			if env.Type != bridgeproto.TypeHello {
				conn.close(websocket.StatusPolicyViolation, reasonExpectedHello)
				return
			}
			helloSeen = true
			close(hello)
			var helloPayload struct {
				BridgeVersion string `json:"bridge_version"`
				Platform      string `json:"platform"`
				Hostname      string `json:"hostname"`
			}
			if err := json.Unmarshal(env.Payload, &helloPayload); err == nil {
				queueTelemetry(func() {
					currConn, isCurrent := h.registry.Get(conn.device.DeviceID)
					if !isCurrent || currConn != conn {
						return
					}
					recordConnectOnce()
					dbCtx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
					defer cancel()
					_ = h.cfg.Store.RecordHello(dbCtx, conn.device.UserID, conn.device.DeviceID, helloPayload.BridgeVersion, helloPayload.Platform, helloPayload.Hostname)
				})
			} else {
				queueTelemetry(func() {
					recordConnectOnce()
				})
			}
			continue
		}
		if env.Type == bridgeproto.TypeHeartbeat {
			hbTime := nowFn()
			queueTelemetry(func() {
				currConn, isCurrent := h.registry.Get(conn.device.DeviceID)
				if !isCurrent || currConn != conn {
					return
				}
				dbCtx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
				defer cancel()
				_ = h.cfg.Store.RecordHeartbeat(dbCtx, conn.device.UserID, conn.device.DeviceID, hbTime)
			})
		}
		if env.Type == bridgeproto.TypeStatus {
			var statusPayload map[string]json.RawMessage
			if err := json.Unmarshal(env.Payload, &statusPayload); err == nil {
				if rawReady, ok := statusPayload["mcp_ready"]; ok {
					var ready bool
					if err := json.Unmarshal(rawReady, &ready); err == nil {
						mcpState := "stopped"
						if ready {
							mcpState = "ready"
						}
						queueTelemetry(func() {
							currConn, isCurrent := h.registry.Get(conn.device.DeviceID)
							if !isCurrent || currConn != conn {
								return
							}
							dbCtx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
							defer cancel()
							_ = h.cfg.Store.RecordStatus(dbCtx, conn.device.UserID, conn.device.DeviceID, mcpState)
						})
					}
				}
			}
		}
		if h.cfg.OnEnvelope != nil && env.Type != bridgeproto.TypeHeartbeat {
			h.cfg.OnEnvelope(conn.ctx, conn.device, env)
		}
	}
}

// reauthLoop revalidates the credential, device, binding, and entitlement
// while the connection is live so revocation disconnects active bridges.
func (h *Hub) reauthLoop(conn *Connection) {
	if h.cfg.ReauthInterval <= 0 {
		return
	}
	ticker := time.NewTicker(h.cfg.ReauthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-conn.ctx.Done():
			return
		case <-ticker.C:
			if _, err := h.auth.AuthenticateDigest(conn.ctx, conn.device.CredentialDigest); err != nil {
				conn.close(websocket.StatusPolicyViolation, reasonCredentialRevoked)
				return
			}
		}
	}
}
