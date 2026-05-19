package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

// ErrUnauthorized is returned by ProxyTokenAuth.Verify for tokens that
// agentserver rejects. Callers MUST respond with HTTP 401 (never 5xx)
// so a misconfigured client recovers without retrying forever.
var ErrUnauthorized = errors.New("sdk auth: token rejected by agentserver")

// ProxyTokenAuth turns a sandbox proxyToken into (workspace_id, user_id)
// by calling agentserver's /internal/validate-proxy-token. Results are
// LRU-cached with a positive TTL and a shorter negative TTL.
type ProxyTokenAuth struct {
	agentserverURL string
	internalSecret string
	posTTL         time.Duration
	negTTL         time.Duration
	cache          *lru.Cache[string, cacheEntry]
	httpClient     *http.Client
}

type cacheEntry struct {
	workspaceID string
	userID      string
	expiresAt   time.Time
	negative    bool
}

func NewProxyTokenAuth(agentserverURL, internalSecret string, posTTL, negTTL time.Duration) *ProxyTokenAuth {
	cache, _ := lru.New[string, cacheEntry](1024)
	return &ProxyTokenAuth{
		agentserverURL: agentserverURL,
		internalSecret: internalSecret,
		posTTL:         posTTL,
		negTTL:         negTTL,
		cache:          cache,
		httpClient:     &http.Client{Timeout: 5 * time.Second},
	}
}

func (a *ProxyTokenAuth) Verify(ctx context.Context, token string) (workspaceID, userID string, err error) {
	if e, ok := a.cache.Get(token); ok && time.Now().Before(e.expiresAt) {
		if e.negative {
			return "", "", ErrUnauthorized
		}
		return e.workspaceID, e.userID, nil
	}

	body, _ := json.Marshal(map[string]string{"token": token})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.agentserverURL+"/internal/validate-proxy-token", bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("sdk auth: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Secret", a.internalSecret)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("sdk auth: agentserver unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		a.cache.Add(token, cacheEntry{negative: true, expiresAt: time.Now().Add(a.negTTL)})
		return "", "", ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("sdk auth: agentserver returned %d", resp.StatusCode)
	}
	var out struct {
		WorkspaceID string `json:"workspace_id"`
		UserID      string `json:"user_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", fmt.Errorf("sdk auth: decode response: %w", err)
	}
	a.cache.Add(token, cacheEntry{
		workspaceID: out.WorkspaceID,
		userID:      out.UserID,
		expiresAt:   time.Now().Add(a.posTTL),
	})
	return out.WorkspaceID, out.UserID, nil
}
