package appconfig

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Production deployment constraints. LoadServer parses and sanity-checks the
// environment; the checks here are the stricter release gate a configuration
// must pass before the gateway is exposed publicly on a single VPS instance.
// docs/operations/vps-runbook.md documents the operator-facing side of every
// rule in this file.

const (
	// MinProductionPepperBytes is the production floor for TOKEN_PEPPER.
	// The pepper feeds token hashing, so short material weakens every
	// derived credential at once.
	MinProductionPepperBytes = 32

	// ProductionDrainBudget mirrors the server's graceful shutdown budget
	// (cmd/server shutdownBudget). A supervisor kill timeout must stay
	// strictly above it so the process drains on its own instead of dying
	// mid-request.
	ProductionDrainBudget = 30 * time.Second
)

// LoadProductionServer reads server configuration with LoadServer and applies
// the production gate. It is the load mode deployment tooling should use: a
// configuration that would run unsafely in production fails here, at load
// time, with every violation named.
func LoadProductionServer(getenv func(string) string) (Server, error) {
	config, err := LoadServer(getenv)
	if err != nil {
		return Server{}, err
	}
	if err := ValidateProduction(config); err != nil {
		return Server{}, err
	}
	return config, nil
}

// ValidateProduction rejects server configurations that are parseable but
// unsafe to expose: cleartext public URLs, wildcard or missing browser
// origins, untrusted or absent proxy entries, weak token pepper, and zeroed
// throughput budgets. The checks restate the LoadServer guarantees on the
// typed struct so a hand-built or mutated Server cannot bypass them.
func ValidateProduction(config Server) error {
	var validationErrors []error

	requireHTTPSURL(config.PublicAppURL, "PUBLIC_APP_URL", &validationErrors)
	requireHTTPSURL(config.MCPResourceURL, "MCP_RESOURCE_URL", &validationErrors)
	requireStrictOrigin(config.AllowedOrigin, &validationErrors)
	requireTrustedProxies(config.TrustedProxies, &validationErrors)
	requireProductionPepper(config.TokenPepper, &validationErrors)

	requirePositiveDuration(config.HTTPReadTimeout, "HTTP_READ_TIMEOUT", &validationErrors)
	requirePositiveDuration(config.HTTPWriteTimeout, "HTTP_WRITE_TIMEOUT", &validationErrors)
	requirePositiveDuration(config.BridgeHeartbeatInterval, "BRIDGE_HEARTBEAT_INTERVAL", &validationErrors)
	requirePositiveDuration(config.BridgeTimeout, "BRIDGE_TIMEOUT", &validationErrors)
	requirePositiveInt(config.MySQLMaxOpenConns, "MYSQL_MAX_OPEN_CONNS", &validationErrors)
	requirePositiveInt(config.BridgeQueueLimit, "BRIDGE_QUEUE_LIMIT", &validationErrors)
	requirePositiveInt(config.BridgeMaxMessageBytes, "BRIDGE_MAX_MESSAGE_BYTES", &validationErrors)

	if err := errors.Join(validationErrors...); err != nil {
		return fmt.Errorf("invalid production configuration: %w", err)
	}
	return nil
}

func requireHTTPSURL(u *url.URL, name string, validationErrors *[]error) {
	if u == nil {
		*validationErrors = append(*validationErrors, fmt.Errorf("%s is required", name))
		return
	}
	if !strings.EqualFold(u.Scheme, "https") {
		*validationErrors = append(*validationErrors, fmt.Errorf("%s must use https", name))
	}
}

// requireStrictOrigin rejects wildcard browser origins: a wildcard CORS
// origin on an authenticated dashboard lets any site read user data. The
// production origin must name the deployment host exactly.
func requireStrictOrigin(u *url.URL, validationErrors *[]error) {
	if u == nil {
		*validationErrors = append(*validationErrors, errors.New("ALLOWED_ORIGIN is required"))
		return
	}
	if !strings.EqualFold(u.Scheme, "https") {
		*validationErrors = append(*validationErrors, errors.New("ALLOWED_ORIGIN must use https"))
	}
	if strings.Contains(u.Host, "*") {
		*validationErrors = append(*validationErrors, errors.New("ALLOWED_ORIGIN must not contain a wildcard"))
	}
}

// requireTrustedProxies insists on an explicit, well-formed proxy list: the
// gateway derives client addresses from X-Forwarded-For only when the direct
// peer is in this list, so an empty or malformed list either breaks client
// identification or trusts the wrong hop.
func requireTrustedProxies(proxies []string, validationErrors *[]error) {
	if len(proxies) == 0 {
		*validationErrors = append(*validationErrors, errors.New("TRUSTED_PROXIES is required"))
		return
	}
	for _, proxy := range proxies {
		if _, err := netip.ParsePrefix(proxy); err != nil {
			*validationErrors = append(*validationErrors, fmt.Errorf("TRUSTED_PROXIES entry %q must be a CIDR prefix", proxy))
		}
	}
}

