package appconfig

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The production gate sits on top of LoadServer: it accepts only
// configuration that is safe to expose publicly, on a single VPS instance,
// behind a trusted reverse proxy. The checks here are the release gate for
// docs/operations/vps-runbook.md — a configuration that passes LoadServer but
// fails ValidateProduction must never reach a public deployment.

func TestLoadProductionServerAcceptsValidEnvironment(t *testing.T) {
	env := validServerEnv()

	got, err := LoadProductionServer(envGetter(env))
	if err != nil {
		t.Fatalf("LoadProductionServer() unexpected error: %v", err)
	}

	assertURL(t, "PublicAppURL", got.PublicAppURL.String(), "https://app.example.test")
	assertURL(t, "AllowedOrigin", got.AllowedOrigin.String(), "https://app.example.test")
	assertStrings(t, "TrustedProxies", got.TrustedProxies, []string{"127.0.0.1/32", "10.20.0.0/16"})
	if got.TokenPepper != "0123456789abcdef0123456789abcdef" {
		t.Errorf("TokenPepper was not preserved")
	}
}

func TestValidateProductionRejectsInsecurePublicURLs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Server)
	}{
		{
			name: "http public app URL",
			mutate: func(s *Server) {
				s.PublicAppURL = mustParseURL(t, "http://app.example.test")
			},
		},
		{
			name: "ftp MCP resource URL",
			mutate: func(s *Server) {
				s.MCPResourceURL = mustParseURL(t, "ftp://api.example.test/mcp")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := productionServer(t)
			tt.mutate(&config)

			err := ValidateProduction(config)
			if err == nil {
				t.Fatal("ValidateProduction() error = nil, want insecure public URL rejection")
			}
			message := err.Error()
			if !strings.Contains(message, "https") {
				t.Errorf("ValidateProduction() error = %q, want an https requirement", message)
			}
		})
	}
}

func TestValidateProductionRejectsWildcardOrMissingBrowserOrigin(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Server)
	}{
		{
			name: "missing origin",
			mutate: func(s *Server) {
				s.AllowedOrigin = nil
			},
		},
		{
			name: "bare wildcard host",
			mutate: func(s *Server) {
				s.AllowedOrigin = mustParseURL(t, "https://*")
			},
		},
		{
			name: "subdomain wildcard host",
			mutate: func(s *Server) {
				s.AllowedOrigin = mustParseURL(t, "https://*.example.test")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := productionServer(t)
			tt.mutate(&config)

			err := ValidateProduction(config)
			if err == nil {
				t.Fatal("ValidateProduction() error = nil, want wildcard or missing origin rejection")
			}
			message := err.Error()
			if !strings.Contains(message, "ALLOWED_ORIGIN") {
				t.Errorf("ValidateProduction() error = %q, want the ALLOWED_ORIGIN setting named", message)
			}
		})
	}
}

func TestValidateProductionRejectsMissingOrUntrustedProxies(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Server)
	}{
		{
			name: "missing trusted proxies",
			mutate: func(s *Server) {
				s.TrustedProxies = nil
			},
		},
		{
			name: "empty trusted proxies",
			mutate: func(s *Server) {
				s.TrustedProxies = []string{}
			},
		},
		{
			name: "non-CIDR trusted proxy entry",
			mutate: func(s *Server) {
				s.TrustedProxies = []string{"127.0.0.1/32", "proxy.internal"}
			},
		},
		{
			name: "truncated CIDR mask",
			mutate: func(s *Server) {
				s.TrustedProxies = []string{"10.0.0.0/99"}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := productionServer(t)
			tt.mutate(&config)

			err := ValidateProduction(config)
			if err == nil {
				t.Fatal("ValidateProduction() error = nil, want trusted proxy rejection")
			}
			message := err.Error()
			if !strings.Contains(message, "TRUSTED_PROXIES") {
				t.Errorf("ValidateProduction() error = %q, want the TRUSTED_PROXIES setting named", message)
			}
		})
	}
}

func TestValidateProductionRejectsShortTokenPepper(t *testing.T) {
	config := productionServer(t)
	config.TokenPepper = strings.Repeat("p", 31)

	if err := ValidateProduction(config); err == nil {
		t.Fatal("ValidateProduction() error = nil, want a short token pepper rejection")
	} else if message := err.Error(); !strings.Contains(message, "TOKEN_PEPPER") {
		t.Errorf("ValidateProduction() error = %q, want the TOKEN_PEPPER setting named", message)
	}

	// Exactly 32 bytes is the production floor and must be accepted.
	config.TokenPepper = strings.Repeat("p", 32)
	if err := ValidateProduction(config); err != nil {
		t.Fatalf("ValidateProduction() unexpected error for a 32-byte pepper: %v", err)
	}
}

func TestValidateProductionRejectsZeroBudgets(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Server)
	}{
		{
			name: "zero bridge queue budget",
			mutate: func(s *Server) {
				s.BridgeQueueLimit = 0
			},
		},
		{
			name: "zero bridge message budget",
			mutate: func(s *Server) {
				s.BridgeMaxMessageBytes = 0
			},
		},
		{
			name: "zero database connection budget",
			mutate: func(s *Server) {
				s.MySQLMaxOpenConns = 0
			},
		},
		{
			name: "zero read timeout budget",
			mutate: func(s *Server) {
				s.HTTPReadTimeout = 0
			},
		},
		{
			name: "zero write timeout budget",
			mutate: func(s *Server) {
				s.HTTPWriteTimeout = 0
			},
		},
		{
			name: "zero heartbeat budget",
			mutate: func(s *Server) {
				s.BridgeHeartbeatInterval = 0
			},
		},
		{
			name: "zero bridge timeout budget",
			mutate: func(s *Server) {
				s.BridgeTimeout = 0
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := productionServer(t)
			tt.mutate(&config)

			err := ValidateProduction(config)
			if err == nil {
				t.Fatal("ValidateProduction() error = nil, want a zero budget rejection")
			}
			if message := err.Error(); !strings.Contains(message, "must be") {
				t.Errorf("ValidateProduction() error = %q, want a setting requirement", message)
			}
		})
	}
}

