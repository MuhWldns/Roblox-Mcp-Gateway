package appconfig

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Server contains the settings required by the public gateway process.
type Server struct {
	PublicAppURL            *url.URL
	MCPResourceURL          *url.URL
	ListenAddress           string
	MySQLDSN                string
	AllowedOrigin           *url.URL
	TrustedProxies          []string
	TokenPepper             string
	HTTPReadTimeout         time.Duration
	HTTPWriteTimeout        time.Duration
	MySQLMaxOpenConns       int
	MySQLMaxIdleConns       int
	BridgeHeartbeatInterval time.Duration
	BridgeTimeout           time.Duration
	BridgeQueueLimit        int
	BridgeMaxMessageBytes   int
}

// Bridge contains the settings required by the local bridge process.
type Bridge struct {
	GatewayURL        *url.URL
	CredentialPath    string
	MCPLauncherPath   string
	ConnectTimeout    time.Duration
	HeartbeatInterval time.Duration
	ResponseTimeout   time.Duration
	QueueLimit        int
	MaxMessageBytes   int
}

// LoadServer reads and validates server configuration using getenv.
func LoadServer(getenv func(string) string) (Server, error) {
	var config Server
	var validationErrors []error

    config.PublicAppURL = parseURL(getenv, "PUBLIC_APP_URL", "https", &validationErrors)
    config.MCPResourceURL = parseURL(getenv, "MCP_RESOURCE_URL", "https", &validationErrors)
    config.ListenAddress = required(getenv, "LISTEN_ADDRESS", &validationErrors)
    config.MySQLDSN = required(getenv, "MYSQL_DSN", &validationErrors)
    config.AllowedOrigin = parseURL(getenv, "ALLOWED_ORIGIN", "https", &validationErrors)
	config.TrustedProxies = parseList(getenv, "TRUSTED_PROXIES", &validationErrors)
	config.TokenPepper = required(getenv, "TOKEN_PEPPER", &validationErrors)
	config.HTTPReadTimeout = parseDuration(getenv, "HTTP_READ_TIMEOUT", &validationErrors)
	config.HTTPWriteTimeout = parseDuration(getenv, "HTTP_WRITE_TIMEOUT", &validationErrors)
	config.MySQLMaxOpenConns = parsePositiveInt(getenv, "MYSQL_MAX_OPEN_CONNS", &validationErrors)
	config.MySQLMaxIdleConns = parseNonNegativeInt(getenv, "MYSQL_MAX_IDLE_CONNS", &validationErrors)
	config.BridgeHeartbeatInterval = parseDuration(getenv, "BRIDGE_HEARTBEAT_INTERVAL", &validationErrors)
	config.BridgeTimeout = parseDuration(getenv, "BRIDGE_TIMEOUT", &validationErrors)
	config.BridgeQueueLimit = parsePositiveInt(getenv, "BRIDGE_QUEUE_LIMIT", &validationErrors)
	config.BridgeMaxMessageBytes = parsePositiveInt(getenv, "BRIDGE_MAX_MESSAGE_BYTES", &validationErrors)

	if err := errors.Join(validationErrors...); err != nil {
		return Server{}, fmt.Errorf("invalid server configuration: %w", err)
	}
	return config, nil
}

// LoadBridge reads and validates local bridge configuration using getenv.
func LoadBridge(getenv func(string) string) (Bridge, error) {
	var config Bridge
	var validationErrors []error

	config.GatewayURL = parseURL(getenv, "BRIDGE_GATEWAY_URL", "wss", &validationErrors)
	config.CredentialPath = required(getenv, "BRIDGE_CREDENTIAL_PATH", &validationErrors)
	config.MCPLauncherPath = required(getenv, "BRIDGE_MCP_LAUNCHER", &validationErrors)
	config.ConnectTimeout = parseDuration(getenv, "BRIDGE_CONNECT_TIMEOUT", &validationErrors)
	config.HeartbeatInterval = parseDuration(getenv, "BRIDGE_HEARTBEAT_INTERVAL", &validationErrors)
	config.ResponseTimeout = parseDuration(getenv, "BRIDGE_RESPONSE_TIMEOUT", &validationErrors)
	config.QueueLimit = parsePositiveInt(getenv, "BRIDGE_QUEUE_LIMIT", &validationErrors)
	config.MaxMessageBytes = parsePositiveInt(getenv, "BRIDGE_MAX_MESSAGE_BYTES", &validationErrors)

	if err := errors.Join(validationErrors...); err != nil {
		return Bridge{}, fmt.Errorf("invalid bridge configuration: %w", err)
	}
	return config, nil
}

func required(getenv func(string) string, key string, validationErrors *[]error) string {
	value := strings.TrimSpace(getenv(key))
	if value == "" {
		*validationErrors = append(*validationErrors, fmt.Errorf("%s is required", key))
	}
	return value
}

func parseURL(getenv func(string) string, key, requiredScheme string, validationErrors *[]error) *url.URL {
	value := required(getenv, key, validationErrors)
	if value == "" {
		return nil
	}

	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		*validationErrors = append(*validationErrors, fmt.Errorf("%s must be a valid absolute URL", key))
		return nil
	}
	if requiredScheme != "" && !strings.EqualFold(parsed.Scheme, requiredScheme) {
		*validationErrors = append(*validationErrors, fmt.Errorf("%s must use %s", key, requiredScheme))
		return nil
	}
	return parsed
}

func parseDuration(getenv func(string) string, key string, validationErrors *[]error) time.Duration {
	value := required(getenv, key, validationErrors)
	if value == "" {
		return 0
	}

	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		*validationErrors = append(*validationErrors, fmt.Errorf("%s must be a positive duration", key))
		return 0
	}
	return parsed
}

func parsePositiveInt(getenv func(string) string, key string, validationErrors *[]error) int {
	return parseInt(getenv, key, 1, validationErrors)
}

func parseNonNegativeInt(getenv func(string) string, key string, validationErrors *[]error) int {
	return parseInt(getenv, key, 0, validationErrors)
}

func parseInt(getenv func(string) string, key string, minimum int, validationErrors *[]error) int {
	value := required(getenv, key, validationErrors)
	if value == "" {
		return 0
	}

	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minimum {
		qualifier := "non-negative"
		if minimum == 1 {
			qualifier = "positive"
		}
		*validationErrors = append(*validationErrors, fmt.Errorf("%s must be a %s integer", key, qualifier))
		return 0
	}
	return parsed
}

func parseList(getenv func(string) string, key string, validationErrors *[]error) []string {
	value := required(getenv, key, validationErrors)
	if value == "" {
		return nil
	}

	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item == "" {
			*validationErrors = append(*validationErrors, fmt.Errorf("%s must not contain empty entries", key))
			continue
		}
		items = append(items, item)
	}
	return items
}
