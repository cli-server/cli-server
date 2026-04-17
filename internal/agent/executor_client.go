package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
	"nhooyr.io/websocket"

	"github.com/agentserver/agentserver/internal/agent/executortools"
	"github.com/agentserver/agentserver/internal/tunnel"
)

const (
	// maxRequestBodyBytes caps the size of an incoming tool-execute request.
	// CC-side tool arguments are small JSON blobs; 16 MiB is generous.
	maxRequestBodyBytes = 16 << 20

	// streamReadTimeout bounds how long we wait for the request headers +
	// body to arrive before giving up. Tool execution itself is bounded by
	// ExecuteRequest.TimeoutMs (enforced in executortools).
	streamReadTimeout = 60 * time.Second

	// streamWriteTimeout bounds how long we wait to flush the response.
	streamWriteTimeout = 30 * time.Second

	// heartbeatInterval matches the registry's expected cadence.
	heartbeatInterval = 20 * time.Second

	// heartbeatHTTPTimeout prevents a stalled registry from piling up
	// heartbeat goroutines / blocking reconnects.
	heartbeatHTTPTimeout = 10 * time.Second
)

// ExecutorClient runs a tunnel to executor-registry and serves tool
// execution requests from cc-broker workers.
type ExecutorClient struct {
	session  *ExecutorSession
	workDir  string
	executor *executortools.ToolExecutor
	hbClient *http.Client
	stale    atomic.Bool
}

// NewExecutorClient constructs a new executor client bound to the given
// registry session and working directory.
func NewExecutorClient(sess *ExecutorSession, workDir string) *ExecutorClient {
	return &ExecutorClient{
		session:  sess,
		workDir:  workDir,
		executor: executortools.New(workDir),
		hbClient: &http.Client{Timeout: heartbeatHTTPTimeout},
	}
}

// Run maintains a persistent tunnel to executor-registry and reconnects
// with exponential backoff on disconnection. If the registry reports the
// session as stale (unauthorized / not found) the saved credentials are
// discarded and a new executor is registered before reconnecting.
func (c *ExecutorClient) Run(ctx context.Context) error {
	backoff := time.Second
	const maxBackoff = 60 * time.Second

	for {
		if c.stale.Load() {
			if err := c.reRegister(ctx); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				log.Printf("re-register failed: %v; retrying in %s", err, backoff)
				if !sleepCtx(ctx, backoff) {
					return ctx.Err()
				}
				backoff = nextBackoff(backoff, maxBackoff)
				continue
			}
			backoff = time.Second
		}

		connectedAt := time.Now()
		err := c.connectAndServe(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.Printf("tunnel disconnected: %v", err)
		}

		if time.Since(connectedAt) > 30*time.Second {
			backoff = time.Second
		}

		log.Printf("reconnecting in %s...", backoff)
		if !sleepCtx(ctx, backoff) {
			return ctx.Err()
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

// reRegister wipes the stale session file and obtains a fresh executor
// identity from the registry. Called from Run on the main loop goroutine
// only, so no locking is needed on c.session.
func (c *ExecutorClient) reRegister(ctx context.Context) error {
	log.Printf("session stale; re-registering with %s", c.session.ServerURL)
	if err := removeExecutorSession(c.session.ExecutorID); err != nil {
		log.Printf("warning: failed to remove stale session file: %v", err)
	}
	newSess, err := registerExecutorWithRegistry(c.session.ServerURL, c.session.Name, c.session.WorkspaceID)
	if err != nil {
		return err
	}
	if err := saveExecutorSession(newSess); err != nil {
		log.Printf("warning: failed to save new executor session: %v", err)
	}
	c.session = newSess
	c.stale.Store(false)
	log.Printf("re-registered: id=%s", newSess.ExecutorID)
	return nil
}

func (c *ExecutorClient) connectAndServe(ctx context.Context) error {
	wsURL := httpToWS(c.session.ServerURL) + "/api/tunnel/" + c.session.ExecutorID + "?token=" + c.session.TunnelToken

	log.Printf("connecting to %s", wsURL)

	ws, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound) {
			log.Printf("tunnel dial returned %d — marking session stale", resp.StatusCode)
			c.stale.Store(true)
		}
		return fmt.Errorf("ws dial: %w", err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "")

	log.Printf("tunnel connected (executor: %s)", c.session.ExecutorID)

	conn := tunnel.NewWSConn(ctx, ws)
	session, err := tunnel.ClientMux(conn)
	if err != nil {
		conn.Close()
		return fmt.Errorf("yamux session: %w", err)
	}
	defer session.Close()

	hbCtx, cancelHB := context.WithCancel(ctx)
	defer cancelHB()
	go c.heartbeatLoop(hbCtx, session)

	for {
		stream, err := session.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("accept stream: %w", err)
		}
		go c.handleStream(ctx, stream)
	}
}

