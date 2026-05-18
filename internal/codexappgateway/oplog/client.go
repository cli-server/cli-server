package oplog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// Operation is what we POST to agentserver /internal/operations.
// Field shapes match internal/server/operations.go's request struct.
type Operation struct {
	ID          string  `json:"id"`
	WorkspaceID string  `json:"workspace_id"`
	UserID      *string `json:"user_id,omitempty"`
	Source      string  `json:"source"`
	ThreadID    *string `json:"thread_id,omitempty"`
	RequestID   *string `json:"request_id,omitempty"`

	EnvID         string          `json:"env_id"`
	Tool          string          `json:"tool"`
	Arguments     json.RawMessage `json:"arguments,omitempty"`
	ArgumentsMeta json.RawMessage `json:"arguments_meta,omitempty"`

	IsError       bool            `json:"is_error"`
	ResultSummary *string         `json:"result_summary,omitempty"`
	ResultMeta    json.RawMessage `json:"result_meta,omitempty"`

	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	DurationMs  int32     `json:"duration_ms"`
}

// Client posts Operations to agentserver. Submit is non-blocking; one
// background goroutine drains a bounded channel and POSTs each one.
type Client struct {
	url     string
	secret  string
	ch      chan Operation
	hc      *http.Client
	logger  *slog.Logger
	dropped uint64
	cancel  context.CancelFunc
	done    chan struct{}
}

// NewClient starts the background drainer immediately. Capacity bounds
// how many Operations can queue before Submit starts dropping.
func NewClient(url, secret string, capacity int) *Client {
	if capacity <= 0 {
		capacity = 1024
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		url:    url,
		secret: secret,
		ch:     make(chan Operation, capacity),
		hc:     &http.Client{Timeout: 5 * time.Second},
		logger: slog.Default().With("component", "oplog"),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go c.drain(ctx)
	return c
}

// Submit enqueues op. Never blocks. Drops on full channel and bumps the
// `dropped` counter, which a metrics exporter can read via Dropped().
func (c *Client) Submit(op Operation) {
	select {
	case c.ch <- op:
	default:
		atomic.AddUint64(&c.dropped, 1)
	}
}

// Dropped is the cumulative number of Submit calls that hit a full channel.
func (c *Client) Dropped() uint64 { return atomic.LoadUint64(&c.dropped) }

// Close stops the drainer. Already-queued ops are abandoned.
func (c *Client) Close() {
	c.cancel()
	<-c.done
}

func (c *Client) drain(ctx context.Context) {
	defer close(c.done)
	for {
		select {
		case <-ctx.Done():
			return
		case op := <-c.ch:
			c.post(ctx, op)
		}
	}
}

func (c *Client) post(ctx context.Context, op Operation) {
	buf, err := json.Marshal(op)
	if err != nil {
		c.logger.Warn("oplog: marshal failed", "id", op.ID, "err", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(buf))
	if err != nil {
		c.logger.Warn("oplog: build request failed", "err", err)
		return
	}
	req.Header.Set("X-Internal-Secret", c.secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		c.logger.Warn("oplog: POST failed", "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		c.logger.Warn("oplog: agentserver non-2xx",
			"status", resp.StatusCode, "body", string(body))
	}
}

// ListClient is the synchronous read-side: used by the gateway's
// operations/list RPC interceptor.
type ListClient struct {
	url    string
	secret string
	hc     *http.Client
}

func NewListClient(url, secret string) *ListClient {
	return &ListClient{
		url: url, secret: secret,
		hc: &http.Client{Timeout: 10 * time.Second},
	}
}

// List forwards filter params to agentserver and returns the raw JSON body.
func (c *ListClient) List(ctx context.Context, params map[string]string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	req.URL.RawQuery = q.Encode()
	req.Header.Set("X-Internal-Secret", c.secret)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("operations list: %d %s", resp.StatusCode, string(body))
	}
	return body, nil
}
