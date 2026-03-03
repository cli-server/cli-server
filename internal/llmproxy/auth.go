package llmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ValidateProxyToken calls the agentserver internal API to validate a sandbox proxy token.
// Returns nil (not error) if the token is invalid.
func (s *Server) ValidateProxyToken(ctx context.Context, proxyToken string) (*SandboxInfo, error) {
	reqBody, err := json.Marshal(map[string]string{"proxy_token": proxyToken})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := s.config.AgentserverURL + "/internal/validate-proxy-token"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	httpClient := s.httpClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call agentserver: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agentserver returned %d: %s", resp.StatusCode, string(body))
	}

	var info SandboxInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &info, nil
}
