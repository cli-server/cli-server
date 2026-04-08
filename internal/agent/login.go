package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mdp/qrterminal/v3"
	"github.com/pkg/browser"
)

const defaultClientID = "agentserver-agent-cli"
const defaultScopes = "openid profile agent:register"

// LoginOptions holds flags for the login command.
type LoginOptions struct {
	ServerURL       string
	Name            string
	Type            string // "opencode" or "claudecode"
	SkipOpenBrowser bool
}

// DeviceAuthResponse is the response from Hydra's device authorization endpoint.
type DeviceAuthResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// TokenResponse is the response from the token endpoint.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

// RegisterResponse is the response from the agent registration endpoint.
type RegisterResponse struct {
	SandboxID   string `json:"sandbox_id"`
	TunnelToken string `json:"tunnel_token"`
	ProxyToken  string `json:"proxy_token"`
	WorkspaceID string `json:"workspace_id"`
	ShortID     string `json:"short_id"`
}

// RunLogin executes the OAuth Device Flow login and agent registration.
func RunLogin(opts LoginOptions) error {
	if opts.ServerURL == "" {
		return fmt.Errorf("--server is required")
	}
	if opts.Name == "" {
		hostname, _ := os.Hostname()
		if hostname != "" {
			opts.Name = hostname
		} else {
			opts.Name = "Local Agent"
		}
	}
	if opts.Type == "" {
		opts.Type = "claudecode"
	}

	// 1. Request device authorization (via agentserver reverse proxy).
	deviceResp, err := requestDeviceCode(opts.ServerURL)
	if err != nil {
		return fmt.Errorf("device authorization failed: %w", err)
	}

	// 2. Display authentication info.
	verifyURL := deviceResp.VerificationURIComplete
	if verifyURL == "" {
		verifyURL = deviceResp.VerificationURI
	}
	fmt.Printf("\nTo authenticate, visit:\n  %s\n\n", verifyURL)
	if deviceResp.UserCode != "" {
		fmt.Printf("Or enter code: %s at %s\n\n", deviceResp.UserCode, deviceResp.VerificationURI)
	}

	// 3. Try opening browser; fall back to QR code.
	if !opts.SkipOpenBrowser {
		if err := browser.OpenURL(verifyURL); err != nil {
			log.Printf("Could not open browser: %v", err)
			showQRCode(verifyURL)
		}
	} else {
		showQRCode(verifyURL)
	}

	// 4. Poll for token.
	fmt.Println("Waiting for authentication...")
	tokenResp, err := pollForToken(opts.ServerURL, deviceResp)
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}
	fmt.Println("Authentication successful!")

	// 5. Register agent with access_token.
	regResp, err := registerAgentWithToken(opts.ServerURL, tokenResp.AccessToken, opts.Name, opts.Type)
	if err != nil {
		return fmt.Errorf("agent registration failed: %w", err)
	}

	// 6. Save credentials.
	credPath := DefaultCredentialsPath()
	if err := SaveCredentials(credPath, &Credentials{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
		Scopes:       strings.Split(tokenResp.Scope, " "),
	}); err != nil {
		log.Printf("Warning: failed to save credentials: %v", err)
	}

	// 7. Save registry entry.
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	locked, err := LockRegistry(DefaultRegistryPath())
	if err != nil {
		return fmt.Errorf("lock registry: %w", err)
	}
	defer locked.Close()

	entry := &RegistryEntry{
		Dir:         cwd,
		Server:      opts.ServerURL,
		SandboxID:   regResp.SandboxID,
		TunnelToken: regResp.TunnelToken,
		WorkspaceID: regResp.WorkspaceID,
		Name:        opts.Name,
		Type:        opts.Type,
	}
	locked.Reg.Put(entry)
	if err := locked.Save(); err != nil {
		return fmt.Errorf("save registry: %w", err)
	}

	fmt.Printf("Registered as '%s' in workspace '%s' (sandbox: %s)\n",
		opts.Name, regResp.WorkspaceID, regResp.SandboxID)
	return nil
}

func requestDeviceCode(serverURL string) (*DeviceAuthResponse, error) {
	form := url.Values{
		"client_id": {defaultClientID},
		"scope":     {defaultScopes},
	}
	resp, err := http.PostForm(strings.TrimRight(serverURL, "/")+"/api/oauth2/device/auth", form)
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

func pollForToken(serverURL string, deviceResp *DeviceAuthResponse) (*TokenResponse, error) {
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

		time.Sleep(interval)

		form := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"client_id":   {defaultClientID},
			"device_code": {deviceResp.DeviceCode},
		}
		resp, err := http.PostForm(tokenURL, form)
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

func registerAgentWithToken(serverURL, accessToken, name, agentType string) (*RegisterResponse, error) {
	bodyData, err := json.Marshal(map[string]string{"name": name, "type": agentType})
	if err != nil {
		return nil, fmt.Errorf("marshal register body: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost,
		strings.TrimRight(serverURL, "/")+"/api/agent/register",
		strings.NewReader(string(bodyData)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("register request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registration failed (%d): %s", resp.StatusCode, body)
	}

	var result RegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode register response: %w", err)
	}
	return &result, nil
}

func showQRCode(url string) {
	config := qrterminal.Config{
		Level:     qrterminal.L,
		Writer:    os.Stderr,
		BlackChar: qrterminal.BLACK,
		WhiteChar: qrterminal.WHITE,
		QuietZone: 1,
	}
	qrterminal.GenerateWithConfig(url, config)
}