func TestLoadProductionServerRejectsShortTokenPepperEnvironment(t *testing.T) {
	env := validServerEnv()
	env["TOKEN_PEPPER"] = "short-pepper"

	_, err := LoadProductionServer(envGetter(env))
	if err == nil {
		t.Fatal("LoadProductionServer() error = nil, want a short token pepper rejection")
	}
	if message := err.Error(); !strings.Contains(message, "TOKEN_PEPPER") {
		t.Errorf("LoadProductionServer() error = %q, want the TOKEN_PEPPER setting named", message)
	}
}

func TestLoadProductionServerAggregatesProductionFailures(t *testing.T) {
	env := validServerEnv()
	env["ALLOWED_ORIGIN"] = "https://*"
	env["TOKEN_PEPPER"] = "too-short"
	env["TRUSTED_PROXIES"] = "proxy.internal"

	_, err := LoadProductionServer(envGetter(env))
	if err == nil {
		t.Fatal("LoadProductionServer() error = nil, want aggregated production failures")
	}

	message := err.Error()
	for _, setting := range []string{"ALLOWED_ORIGIN", "TOKEN_PEPPER", "TRUSTED_PROXIES"} {
		if !strings.Contains(message, setting) {
			t.Errorf("LoadProductionServer() error = %q, want it to include %s failure", message, setting)
		}
	}
}

// The PM2 ecosystem file is part of the production configuration surface: a
// second instance or cluster mode would split the process-local Bridge hub,
// rate limiter, and WSS session state. The repository file must always carry
// the single-instance contract.

func TestValidateEcosystemFileAcceptsRepositoryEcosystem(t *testing.T) {
	path := repositoryEcosystemPath(t)

	if err := ValidateEcosystemFile(path); err != nil {
		t.Fatalf("ValidateEcosystemFile(%s) unexpected error: %v", path, err)
	}
}

func TestValidateEcosystemFileRejectsMultiInstanceMarkers(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name: "cluster mode",
			content: `
module.exports = { apps: [{ name: "robloxkit-server", script: "./bin/robloxkit-server",
  exec_mode: "cluster", instances: 1, autorestart: true, restart_delay: 5000, kill_timeout: 40000 }] };
`,
		},
		{
			name: "max instances",
			content: `
module.exports = { apps: [{ name: "robloxkit-server", script: "./bin/robloxkit-server",
  exec_mode: "fork", instances: "max", autorestart: true, restart_delay: 5000, kill_timeout: 40000 }] };
`,
		},
		{
			name: "multiple instances",
			content: `
module.exports = { apps: [{ name: "robloxkit-server", script: "./bin/robloxkit-server",
  exec_mode: "fork", instances: 2, autorestart: true, restart_delay: 5000, kill_timeout: 40000 }] };
`,
		},
		{
			name: "missing exec_mode",
			content: `
module.exports = { apps: [{ name: "robloxkit-server", script: "./bin/robloxkit-server",
  instances: 1, autorestart: true, restart_delay: 5000, kill_timeout: 40000 }] };
`,
		},
		{
			name: "kill_timeout not beyond drain budget",
			content: `
module.exports = { apps: [{ name: "robloxkit-server", script: "./bin/robloxkit-server",
  exec_mode: "fork", instances: 1, autorestart: true, restart_delay: 5000, kill_timeout: 30000 }] };
`,
		},
		{
			name: "missing restart delay",
			content: `
module.exports = { apps: [{ name: "robloxkit-server", script: "./bin/robloxkit-server",
  exec_mode: "fork", instances: 1, autorestart: true, kill_timeout: 40000 }] };
`,
		},
		{
			name: "autorestart disabled",
			content: `
module.exports = { apps: [{ name: "robloxkit-server", script: "./bin/robloxkit-server",
  exec_mode: "fork", instances: 1, autorestart: false, restart_delay: 5000, kill_timeout: 40000 }] };
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ecosystem.config.cjs")
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatalf("write ecosystem fixture: %v", err)
			}

			if err := ValidateEcosystemFile(path); err == nil {
				t.Fatal("ValidateEcosystemFile() error = nil, want a multi-instance or lifecycle rejection")
			}
		})
	}
}

func TestValidateEcosystemFileRejectsMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.config.cjs")

	err := ValidateEcosystemFile(path)
	if err == nil {
		t.Fatal("ValidateEcosystemFile() error = nil for a missing file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ValidateEcosystemFile() error = %v, want an os.ErrNotExist wrapper", err)
	}
}

// productionServer returns a configuration that passes the production gate.
func productionServer(t *testing.T) Server {
	t.Helper()

	config, err := LoadServer(envGetter(validServerEnv()))
	if err != nil {
		t.Fatalf("LoadServer() unexpected error: %v", err)
	}
	return config
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return parsed
}

// repositoryEcosystemPath locates ecosystem.config.cjs at the module root.
// `go test` runs with the package directory as the working directory.
func repositoryEcosystemPath(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd(): %v", err)
	}
	return filepath.Join(wd, "..", "..", "ecosystem.config.cjs")
}
