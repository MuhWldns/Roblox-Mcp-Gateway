package main

import (
	"context"
	"errors"
	"log"
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
	"robloxkit/internal/device"
	"robloxkit/internal/entitlement"
	"robloxkit/internal/health"
	"robloxkit/internal/httpserver"
	"robloxkit/internal/mcpoauth"
	"robloxkit/internal/mysqlstore"
	"robloxkit/internal/robloxauth"
	"robloxkit/internal/session"
)

const defaultSessionLifetime = 12 * time.Hour

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

func main() {
	config, err := appconfig.LoadServer(os.Getenv)
	if err != nil {
		log.Printf("startup failed: %v", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := mysqlstore.Open(ctx, config.MySQLDSN, mysqlstore.PoolConfig{
		MaxOpenConns: config.MySQLMaxOpenConns,
		MaxIdleConns: config.MySQLMaxIdleConns,
	})
	if err != nil {
		log.Printf("database open failed: %v", err)
		os.Exit(1)
	}
	defer db.Close()

	pepper := []byte(config.TokenPepper)
	clock := systemClock{}

	sessions := session.NewService(mysqlstore.NewSessionStore(db), pepper, sessionLifetime())
	identities := mysqlstore.NewIdentityStore(db)
	auditService := audit.NewService(mysqlstore.NewAuditStore(db))
	entitlements := entitlement.NewService(mysqlstore.NewEntitlementStore(db, clock, auditService), clock)
	deviceStore := mysqlstore.NewDeviceStore(db)

	enrollment, err := device.NewEnrollment(deviceStore, entitlements, pepper, time.Now)
	if err != nil {
		log.Printf("enrollment setup failed: %v", err)
		os.Exit(1)
	}
	enrollment.VerificationBaseURL = config.PublicAppURL.String()

	redirectURI := env("ROBLOX_REDIRECT_URI", config.PublicAppURL.String()+"/api/v1/auth/roblox/callback")
	flow, err := robloxauth.NewFlow(robloxauth.Config{
		ClientID:        env("ROBLOX_CLIENT_ID", ""),
		ClientSecret:    env("ROBLOX_CLIENT_SECRET", ""),
		RedirectURI:     redirectURI,
		ProviderBaseURL: env("ROBLOX_PROVIDER_BASE_URL", ""),
		Issuer:          env("ROBLOX_ISSUER", ""),
		JWKSURI:         env("ROBLOX_JWKS_URI", ""),
	})
	if err != nil {
		log.Printf("roblox login setup failed: %v", err)
		os.Exit(1)
	}
	robloxHandler := &robloxauth.Handler{
		Flow: flow, Identities: identities, Sessions: sessions,
		SuccessRedirect: "/download", SessionMaxAge: sessionLifetime(),
	}

	artifact := device.Artifact{
		Version:  env("BRIDGE_ARTIFACT_VERSION", "0.1.0"),
		Filename: env("BRIDGE_ARTIFACT_FILENAME", "RobloxBridge.exe"),
		Path:     env("BRIDGE_ARTIFACT_PATH", filepath.Join("bin", "RobloxBridge.exe")),
	}
	download, err := device.NewDownloadHandler(sessions, artifact)
	if err != nil {
		log.Printf("bridge artifact unavailable: %v", err)
		os.Exit(1)
	}
	downloadMetadata, err := device.NewDownloadMetadataHandler(sessions, artifact)
	if err != nil {
		log.Printf("bridge artifact unavailable: %v", err)
		os.Exit(1)
	}
	// The OAuth discovery documents share the gateway origin: the issuer is
	// the public MCP resource origin, so the /mcp challenge, the well-known
	// document locations, and the issuer claim always agree.
	resource := config.MCPResourceURL
	issuer := &url.URL{Scheme: resource.Scheme, Host: resource.Host}
	metadata, err := mcpoauth.NewMetadata(issuer, resource, mcpoauth.SupportedScopes)
	if err != nil {
		log.Printf("oauth metadata setup failed: %v", err)
		os.Exit(1)
	}

	dashboard := mysqlstore.NewDashboardStore(db, auditService)
	probes := health.NewHandler(db, nil)

	router, err := httpserver.NewRouter(httpserver.Config{
		Sessions:         sessions,
		RobloxAuth:       robloxHandler,
		IdentityReader:   deviceStore,
		Entitlements:     entitlements,
		Download:         download,
		DownloadMetadata: downloadMetadata,
		Enrollment:       enrollment,
		Dashboard:        dashboard,
		Health:           probes,
		Metadata:         &metadata,
		AllowedOrigin:    config.AllowedOrigin,
		StaticDir:        env("WEB_STATIC_DIR", ""),
	})
	if err != nil {
		log.Printf("router setup failed: %v", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:         config.ListenAddress,
		Handler:      router,
		ReadTimeout:  config.HTTPReadTimeout,
		WriteTimeout: config.HTTPWriteTimeout,
	}

	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("shutdown failed: %v", err)
		}
		close(shutdownDone)
	}()

	log.Printf("robloxkit server listening on %s", config.ListenAddress)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("server stopped: %v", err)
		os.Exit(1)
	}
	<-shutdownDone
}

func sessionLifetime() time.Duration {
	if parsed, err := time.ParseDuration(os.Getenv("SESSION_LIFETIME")); err == nil && parsed > 0 {
		return parsed
	}
	return defaultSessionLifetime
}
