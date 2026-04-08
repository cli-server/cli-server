package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrNeedReLogin indicates all tokens have expired and interactive re-auth is needed.
var ErrNeedReLogin = errors.New("all tokens expired, please run 'agentserver-agent login' again")

// EnsureValidCredentials checks sandbox credentials and refreshes if needed.
func EnsureValidCredentials(entry *RegistryEntry) error {
	return ensureValidCredentials(entry, DefaultCredentialsPath(), DefaultRegistryPath(), pingServer)
}

// ensureValidCredentials is the testable inner implementation.
func ensureValidCredentials(entry *RegistryEntry, credPath, regPath string, pingFn func(*RegistryEntry) error) error {
	// 1. Try existing sandbox credentials.
	if err := pingFn(entry); err == nil {
		return nil
	}

	// 2. Load OAuth credentials.
	creds, err := LoadCredentials(credPath)
	if err != nil || creds == nil {
		return ErrNeedReLogin
	}

	// 3. Try refresh_token.
	if creds.RefreshToken == "" {
		return ErrNeedReLogin
	}

	newToken, err := refreshAccessToken(entry.Server, creds.RefreshToken)
	if err != nil {
		return ErrNeedReLogin
	}

	// 4. Re-register with new access_token.
	regResp, err := registerAgentWithToken(entry.Server, newToken.AccessToken, entry.Name, entry.Type)
	if err != nil {
		return ErrNeedReLogin
	}

	// 5. Update entry and save.
	entry.SandboxID = regResp.SandboxID
	entry.TunnelToken = regResp.TunnelToken

	locked, lockErr := LockRegistry(regPath)
	if lockErr == nil {
		locked.Reg.Put(entry)
		if err := locked.Save(); err != nil {
			log.Printf("warning: failed to persist refreshed registry: %v", err)
		}
		locked.Close()
	}

	// 6. Update credentials.
	creds.AccessToken = newToken.AccessToken
	creds.RefreshToken = newToken.RefreshToken
	creds.ExpiresAt = time.Now().Add(time.Duration(newToken.ExpiresIn) * time.Second)
	if err := SaveCredentials(credPath, creds); err != nil {
		log.Printf("warning: failed to persist refreshed credentials: %v", err)
	}

	return nil
}

func refreshAccessToken(serverURL, refreshToken string) (*TokenResponse, error) {
	tokenURL := strings.TrimRight(serverURL, "/") + "/oauth2/token"
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {defaultClientID},
		"refresh_token": {refreshToken},
	}
	resp, err := http.PostForm(tokenURL, form)
	if err != nil {
		return nil, fmt.Errorf("refresh token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh token failed (status %d)", resp.StatusCode)
	}

	var tokenResp TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("decode refresh response: %w", err)
	}
	return &tokenResp, nil
}

// pingServer verifies sandbox credentials are still valid.
func pingServer(entry *RegistryEntry) error {
	req, err := http.NewRequest(http.MethodGet,
		strings.TrimRight(entry.Server, "/")+"/api/agent/tasks/poll?sandbox_id="+entry.SandboxID,
		nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+entry.TunnelToken)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("credentials expired (status %d)", resp.StatusCode)
	}
	return nil
}