func requireProductionPepper(pepper string, validationErrors *[]error) {
	if len(pepper) < MinProductionPepperBytes {
		*validationErrors = append(*validationErrors, fmt.Errorf("TOKEN_PEPPER must be at least %d bytes", MinProductionPepperBytes))
	}
}

func requirePositiveDuration(d time.Duration, name string, validationErrors *[]error) {
	if d <= 0 {
		*validationErrors = append(*validationErrors, fmt.Errorf("%s must be a positive duration", name))
	}
}

func requirePositiveInt(n int, name string, validationErrors *[]error) {
	if n <= 0 {
		*validationErrors = append(*validationErrors, fmt.Errorf("%s must be a positive integer", name))
	}
}

// PM2 ecosystem validation patterns. The ecosystem file is a deployment
// artifact this repository owns, so targeted pattern checks are deterministic;
// every occurrence must satisfy the rule, not only the first one.
var (
	ecosystemExecMode    = regexp.MustCompile(`exec_mode\s*:\s*["']([^"']*)["']`)
	ecosystemInstances   = regexp.MustCompile(`instances\s*:\s*([^,}\n]+)`)
	ecosystemKillTimeout = regexp.MustCompile(`kill_timeout\s*:\s*(\d+)`)
	ecosystemRestart     = regexp.MustCompile(`restart_delay\s*:\s*(\d+)`)
	ecosystemAutorestart = regexp.MustCompile(`autorestart\s*:\s*(true|false)`)
)

// ValidateEcosystemFile checks a PM2 ecosystem file against the
// single-instance deployment contract: fork mode, exactly one instance,
// automatic restart with a delay, and a kill timeout strictly beyond the
// server's drain budget. Cluster mode or multiple instances would split the
// process-local Bridge hub, rate limiter, and WSS session state.
func ValidateEcosystemFile(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read pm2 ecosystem file: %w", err)
	}
	return ValidateEcosystem(string(content))
}

// ValidateEcosystem applies the single-instance contract to ecosystem file
// content.
func ValidateEcosystem(content string) error {
	var validationErrors []error

	modes := ecosystemExecMode.FindAllStringSubmatch(content, -1)
	if len(modes) == 0 {
		validationErrors = append(validationErrors, errors.New("exec_mode must be set to fork"))
	}
	for _, mode := range modes {
		if mode[1] != "fork" {
			validationErrors = append(validationErrors, fmt.Errorf("exec_mode must be fork, got %q", mode[1]))
		}
	}

	instanceSpecs := ecosystemInstances.FindAllStringSubmatch(content, -1)
	if len(instanceSpecs) == 0 {
		validationErrors = append(validationErrors, errors.New("instances must be set to 1"))
	}
	for _, spec := range instanceSpecs {
		count, err := strconv.Atoi(strings.TrimSpace(spec[1]))
		if err != nil || count != 1 {
			validationErrors = append(validationErrors, fmt.Errorf("instances must be exactly 1, got %q", strings.TrimSpace(spec[1])))
		}
	}

	killTimeouts := ecosystemKillTimeout.FindAllStringSubmatch(content, -1)
	if len(killTimeouts) == 0 {
		validationErrors = append(validationErrors, fmt.Errorf("kill_timeout must exceed the %s drain budget", ProductionDrainBudget))
	}
	for _, timeout := range killTimeouts {
		ms, err := strconv.ParseInt(timeout[1], 10, 64)
		if err != nil || time.Duration(ms)*time.Millisecond <= ProductionDrainBudget {
			validationErrors = append(validationErrors, fmt.Errorf("kill_timeout must exceed the %s drain budget", ProductionDrainBudget))
		}
	}

	restartDelays := ecosystemRestart.FindAllStringSubmatch(content, -1)
	if len(restartDelays) == 0 {
		validationErrors = append(validationErrors, errors.New("restart_delay must be a positive number of milliseconds"))
	}
	for _, delay := range restartDelays {
		if ms, err := strconv.ParseInt(delay[1], 10, 64); err != nil || ms <= 0 {
			validationErrors = append(validationErrors, errors.New("restart_delay must be a positive number of milliseconds"))
		}
	}

	autorestarts := ecosystemAutorestart.FindAllStringSubmatch(content, -1)
	if len(autorestarts) == 0 {
		validationErrors = append(validationErrors, errors.New("autorestart must be true"))
	}
	for _, restart := range autorestarts {
		if restart[1] != "true" {
			validationErrors = append(validationErrors, errors.New("autorestart must be true"))
		}
	}

	if err := errors.Join(validationErrors...); err != nil {
		return fmt.Errorf("invalid pm2 ecosystem configuration: %w", err)
	}
	return nil
}