// handleStream reads one HTTP request from the stream, dispatches it to the
// local tool executor, and writes back the response. Panics in the tool
// handler are caught so one malformed call can't take down the tunnel.
func (c *ExecutorClient) handleStream(ctx context.Context, stream net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic in tool handler: %v", r)
		}
	}()
	defer stream.Close()

	_ = stream.SetReadDeadline(time.Now().Add(streamReadTimeout))

	req, err := http.ReadRequest(bufio.NewReader(stream))
	if err != nil {
		if err != io.EOF {
			log.Printf("read request: %v", err)
		}
		return
	}
	defer req.Body.Close()

	reply := func(status int, body []byte) {
		_ = stream.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
		writeHTTPResponse(stream, status, body)
	}

	if req.Method != http.MethodPost || req.URL.Path != "/tool/execute" {
		reply(http.StatusNotFound, []byte(`{"error":"not found"}`))
		return
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, maxRequestBodyBytes+1))
	if err != nil {
		reply(http.StatusBadRequest, []byte(`{"error":"read body"}`))
		return
	}
	if len(body) > maxRequestBodyBytes {
		reply(http.StatusRequestEntityTooLarge, []byte(`{"error":"request body too large"}`))
		return
	}

	var execReq executortools.ExecuteRequest
	if err := json.Unmarshal(body, &execReq); err != nil {
		reply(http.StatusBadRequest, []byte(`{"error":"invalid body"}`))
		return
	}

	// Clear the read deadline: tool execution has its own timeout.
	_ = stream.SetReadDeadline(time.Time{})

	resp := c.executor.Execute(ctx, execReq)
	respBody, err := json.Marshal(resp)
	if err != nil {
		log.Printf("marshal tool response: %v", err)
		respBody = []byte(`{"output":"internal: failed to marshal response","exit_code":1}`)
	}
	reply(http.StatusOK, respBody)
}

// heartbeatLoop sends a heartbeat every heartbeatInterval (plus one
// immediately). If the registry reports the executor as stale it flags
// the client and closes the session so the accept loop unblocks.
func (c *ExecutorClient) heartbeatLoop(ctx context.Context, session *yamux.Session) {
	if c.sendHeartbeat(ctx, session) {
		return
	}
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if c.sendHeartbeat(ctx, session) {
				return
			}
		}
	}
}

// sendHeartbeat posts one heartbeat. Returns true if the caller should stop
// the heartbeat loop (the session was deemed stale and closed).
func (c *ExecutorClient) sendHeartbeat(ctx context.Context, session *yamux.Session) (stop bool) {
	info := collectAgentInfo("", c.workDir)
	infoJSON, err := json.Marshal(info)
	if err != nil {
		log.Printf("heartbeat: marshal sysinfo: %v", err)
		return false
	}
	body, err := json.Marshal(map[string]interface{}{
		"status":      "online",
		"system_info": json.RawMessage(infoJSON),
		"capabilities": map[string]interface{}{
			"tools":       []string{"Bash", "Read", "Edit", "Write", "Glob", "Grep", "LS"},
			"working_dir": c.workDir,
			"description": "Local machine executor",
		},
	})
	if err != nil {
		log.Printf("heartbeat: marshal body: %v", err)
		return false
	}

	url := c.session.ServerURL + "/api/executors/" + c.session.ExecutorID + "/heartbeat"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		log.Printf("heartbeat: build request: %v", err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.session.RegistryToken)

	resp, err := c.hbClient.Do(req)
	if err != nil {
		log.Printf("heartbeat failed: %v", err)
		return false
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		log.Printf("heartbeat returned %d — marking session stale and tearing down tunnel", resp.StatusCode)
		c.stale.Store(true)
		_ = session.Close()
		return true
	case http.StatusOK:
		return false
	default:
		respBody, _ := io.ReadAll(resp.Body)
		log.Printf("heartbeat returned %d: %s", resp.StatusCode, respBody)
		return false
	}
}

// writeHTTPResponse writes a minimal HTTP/1.1 response over the stream.
func writeHTTPResponse(w io.Writer, status int, body []byte) {
	resp := &http.Response{
		StatusCode:    status,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	_ = resp.Write(w)
}

// httpToWS converts http(s):// → ws(s):// preserving the remainder.
func httpToWS(u string) string {
	if strings.HasPrefix(u, "https://") {
		return "wss://" + strings.TrimPrefix(u, "https://")
	}
	if strings.HasPrefix(u, "http://") {
		return "ws://" + strings.TrimPrefix(u, "http://")
	}
	return u
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func nextBackoff(cur, max time.Duration) time.Duration {
	cur *= 2
	if cur > max {
		cur = max
	}
	return cur
}
