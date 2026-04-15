package agentsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultClientID = "agentserver-agent-cli"
	defaultScopes   = "openid profile agent:register"
)

// RequestDeviceCode initiates the OAuth Device Flow by requesting a device
// code from the agentserver's device authorization endpoint.
func RequestDeviceCode(ctx context.Context, serverURL string) (*DeviceAuthResponse, error) {
	form := url.Values{
		"client_id": {defaultClientID},
		"scope":     {defaultScopes},
	}
	reqURL := strings.TrimRight(serverURL, "/") + "/api/oauth2/device/auth"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request device code: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("device auth failed (%d): %s", resp.StatusCode, body)
	}

	var result DeviceAuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode device auth response: %w", err)
	}
	return &result, nil
}

// PollForToken polls the token endpoint until the user completes
// authorization, the device code expires, or the context is cancelled.
func PollForToken(ctx context.Context, serverURL string, deviceResp *DeviceAuthResponse) (*TokenResponse, error) {
	tokenURL := strings.TrimRight(serverURL, "/") + "/api/oauth2/token"
	interval := time.Duration(deviceResp.Interval) * time.Second
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(deviceResp.ExpiresIn) * time.Second)

	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("authorization expired, please try again")
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}

		form := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"client_id":   {defaultClientID},
			"device_code": {deviceResp.DeviceCode},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, fmt.Errorf("create token request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			continue // Retry on network errors.
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			var tokenResp TokenResponse
			if err := json.Unmarshal(body, &tokenResp); err != nil {
				return nil, fmt.Errorf("decode token response: %w", err)
			}
			return &tokenResp, nil
		}

		// Parse error response.
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(body, &errResp)

		switch errResp.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "access_denied":
			return nil, fmt.Errorf("authorization denied by user")
		case "expired_token":
			return nil, fmt.Errorf("authorization expired, please try again")
		default:
			return nil, fmt.Errorf("token error: %s", errResp.Error)
		}
	}
}
