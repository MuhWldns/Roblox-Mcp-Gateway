package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"robloxkit/internal/appconfig"
	"robloxkit/internal/audit"
	"robloxkit/internal/bridgehub"
	"robloxkit/internal/device"
	"robloxkit/internal/entitlement"
	"robloxkit/internal/health"
	"robloxkit/internal/httpserver"
	"robloxkit/internal/mcpoauth"
	"robloxkit/internal/mysqlstore"
	"robloxkit/internal/robloxauth"
	"robloxkit/internal/session"
)

const (
	defaultSessionLifetime = 12 * time.Hour

	// shutdownBudget bounds the whole graceful teardown; it must stay
	// below any supervisor's kill timeout so the process exits on its own.
	shutdownBudget = 30 * time.Second
)

// systemClock pins policy evaluation to wall time.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

func env(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

// newLogger builds the structured JSON lifecycle logger. Every operational
// line the process emits is one JSON record on stderr; no event ever
// carries a DSN, credential, or token material.
func newLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// slogWriter adapts the standard library logger — the health probes' failure
// notes — onto the structured JSON logger.
type slogWriter struct {
	logger *slog.Logger
}

func (w slogWriter) Write(p []byte) (int, error) {
	if msg := strings.TrimSpace(string(p)); msg != "" {
		w.logger.Warn(msg)
	}
	return len(p), nil
}

func main() {
	logger := newLogger()

	config, err := appconfig.LoadServer(os.Getenv)
	if err != nil {
		logger.Error("startup failed", "error", err.Error())
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := mysqlstore.Open(ctx, config.MySQLDSN, mysqlstore.PoolConfig{
		MaxOpenConns: config.MySQLMaxOpenConns,
		MaxIdleConns: config.MySQLMaxIdleConns,
	})
	if err != nil {
		logger.Error("database open failed", "error", err.Error())
		os.Exit(1)
	}

	pepper := []byte(config.TokenPepper)
	clock := systemClock{}

	sessions := session.NewService(mysqlstore.NewSessionStore(db), pepper, sessionLifetime())
	identities := mysqlstore.NewIdentityStore(db)
	auditService := audit.NewService(mysqlstore.NewAuditStore(db))
	entitlements := entitlement.NewService(mysqlstore.NewEntitlementStore(db, clock, auditService), clock)
	deviceStore := mysqlstore.NewDeviceStore(db)

	enrollment, err := device.NewEnrollment(deviceStore, entitlements, pepper, time.Now)
	if err != nil {
		logger.Error("enrollment setup failed", "error", err.Error())
		os.Exit(1)
	}
	enrollment.VerificationBaseURL = config.PublicAppURL.String()

	redirectURI := env("ROBLOX_REDIRECT_URI", config.PublicAppURL.String()+"/api/v1/auth/roblox/callback")
	flow, err := robloxauth.NewFlow(robloxauth.Config{
		ClientID:        env("ROBLOX_CLIENT_ID", ""),
		ClientSecret:    config.RobloxClientSecret,
		RedirectURI:     redirectURI,
		ProviderBaseURL: env("ROBLOX_PROVIDER_BASE_URL", ""),
		Issuer:          env("ROBLOX_ISSUER", ""),
		JWKSURI:         env("ROBLOX_JWKS_URI", ""),
	})
	if err != nil {
		logger.Error("roblox login setup failed", "error", err.Error())
		os.Exit(1)
	}
	robloxHandler := &robloxauth.Handler{
		Flow: flow, Identities: identities, Sessions: sessions,
		SuccessRedirect: "/download", Logger: log.New(slogWriter{logger: logger}, "", 0), SessionMaxAge: sessionLifetime(),
	}

	artifact := device.Artifact{
		Version:  env("BRIDGE_ARTIFACT_VERSION", "0.1.0"),
		Filename: env("BRIDGE_ARTIFACT_FILENAME", "RobloxBridge.exe"),
		Path:     env("BRIDGE_ARTIFACT_PATH", filepath.Join("bin", "RobloxBridge.exe")),
	}
	download, err := device.NewDownloadHandler(sessions, artifact)
	if err != nil {
		logger.Error("bridge artifact unavailable", "error", err.Error())
		os.Exit(1)
	}
	downloadMetadata, err := device.NewDownloadMetadataHandler(sessions, artifact)
	if err != nil {
		logger.Error("bridge artifact unavailable", "error", err.Error())
		os.Exit(1)
	}
	// The OAuth discovery documents share the gateway origin: the issuer is
	// the public MCP resource origin, so the /mcp challenge, the well-known
	// document locations, and the issuer claim always agree.
	resource := config.MCPResourceURL
	issuer := &url.URL{Scheme: resource.Scheme, Host: resource.Host}
	metadata, err := mcpoauth.NewMetadata(issuer, resource, mcpoauth.SupportedScopes)
	if err != nil {
		logger.Error("oauth metadata setup failed", "error", err.Error())
		os.Exit(1)
	}

	dashboard := mysqlstore.NewDashboardStore(db, auditService, pepper)
	oauthStore := mysqlstore.NewOAuthStore(db)
	// The readiness gate wraps the pool ping: while it is open, probes
	// reflect the database; once shutdown marks it unready, every probe
	// answers unavailable immediately without touching the pool.
	gate := health.NewGate(db, logger)
	probes := health.NewHandler(gate, log.New(slogWriter{logger: logger}, "", 0))

	// The Bridge hub serves the device-authenticated /bridge WebSocket
	// mount. Relayed MCP tool delivery lands with the connector gateway
	// wiring; the hub alone already carries hello, heartbeat, and
	// revocation traffic.
	hub, err := bridgehub.NewHub(bridgehub.Config{
		Store:             bridgehub.NewSQLStore(db),
		Entitlements:      entitlements,
		Pepper:            pepper,
		HeartbeatInterval: config.BridgeHeartbeatInterval,
		HeartbeatTimeout:  config.BridgeTimeout,
		QueueDepth:        config.BridgeQueueLimit,
		MaxEnvelopeBytes:  config.BridgeMaxMessageBytes,
	})
	if err != nil {
		logger.Error("bridge hub setup failed", "error", err.Error())
		os.Exit(1)
	}

	// The work gate fronts the MCP and WSS mounts: shutdown refuses new
	// work through it and drains admitted MCP requests before the hub
	// closes.
	work := httpserver.NewWorkGate()

	// Endpoint rate limits: one conservative default per class, keyed by
	// remote host for unauthenticated endpoints and by session user for
	// admin executes. The /mcp class is an outer brake on top of the
	// per-grant limiter inside the MCP gateway itself.
	limits, err := httpserver.NewLimiter(httpserver.LimiterConfig{
		Budgets: map[httpserver.Class]httpserver.Budget{
			httpserver.ClassLogin:  {Burst: 10, Refill: 10, Interval: time.Minute},
			httpserver.ClassOAuth:  {Burst: 20, Refill: 20, Interval: time.Minute},
			httpserver.ClassEnroll: {Burst: 20, Refill: 20, Interval: time.Minute},
			httpserver.ClassWSS:    {Burst: 10, Refill: 10, Interval: time.Minute},
			httpserver.ClassAdmin:  {Burst: 20, Refill: 20, Interval: time.Minute},
			httpserver.ClassMCP:    {Burst: 120, Refill: 120, Interval: time.Minute, MaxInFlight: 8},
		},
	})
	if err != nil {
		logger.Error("rate limiter setup failed", "error", err.Error())
		os.Exit(1)
	}

	// The administration surface is enabled unconditionally; the configured
	// ADMIN_USER_IDS decide who may execute. An empty list leaves every
	// endpoint answering 403.
	router, err := httpserver.NewRouter(httpserver.Config{
		Limits: limits,

		Sessions:         sessions,
		RobloxAuth:       robloxHandler,
		IdentityReader:   deviceStore,
		Entitlements:     entitlements,
		Download:         download,
		DownloadMetadata: downloadMetadata,
		Enrollment:       enrollment,
		Dashboard:        dashboard,
		Admin: &httpserver.AdminConfig{
			Entitlements: entitlements,
			OAuth:        oauthStore,
			AdminUsers:   splitAndTrim(env("ADMIN_USER_IDS", ""), ","),
		},
		Health:        probes,
		Metadata:      &metadata,
		Bridge:        work.WSS(hub),
		AllowedOrigin: config.AllowedOrigin,
		StaticDir:     env("WEB_STATIC_DIR", ""),
	})
	if err != nil {
		logger.Error("router setup failed", "error", err.Error())
		os.Exit(1)
	}

	// The lifecycle controller owns the committed teardown order: mark
	// readiness unready, refuse new MCP/WSS work, drain and fail pending
	// relay calls, close the Bridge hub, close the HTTP kernel, and close
	// the MySQL pool last.
	server, err := httpserver.NewServer(httpserver.ServerConfig{
		Addr:         config.ListenAddress,
		Handler:      router,
		ReadTimeout:  config.HTTPReadTimeout,
		WriteTimeout: config.HTTPWriteTimeout,
		Gate:         gate,
		Work:         work,
		Hub:          hub,
		Pool:         db,
		Logger:       logger,
	})
	if err != nil {
		logger.Error("server setup failed", "error", err.Error())
		os.Exit(1)
	}

	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		logger.Info("shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown failed", "error", err.Error())
		}
		close(shutdownDone)
	}()

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped", "error", err.Error())
		os.Exit(1)
	}
	<-shutdownDone
	logger.Info("server exited cleanly")
}

func sessionLifetime() time.Duration {
	if parsed, err := time.ParseDuration(os.Getenv("SESSION_LIFETIME")); err == nil && parsed > 0 {
		return parsed
	}
	return defaultSessionLifetime
}

// splitAndTrim splits s by sep and trims whitespace from every element.
// An empty input produces a nil slice, not ["\"].
func splitAndTrim(s, sep string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
