package bridgeapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Enrollment endpoints on the gateway web API.
const (
	enrollBeginPath    = "/api/v1/device/enrollment/begin"
	enrollExchangePath = "/api/v1/device/enrollment/exchange"
)

// Enrollment exchange outcome limits. PollInterval bounds how fast the
// bridge polls a pending approval; the exchange itself decides 202/200/4xx.
const (
	defaultEnrollPollInterval = 2 * time.Second
	defaultEnrollHTTPTimeout  = 15 * time.Second
)

// EnrollConfig configures the device enrollment client: the bridge claims
// its identity, receives a pairing code and verification URL for the
// operator to approve in the browser, polls the code exchange until the
// approval lands, and saves the returned credential through the platform
// credential store. The API origin must be https; enrollment never relaxes
// TLS verification (a nil HTTPClient uses the default verified transport).
type EnrollConfig struct {
	APIBaseURL string
	DeviceID   string
	DeviceName string
	Hostname   string
	Platform   string

	BridgeVersion string

	// HTTPClient performs the API calls; nil selects the default transport
	// with full TLS verification.
	HTTPClient *http.Client
	// Credential persists the returned credential under the current
	// (interactive or service) identity; required.
	Credential CredentialStore
	// Output receives the operator-facing progress lines; required.
	Output io.Writer
	// PollInterval bounds the pending-exchange polling; zero selects the
	// default.
	PollInterval time.Duration
}

// RunEnroll performs the device enrollment flow:
//
//	POST /api/v1/device/enrollment/begin   — claim the device identity
//	print the verification URL + user code — the operator approves in a browser session
//	POST /api/v1/device/enrollment/exchange — poll until approved (202 → retry)
//	save the rkd_ credential via the credential store — never printed
//	print the enrolled device id
//
// Every failure is terminal: a rejected begin, an expired code (410), a
// non-2xx exchange, a save failure, or a cancelled/timed-out context.
func RunEnroll(ctx context.Context, cfg EnrollConfig) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultEnrollHTTPTimeout}
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = defaultEnrollPollInterval
	}

	claim, err := json.Marshal(map[string]string{
		"device_id":      cfg.DeviceID,
		"name":           cfg.DeviceName,
		"hostname":       cfg.Hostname,
		"platform":       cfg.Platform,
		"bridge_version": cfg.BridgeVersion,
	})
	if err != nil {
		return fmt.Errorf("bridgeapp: encode device claim: %w", err)
	}

	beginBody, err := enrollPostJSON(ctx, client, cfg.APIBaseURL+enrollBeginPath, claim)
	if err != nil {
		return fmt.Errorf("bridgeapp: begin enrollment: %w", err)
	}
	var begin struct {
		UserCode        string `json:"user_code"`
		VerificationURL string `json:"verification_url"`
	}
	if err := json.Unmarshal(beginBody, &begin); err != nil {
		return fmt.Errorf("bridgeapp: decode enrollment begin response: %w", err)
	}
	if strings.TrimSpace(begin.UserCode) == "" {
		return errors.New("bridgeapp: enrollment begin returned no user code")
	}
	fmt.Fprintf(cfg.Output, "Open the verification URL in your browser and approve this device:\n%s\n", begin.VerificationURL)
	fmt.Fprintf(cfg.Output, "Enrollment user code: %s\n", begin.UserCode)

	exchangePayload, err := json.Marshal(map[string]string{"device_code": begin.UserCode})
	if err != nil {
		return fmt.Errorf("bridgeapp: encode exchange request: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("bridgeapp: enrollment interrupted: %w", err)
		}
		body, status, err := enrollPostJSONStatus(ctx, client, cfg.APIBaseURL+enrollExchangePath, exchangePayload)
		if err != nil && status != http.StatusGone {
			return fmt.Errorf("bridgeapp: exchange enrollment: %w", err)
		}
		switch {
		case status == http.StatusAccepted:
			fmt.Fprintf(cfg.Output, "Waiting for approval…\n")
			if err := sleepContext(ctx, poll); err != nil {
				return fmt.Errorf("bridgeapp: enrollment interrupted: %w", err)
			}
			continue
		case status == http.StatusOK:
			var cred struct {
				DeviceCredential string `json:"device_credential"`
				DeviceID         string `json:"device_id"`
			}
			if err := json.Unmarshal(body, &cred); err != nil {
				return fmt.Errorf("bridgeapp: decode enrollment credential: %w", err)
			}
			if strings.TrimSpace(cred.DeviceCredential) == "" {
				return errors.New("bridgeapp: enrollment exchange returned no credential")
			}
			if err := cfg.Credential.Save([]byte(cred.DeviceCredential)); err != nil {
				return fmt.Errorf("bridgeapp: save enrollment credential: %w", err)
			}
			// The credential itself is never printed: it lives only in the
			// platform credential store.
			fmt.Fprintf(cfg.Output, "Enrollment complete. Device ID: %s\n", cred.DeviceID)
			return nil
		case status == http.StatusGone:
			return errors.New("bridgeapp: enrollment code expired before approval; run enrollment again")
		default:
			return fmt.Errorf("bridgeapp: enrollment exchange rejected with status %d: %s", status, sanitizeStatusBody(body))
		}
	}
}

func (cfg EnrollConfig) validate() error {
	parsed, err := url.Parse(cfg.APIBaseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return fmt.Errorf("bridgeapp: https API origin is required, got %q", cfg.APIBaseURL)
	}
	if strings.TrimSpace(cfg.DeviceID) == "" {
		return errors.New("bridgeapp: device id is required for enrollment")
	}
	if cfg.Credential == nil {
		return errors.New("bridgeapp: credential store is required for enrollment")
	}
	if cfg.Output == nil {
		return errors.New("bridgeapp: output writer is required for enrollment")
	}
	return nil
}

func enrollPostJSON(ctx context.Context, client *http.Client, target string, payload []byte) ([]byte, error) {
	body, _, err := enrollPostJSONStatus(ctx, client, target, payload)
	return body, err
}

func enrollPostJSONStatus(ctx context.Context, client *http.Client, target string, payload []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, resp.StatusCode, fmt.Errorf("status %d", resp.StatusCode)
	}
	return body, resp.StatusCode, nil
}

// sanitizeStatusBody trims an error body to a bounded, safe message.
func sanitizeStatusBody(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 200 {
		text = text[:200]
	}
	if text == "" {
		return "(no body)"
	}
	return text
}
