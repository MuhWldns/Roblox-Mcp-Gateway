package robloxauth

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

const (
	defaultProviderBaseURL = "https://apis.roblox.com"
	maxProviderResponse    = 1 << 20
	providerRequestTimeout = 10 * time.Second
)

type providerClient struct {
	httpClient   *http.Client
	baseURL      *url.URL
	clientID     string
	clientSecret string
	redirectURI  string
}

type providerTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
}

type providerUserInfo struct {
	Subject        string `json:"sub"`
	Username       string `json:"preferred_username"`
	DisplayName    string `json:"name"`
	LegacyUsername string `json:"nickname"`
}

func (c *providerClient) endpoint(path string) string {
	target := *c.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + path
	target.RawPath = ""
	target.RawQuery = ""
	target.Fragment = ""
	return target.String()
}

func (c *providerClient) exchange(ctx context.Context, code, verifier string) (providerTokens, error) {
	requestContext, cancel := context.WithTimeout(ctx, providerRequestTimeout)
	defer cancel()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {c.redirectURI},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(requestContext, http.MethodPost, c.endpoint("/oauth/v1/token"), strings.NewReader(form.Encode()))
	if err != nil {
		return providerTokens{}, fmt.Errorf("robloxauth: create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(c.clientID, c.clientSecret)

	var tokens providerTokens
	if err := c.doJSON(req, &tokens); err != nil {
		return providerTokens{}, fmt.Errorf("robloxauth: token exchange failed: %w", err)
	}
	if tokens.AccessToken == "" {
		return providerTokens{}, errors.New("robloxauth: token response missing access token")
	}
	return tokens, nil
}

func (c *providerClient) userInfo(ctx context.Context, accessToken string) (providerUserInfo, error) {
	requestContext, cancel := context.WithTimeout(ctx, providerRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestContext, http.MethodGet, c.endpoint("/oauth/v1/userinfo"), nil)
	if err != nil {
		return providerUserInfo{}, fmt.Errorf("robloxauth: create userinfo request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	var info providerUserInfo
	if err := c.doJSON(req, &info); err != nil {
		return providerUserInfo{}, fmt.Errorf("robloxauth: userinfo request failed: %w", err)
	}
	return info, nil
}

func (c *providerClient) doJSON(req *http.Request, target any) error {
	response, err := c.httpClient.Do(req)
	if err != nil {
		return errors.New("provider request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxProviderResponse))
		return fmt.Errorf("provider returned HTTP %d", response.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxProviderResponse+1))
	if err != nil {
		return errors.New("read provider response")
	}
	if len(body) > maxProviderResponse {
		return errors.New("provider response exceeded size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		return errors.New("provider returned invalid JSON")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("provider returned invalid JSON")
	}
	return nil
}
