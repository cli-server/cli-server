// Package execgwclient provides an HTTP client for the codex-exec-gateway
// internal API. It is consumed by codex-app-gateway's buildConfig closure
// to fetch the set of currently-connected executors for a workspace before
// spawning a codex app-server subprocess.
package execgwclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/agentserver/agentserver/internal/codexexecgateway/execmodel"
)

// Client calls the codex-exec-gateway internal API.
type Client struct {
	internalURL string
	sharedSecret string
	httpClient  *http.Client
}

// NewClient constructs a Client. internalURL should be the base URL of
// the codex-exec-gateway service, e.g. "http://codex-exec-gateway:8090".
// sharedSecret is sent as a Bearer token in the Authorization header.
func NewClient(internalURL, sharedSecret string) *Client {
	return &Client{
		internalURL:  internalURL,
		sharedSecret: sharedSecret,
		httpClient:   &http.Client{Timeout: 5 * time.Second},
	}
}

// ListConnected fetches the executors that are (a) bound to workspaceID
// and (b) currently connected to the exec-gateway. The result is the
// intersection suitable for inclusion in a per-thread config manifest.
func (c *Client) ListConnected(ctx context.Context, workspaceID string) ([]execmodel.ConnectedExecutor, error) {
	u, err := url.Parse(c.internalURL + "/api/exec-gateway/connected")
	if err != nil {
		return nil, fmt.Errorf("execgwclient: parse URL: %w", err)
	}
	q := u.Query()
	q.Set("workspace_id", workspaceID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("execgwclient: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.sharedSecret)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execgwclient: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("execgwclient: unexpected status %d from exec-gateway", resp.StatusCode)
	}

	var result []execmodel.ConnectedExecutor
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("execgwclient: decode response: %w", err)
	}
	return result, nil
}
