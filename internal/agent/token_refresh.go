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
var ErrNeedReLogin = errors.New("all tokens expired, please run 'agentserver login' again")

// EnsureValidToken checks for a valid OAuth access token, refreshing if needed.
// Returns the access token or ErrNeedReLogin if interactive auth is required.
func EnsureValidToken(serverURL string) (string, error) {
	return ensureValidToken(serverURL, DefaultCredentialsPath())
}

func ensureValidToken(serverURL, credPath string) (string, error) {
	creds, err := LoadCredentials(credPath)
	if err != nil || creds == nil {
		return "", ErrNeedReLogin
	}

	// Credentials were issued for a different server — need re-login.
	if creds.ServerURL != "" && creds.ServerURL != serverURL {
		return "", ErrNeedReLogin
	}

	// Token still valid.
	if creds.AccessToken != "" && time.Now().Before(creds.ExpiresAt) {
		return creds.AccessToken, nil
	}

	// Try refresh.
	if creds.RefreshToken == "" {
		return "", ErrNeedReLogin
	}

	newToken, err := refreshAccessToken(serverURL, creds.RefreshToken)
	if err != nil {
		return "", ErrNeedReLogin
	}

	creds.AccessToken = newToken.AccessToken
	creds.RefreshToken = newToken.RefreshToken
	creds.ExpiresAt = time.Now().Add(time.Duration(newToken.ExpiresIn) * time.Second)
	if err := SaveCredentials(credPath, creds); err != nil {
		log.Printf("warning: failed to persist refreshed credentials: %v", err)
	}

	return newToken.AccessToken, nil
}

func refreshAccessToken(serverURL, refreshToken string) (*TokenResponse, error) {
	tokenURL := strings.TrimRight(serverURL, "/") + "/api/oauth2/token"
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
