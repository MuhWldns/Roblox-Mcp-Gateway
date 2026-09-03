package appconfig

import (
	"strings"
	"testing"
	"time"
)

func TestLoadServerRejectsMissingProductionSettingsTogether(t *testing.T) {
	_, err := LoadServer(func(string) string { return "" })
	if err == nil {
		t.Fatal("LoadServer() error = nil, want an aggregated missing-settings error")
	}

	message := err.Error()
	for _, setting := range []string{"MYSQL_DSN", "TOKEN_PEPPER"} {
		if !strings.Contains(message, setting) {
			t.Errorf("LoadServer() error = %q, want it to identify missing %s", message, setting)
		}
	}
}

func TestLoadServerParsesValidTypedConfiguration(t *testing.T) {
	env := validServerEnv()

	got, err := LoadServer(envGetter(env))
	if err != nil {
		t.Fatalf("LoadServer() unexpected error: %v", err)
	}

	assertURL(t, "PublicAppURL", got.PublicAppURL.String(), "https://app.example.test")
	assertURL(t, "MCPResourceURL", got.MCPResourceURL.String(), "https://api.example.test/mcp")
	if got.ListenAddress != "127.0.0.1:8080" {
		t.Errorf("ListenAddress = %q, want %q", got.ListenAddress, "127.0.0.1:8080")
	}
	if got.MySQLDSN != "gateway:db-password@tcp(db.internal:3306)/robloxkit?parseTime=true" {
		t.Errorf("MySQLDSN was not preserved")
	}
	assertURL(t, "AllowedOrigin", got.AllowedOrigin.String(), "https://app.example.test")
	assertStrings(t, "TrustedProxies", got.TrustedProxies, []string{"127.0.0.1/32", "10.20.0.0/16"})
	if got.TokenPepper != "0123456789abcdef0123456789abcdef" {
		t.Errorf("TokenPepper was not preserved")
	}
	assertDuration(t, "HTTPReadTimeout", got.HTTPReadTimeout, 11*time.Second)
	assertDuration(t, "HTTPWriteTimeout", got.HTTPWriteTimeout, 17*time.Second)
	assertInt(t, "MySQLMaxOpenConns", got.MySQLMaxOpenConns, 24)
	assertInt(t, "MySQLMaxIdleConns", got.MySQLMaxIdleConns, 6)
	assertDuration(t, "BridgeHeartbeatInterval", got.BridgeHeartbeatInterval, 19*time.Second)
	assertDuration(t, "BridgeTimeout", got.BridgeTimeout, 43*time.Second)
	assertInt(t, "BridgeQueueLimit", got.BridgeQueueLimit, 128)
	assertInt(t, "BridgeMaxMessageBytes", got.BridgeMaxMessageBytes, 1048576)
}

func TestLoadServerRejectsInvalidURLsDurationsAndLimits(t *testing.T) {
	tests := []struct {
		name    string
		setting string
		value   string
	}{
		{name: "malformed public app URL", setting: "PUBLIC_APP_URL", value: "://missing-scheme"},
		{name: "malformed MCP resource URL", setting: "MCP_RESOURCE_URL", value: "https://[::1"},
		{name: "malformed allowed origin", setting: "ALLOWED_ORIGIN", value: "%not-an-origin"},
		{name: "malformed read timeout", setting: "HTTP_READ_TIMEOUT", value: "eventually"},
		{name: "non-positive write timeout", setting: "HTTP_WRITE_TIMEOUT", value: "0s"},
		{name: "malformed bridge timeout", setting: "BRIDGE_TIMEOUT", value: "forty"},
		{name: "non-positive heartbeat", setting: "BRIDGE_HEARTBEAT_INTERVAL", value: "-1s"},
		{name: "malformed open connection limit", setting: "MYSQL_MAX_OPEN_CONNS", value: "many"},
		{name: "non-positive open connection limit", setting: "MYSQL_MAX_OPEN_CONNS", value: "0"},
		{name: "negative idle connection limit", setting: "MYSQL_MAX_IDLE_CONNS", value: "-1"},
		{name: "non-positive bridge queue limit", setting: "BRIDGE_QUEUE_LIMIT", value: "0"},
		{name: "malformed bridge message limit", setting: "BRIDGE_MAX_MESSAGE_BYTES", value: "1MiB"},
		{name: "non-positive bridge message limit", setting: "BRIDGE_MAX_MESSAGE_BYTES", value: "-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validServerEnv()
			env[tt.setting] = tt.value

			_, err := LoadServer(envGetter(env))
			if err == nil {
				t.Fatalf("LoadServer() error = nil for %s=%q", tt.setting, tt.value)
			}
			if !strings.Contains(err.Error(), tt.setting) {
				t.Errorf("LoadServer() error = %q, want setting name %s", err, tt.setting)
			}
		})
	}
}
func TestLoadServerRejectsNonHTTPSPublicURLs(t *testing.T) {
	tests := []struct {
		setting string
		scheme  string
	}{
		{setting: "PUBLIC_APP_URL", scheme: "http"},
		{setting: "PUBLIC_APP_URL", scheme: "ftp"},
		{setting: "PUBLIC_APP_URL", scheme: "file"},
		{setting: "MCP_RESOURCE_URL", scheme: "http"},
		{setting: "MCP_RESOURCE_URL", scheme: "ftp"},
		{setting: "MCP_RESOURCE_URL", scheme: "file"},
		{setting: "ALLOWED_ORIGIN", scheme: "http"},
		{setting: "ALLOWED_ORIGIN", scheme: "ftp"},
		{setting: "ALLOWED_ORIGIN", scheme: "file"},
	}

	for _, tt := range tests {
		t.Run(tt.setting+"/"+tt.scheme, func(t *testing.T) {
			env := validServerEnv()
			env[tt.setting] = tt.scheme + "://example.test"

			_, err := LoadServer(envGetter(env))
			if err == nil {
				t.Fatalf("LoadServer() error = nil for %s=%q", tt.setting, env[tt.setting])
			}
			message := err.Error()
			if !strings.Contains(message, tt.setting) {
				t.Errorf("LoadServer() error = %q, want setting name %s", message, tt.setting)
			}
			if !strings.Contains(message, "https") {
				t.Errorf("LoadServer() error = %q, want HTTPS requirement", message)
			}
		})
	}
}

