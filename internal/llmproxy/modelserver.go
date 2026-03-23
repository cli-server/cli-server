package llmproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// modelserverTokenCache is a thread-safe in-memory cache for modelserver access tokens.
type modelserverTokenCache struct {
	mu    sync.RWMutex
	items map[string]cachedToken
}

type cachedToken struct {
	accessToken string
	expiresAt   time.Time
	fetchedAt   time.Time
}

func newModelserverTokenCache() *modelserverTokenCache {
	return &modelserverTokenCache{
		items: make(map[string]cachedToken),
	}
}

// Get returns a cached token if it exists, was fetched less than 5 minutes ago,
// and has at least 60 seconds before expiry.
func (c *modelserverTokenCache) Get(workspaceID string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	tok, ok := c.items[workspaceID]
	if !ok {
		return "", false
	}

	now := time.Now()

	// Stale if fetched more than 5 minutes ago.
	if now.Sub(tok.fetchedAt) > 5*time.Minute {
		return "", false
	}

	// Stale if less than 60 seconds until expiry.
	if tok.expiresAt.Sub(now) < 60*time.Second {
		return "", false
	}

	return tok.accessToken, true
}

// Set stores a token in the cache.
func (c *modelserverTokenCache) Set(workspaceID, accessToken string, expiresAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items[workspaceID] = cachedToken{
		accessToken: accessToken,
		expiresAt:   expiresAt,
		fetchedAt:   time.Now(),
	}
}

// modelserverTokenResponse is the JSON response from the agentserver modelserver-token endpoint.
type modelserverTokenResponse struct {
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// fetchModelserverToken returns a valid modelserver access token for the given workspace,
// using the cache when possible.
func (s *Server) fetchModelserverToken(workspaceID string) (string, error) {
	// Check cache first.
	if token, ok := s.msTokenCache.Get(workspaceID); ok {
		return token, nil
	}

	// Cache miss — fetch from agentserver.
	url := fmt.Sprintf("%s/internal/workspaces/%s/modelserver-token", s.config.AgentserverURL, workspaceID)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	httpClient := s.httpClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("call agentserver: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("agentserver returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp modelserverTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("empty access token in response")
	}

	// Cache the token.
	s.msTokenCache.Set(workspaceID, tokenResp.AccessToken, tokenResp.ExpiresAt)

	return tokenResp.AccessToken, nil
}
