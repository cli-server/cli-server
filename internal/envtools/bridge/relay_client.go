package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// RelayClient mints HTTPS relay tickets on the codex-exec-gateway's
// /api/exec-gateway/relay/create endpoint. Used by CopyPathTool to
// route file bytes around the bridge ws path.
//
// nil-safe: when ExecGatewayInternalURL or InternalSecret is empty,
// CreateRelay returns a sentinel error and the caller falls back to
// the ws cat-pump path.
type RelayClient struct {
	baseURL     string // e.g. "http://codex-exec-gateway:6060" (internal cluster)
	secret      string // CXG_INTERNAL_SHARED_SECRET value (Bearer auth)
	workspaceID string
	httpClient  *http.Client
	logger      *slog.Logger
}

// ErrRelayDisabled is returned by CreateRelay when the gateway URL or
// internal secret is empty (config-disabled).
var ErrRelayDisabled = fmt.Errorf("relay: HTTP relay path disabled (no exec-gateway-internal-url or secret)")

func NewRelayClient(baseURL, secret, workspaceID string, logger *slog.Logger) *RelayClient {
	if logger == nil {
		logger = slog.Default()
	}
	return &RelayClient{
		baseURL:     strings.TrimRight(baseURL, "/"),
		secret:      secret,
		workspaceID: workspaceID,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		logger:      logger,
	}
}

// Enabled reports whether the relay path can be used (both URL +
// secret present).
func (c *RelayClient) Enabled() bool {
	return c != nil && c.baseURL != "" && c.secret != ""
}

// RelayTicket is the gateway's response to relay/create.
type RelayTicket struct {
	Ticket      string    `json:"ticket"`
	UploadURL   string    `json:"upload_url"`
	DownloadURL string    `json:"download_url"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type relayCreateBody struct {
	WorkspaceID string `json:"workspace_id"`
	SourceExeID string `json:"source_exe_id"`
	DestExeID   string `json:"dest_exe_id"`
	TTLSeconds  int    `json:"ttl_seconds,omitempty"`
	MaxBytes    int64  `json:"max_bytes,omitempty"`
}

// CreateRelay mints a ticket good for one PUT + one GET on the
// gateway's /relay/<ticket> endpoint.
func (c *RelayClient) CreateRelay(ctx context.Context, srcExeID, dstExeID string, ttl time.Duration, maxBytes int64) (*RelayTicket, error) {
	if !c.Enabled() {
		return nil, ErrRelayDisabled
	}
	body, _ := json.Marshal(relayCreateBody{
		WorkspaceID: c.workspaceID,
		SourceExeID: srcExeID,
		DestExeID:   dstExeID,
		TTLSeconds:  int(ttl.Seconds()),
		MaxBytes:    maxBytes,
	})
	req, err := http.NewRequestWithContext(ctx, "POST",
		c.baseURL+"/api/exec-gateway/relay/create",
		bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("relay/create: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.secret)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("relay/create: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("relay/create: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var t RelayTicket
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return nil, fmt.Errorf("relay/create: decode response: %w", err)
	}
	return &t, nil
}