func TestLoadServerAggregatesIndependentValidationFailures(t *testing.T) {
	env := validServerEnv()
	env["PUBLIC_APP_URL"] = "://bad"
	env["HTTP_READ_TIMEOUT"] = "never"
	env["BRIDGE_QUEUE_LIMIT"] = "0"

	_, err := LoadServer(envGetter(env))
	if err == nil {
		t.Fatal("LoadServer() error = nil, want aggregated validation error")
	}

	message := err.Error()
	for _, setting := range []string{"PUBLIC_APP_URL", "HTTP_READ_TIMEOUT", "BRIDGE_QUEUE_LIMIT"} {
		if !strings.Contains(message, setting) {
			t.Errorf("LoadServer() error = %q, want it to include %s failure", message, setting)
		}
	}
}

func TestLoadServerErrorsDoNotExposeConfigurationValues(t *testing.T) {
	env := validServerEnv()
	env["PUBLIC_APP_URL"] = "https://url-user:url-secret@[::1"
	env["MYSQL_DSN"] = "dsn-user:dsn-secret@tcp(secret-db.internal:3306)/robloxkit"
	env["TOKEN_PEPPER"] = "pepper-secret-0123456789"

	_, err := LoadServer(envGetter(env))
	if err == nil {
		t.Fatal("LoadServer() error = nil, want invalid URL error")
	}

	message := err.Error()
	for _, secret := range []string{
		env["PUBLIC_APP_URL"],
		"url-secret",
		env["MYSQL_DSN"],
		"dsn-secret",
		env["TOKEN_PEPPER"],
	} {
		if strings.Contains(message, secret) {
			t.Errorf("LoadServer() error exposed sensitive configuration value %q: %q", secret, message)
		}
	}
	if !strings.Contains(message, "PUBLIC_APP_URL") {
		t.Errorf("LoadServer() error = %q, want sanitized setting name PUBLIC_APP_URL", message)
	}
}

func TestLoadBridgeRequiresWSS(t *testing.T) {
	env := validBridgeEnv()
	env["BRIDGE_GATEWAY_URL"] = "ws://api.example.test/bridge"

	_, err := LoadBridge(envGetter(env))
	if err == nil {
		t.Fatal("LoadBridge() error = nil for insecure ws gateway")
	}
	message := strings.ToLower(err.Error())
	if !strings.Contains(message, "bridge_gateway_url") || !strings.Contains(message, "wss") {
		t.Errorf("LoadBridge() error = %q, want BRIDGE_GATEWAY_URL and wss requirement", err)
	}
}

func TestLoadBridgeParsesValidTypedConfiguration(t *testing.T) {
	env := validBridgeEnv()

	got, err := LoadBridge(envGetter(env))
	if err != nil {
		t.Fatalf("LoadBridge() unexpected error: %v", err)
	}

	assertURL(t, "GatewayURL", got.GatewayURL.String(), "wss://api.example.test/bridge")
	if got.CredentialPath != `C:\ProgramData\RobloxKit\device.credential` {
		t.Errorf("CredentialPath = %q, want configured path", got.CredentialPath)
	}
	if got.MCPLauncherPath != `C:\Program Files\Roblox\RobloxStudioMCP.bat` {
		t.Errorf("MCPLauncherPath = %q, want configured trusted launcher path", got.MCPLauncherPath)
	}
	assertDuration(t, "ConnectTimeout", got.ConnectTimeout, 13*time.Second)
	assertDuration(t, "HeartbeatInterval", got.HeartbeatInterval, 23*time.Second)
	assertDuration(t, "ResponseTimeout", got.ResponseTimeout, 47*time.Second)
	assertInt(t, "QueueLimit", got.QueueLimit, 64)
	assertInt(t, "MaxMessageBytes", got.MaxMessageBytes, 2097152)
}

func TestLoadBridgeRejectsInvalidURLDurationsAndLimits(t *testing.T) {
	tests := []struct {
		name    string
		setting string
		value   string
	}{
		{name: "malformed gateway URL", setting: "BRIDGE_GATEWAY_URL", value: "wss://[::1"},
		{name: "wrong gateway scheme", setting: "BRIDGE_GATEWAY_URL", value: "https://api.example.test/bridge"},
		{name: "malformed connect timeout", setting: "BRIDGE_CONNECT_TIMEOUT", value: "soon"},
		{name: "non-positive heartbeat", setting: "BRIDGE_HEARTBEAT_INTERVAL", value: "0s"},
		{name: "negative response timeout", setting: "BRIDGE_RESPONSE_TIMEOUT", value: "-2s"},
		{name: "malformed queue limit", setting: "BRIDGE_QUEUE_LIMIT", value: "sixty-four"},
		{name: "non-positive queue limit", setting: "BRIDGE_QUEUE_LIMIT", value: "0"},
		{name: "malformed message limit", setting: "BRIDGE_MAX_MESSAGE_BYTES", value: "2MiB"},
		{name: "non-positive message limit", setting: "BRIDGE_MAX_MESSAGE_BYTES", value: "-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validBridgeEnv()
			env[tt.setting] = tt.value

			_, err := LoadBridge(envGetter(env))
			if err == nil {
				t.Fatalf("LoadBridge() error = nil for %s=%q", tt.setting, tt.value)
			}
			if !strings.Contains(err.Error(), tt.setting) {
				t.Errorf("LoadBridge() error = %q, want setting name %s", err, tt.setting)
			}
		})
	}
}

func TestLoadBridgeErrorsDoNotExposeConfigurationValues(t *testing.T) {
	env := validBridgeEnv()
	env["BRIDGE_GATEWAY_URL"] = "wss://device:gateway-secret@[::1"
	env["BRIDGE_CREDENTIAL_PATH"] = `C:\secret\device-token.credential`

	_, err := LoadBridge(envGetter(env))
	if err == nil {
		t.Fatal("LoadBridge() error = nil, want invalid URL error")
	}

	message := err.Error()
	for _, secret := range []string{env["BRIDGE_GATEWAY_URL"], "gateway-secret", env["BRIDGE_CREDENTIAL_PATH"]} {
		if strings.Contains(message, secret) {
			t.Errorf("LoadBridge() error exposed configuration value %q: %q", secret, message)
		}
	}
	if !strings.Contains(message, "BRIDGE_GATEWAY_URL") {
		t.Errorf("LoadBridge() error = %q, want sanitized setting name BRIDGE_GATEWAY_URL", message)
	}
}

func validServerEnv() map[string]string {
	return map[string]string{
		"PUBLIC_APP_URL":            "https://app.example.test",
		"MCP_RESOURCE_URL":          "https://api.example.test/mcp",
		"LISTEN_ADDRESS":            "127.0.0.1:8080",
		"MYSQL_DSN":                 "gateway:db-password@tcp(db.internal:3306)/robloxkit?parseTime=true",
		"ALLOWED_ORIGIN":            "https://app.example.test",
		"TRUSTED_PROXIES":           " 127.0.0.1/32,10.20.0.0/16 ",
		"TOKEN_PEPPER":              "0123456789abcdef0123456789abcdef",
		"HTTP_READ_TIMEOUT":         "11s",
		"HTTP_WRITE_TIMEOUT":        "17s",
		"MYSQL_MAX_OPEN_CONNS":      "24",
		"MYSQL_MAX_IDLE_CONNS":      "6",
		"BRIDGE_HEARTBEAT_INTERVAL": "19s",
		"BRIDGE_TIMEOUT":            "43s",
		"BRIDGE_QUEUE_LIMIT":        "128",
		"BRIDGE_MAX_MESSAGE_BYTES":  "1048576",
	}
}

func validBridgeEnv() map[string]string {
	return map[string]string{
		"BRIDGE_GATEWAY_URL":        "wss://api.example.test/bridge",
		"BRIDGE_CREDENTIAL_PATH":    `C:\ProgramData\RobloxKit\device.credential`,
		"BRIDGE_MCP_LAUNCHER":       `C:\Program Files\Roblox\RobloxStudioMCP.bat`,
		"BRIDGE_CONNECT_TIMEOUT":    "13s",
		"BRIDGE_HEARTBEAT_INTERVAL": "23s",
		"BRIDGE_RESPONSE_TIMEOUT":   "47s",
		"BRIDGE_QUEUE_LIMIT":        "64",
		"BRIDGE_MAX_MESSAGE_BYTES":  "2097152",
	}
}

func envGetter(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func assertURL(t *testing.T, name, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

func assertDuration(t *testing.T, name string, got, want time.Duration) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %s, want %s", name, got, want)
	}
}

func assertInt(t *testing.T, name string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %d, want %d", name, got, want)
	}
}

func assertStrings(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s length = %d, want %d (%q)", name, len(got), len(want), got)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %q, want %q", name, i, got[i], want[i])
		}
	}
}
