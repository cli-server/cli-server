package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"nhooyr.io/websocket"

	"github.com/agentserver/agentserver/internal/codexappgateway/approvalfilter"
)

// Conn is one loopback ws to a codex app-server subprocess. Safe for
// concurrent Turn() / StartThread() calls — internally serializes
// writes and demuxes responses + turn/completed notifications.
type Conn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
	nextID  atomic.Int64

	mu           sync.Mutex
	pendingResp  map[int64]chan rpcResponse   // request id → 1-buffered chan
	pendingTurns map[string]chan turnPayload  // turn id → 1-buffered chan
	itemsByTurn  map[string][]json.RawMessage // turn id → accumulated item/completed payloads

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  atomic.Value // stores *errHolder, set when reader exits
}

// errHolder wraps an error so atomic.Value always sees the same concrete type.
type errHolder struct{ err error }

// Dial opens a fresh ws, performs the codex initialize / initialized
// handshake, and starts the reader goroutine. Caller must Close().
func Dial(ctx context.Context, wsURL string) (*Conn, error) {
	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", wsURL, err)
	}
	ws.SetReadLimit(64 << 20)

	c := &Conn{
		ws:           ws,
		pendingResp:  make(map[int64]chan rpcResponse),
		pendingTurns: make(map[string]chan turnPayload),
		itemsByTurn:  make(map[string][]json.RawMessage),
		closed:       make(chan struct{}),
	}

	// Send initialize synchronously (no reader yet, so we read inline).
	id := c.nextID.Add(1)
	if err := c.writeJSON(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: "initialize", Params: json.RawMessage(`{"clientInfo":{"name":"agentserver-codex-broker","version":"0.1.0"},"capabilities":{}}`)}); err != nil {
		ws.Close(websocket.StatusInternalError, "")
		return nil, fmt.Errorf("initialize: %w", err)
	}
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			ws.Close(websocket.StatusInternalError, "")
			return nil, fmt.Errorf("initialize read: %w", err)
		}
		var resp rpcResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			ws.Close(websocket.StatusInternalError, "")
			return nil, fmt.Errorf("initialize decode: %w", err)
		}
		if resp.ID != nil && *resp.ID == id {
			if resp.Error != nil {
				ws.Close(websocket.StatusInternalError, "")
				return nil, fmt.Errorf("initialize rpc error: %s", resp.Error.Message)
			}
			break
		}
	}
	// initialized (notification).
	if err := c.writeJSON(ctx, rpcRequest{JSONRPC: "2.0", Method: "initialized", Params: json.RawMessage(`{}`)}); err != nil {
		ws.Close(websocket.StatusInternalError, "")
		return nil, fmt.Errorf("initialized: %w", err)
	}

	go c.readLoop()
	return c, nil
}

// readLoop consumes every inbound frame and routes it: rpc responses
// to pendingResp[id]; turn/completed notifications to pendingTurns;
// approval requests get auto-replied; everything else is dropped.
func (c *Conn) readLoop() {
	defer c.failAllPending(errors.New("connection closed"))

	// One context + one watcher goroutine for the connection lifetime.
	// Previously a new watcher was spawned per frame; cancel() does not
	// unblock <-c.closed, so goroutines accumulated one-per-frame.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-c.closed
		cancel()
	}()

	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			c.closeErr.CompareAndSwap(nil, &errHolder{err})
			return
		}
		c.dispatchFrame(data)
	}
}

func (c *Conn) dispatchFrame(data []byte) {
	var f rpcResponse // shape covers both response and notification
	if err := json.Unmarshal(data, &f); err != nil {
		return
	}
	if f.ID != nil && f.Method == "" {
		c.deliverResponse(*f.ID, f)
		return
	}
	// Notification or server request.
	if f.ID != nil && approvalfilter.IsApproval(f.Method) {
		_ = c.writeJSON(context.Background(), rpcResponse{
			JSONRPC: "2.0", ID: f.ID, Result: approvalfilter.Reply(f.Method),
		})
		return
	}
	if f.Method == "item/completed" {
		// Codex emits items incrementally via item/completed; turn/completed's
		// Turn.items is empty (items_view: NotLoaded). Accumulate so we can
		// inject the items into the final Turn payload at delivery time.
		var p itemCompletedParams
		if err := json.Unmarshal(f.Params, &p); err != nil {
			return
		}
		if p.TurnID != "" && len(p.Item) > 0 {
			c.mu.Lock()
			c.itemsByTurn[p.TurnID] = append(c.itemsByTurn[p.TurnID], p.Item)
			c.mu.Unlock()
		}
		return
	}
	if f.Method == "turn/completed" {
		var p turnCompletedParams
		if err := json.Unmarshal(f.Params, &p); err != nil {
			return
		}
		c.deliverTurn(p.Turn.ID, p.Turn)
		return
	}
	// Unknown server-side request (id-bearing, method not in our
	// approval allowlist): reply with a JSON-RPC method-not-found error
	// so codex doesn't block waiting for a response it'll never get.
	// Silent drop would cause every subsequent Turn on this conn to
	// time out — the prod symptom that led to commit 322c2db.
	if f.ID != nil && f.Method != "" {
		log.Printf("broker: unhandled server request method=%q id=%d — replying method-not-found", f.Method, *f.ID)
		_ = c.writeJSON(context.Background(), rpcResponse{
			JSONRPC: "2.0", ID: f.ID,
			Error: &rpcError{Code: -32601, Message: "method not implemented by agentserver broker: " + f.Method},
		})
		return
	}
	// Drop genuine notifications (no id) silently — codex won't block on them.
}

func (c *Conn) deliverResponse(id int64, resp rpcResponse) {
	c.mu.Lock()
	ch, ok := c.pendingResp[id]
	delete(c.pendingResp, id)
	c.mu.Unlock()
	if ok {
		ch <- resp
	}
}

func (c *Conn) deliverTurn(turnID string, payload turnPayload) {
	c.mu.Lock()
	ch, ok := c.pendingTurns[turnID]
	items := c.itemsByTurn[turnID]
	delete(c.pendingTurns, turnID)
	delete(c.itemsByTurn, turnID)
	c.mu.Unlock()
	if !ok {
		return
	}
	// Inject the accumulated item/completed payloads into Turn.items.
	// turn/completed's Turn.items is empty in codex's v2 protocol
	// (TurnItemsView::NotLoaded); the items arrived as separate
	// item/completed notifications. Without this merge, REST callers
	// see an empty items list and can't pull the agentMessage text.
	if len(items) > 0 {
		if merged, err := mergeItemsIntoTurnRaw(payload.Raw, items); err == nil {
			payload.Raw = merged
		}
	}
	ch <- payload
}

// mergeItemsIntoTurnRaw replaces the "items" field of a codex Turn JSON
// payload with the supplied items slice and returns the re-serialized
// bytes. The original payload is parsed into an ordered map so unknown
// fields and field order are preserved across codex protocol updates.
func mergeItemsIntoTurnRaw(raw json.RawMessage, items []json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw, err
	}
	itemsRaw, err := json.Marshal(items)
	if err != nil {
		return raw, err
	}
	m["items"] = itemsRaw
	// Caller's lastAgentMessageText scans by item type, so this is
	// sufficient — itemsView still says "notLoaded" but we don't
	// promise to update it.
	return json.Marshal(m)
}

func (c *Conn) failAllPending(err error) {
	c.mu.Lock()
	for id, ch := range c.pendingResp {
		close(ch)
		delete(c.pendingResp, id)
	}
	for tid, ch := range c.pendingTurns {
		close(ch)
		delete(c.pendingTurns, tid)
	}
	for tid := range c.itemsByTurn {
		delete(c.itemsByTurn, tid)
	}
	c.mu.Unlock()
	c.closeErr.CompareAndSwap(nil, &errHolder{err})
}

// Turn sends turn/start and blocks until the matching turn/completed
// notification arrives or timeout elapses. Returns the raw codex Turn
// JSON for verbatim REST passthrough.
func (c *Conn) Turn(ctx context.Context, threadID string, callerParams json.RawMessage, timeout time.Duration) (json.RawMessage, error) {
	mergedParams, err := mergeTurnParams(threadID, callerParams)
	if err != nil {
		return nil, fmt.Errorf("merge params: %w", err)
	}

	id := c.nextID.Add(1)
	respCh := make(chan rpcResponse, 1)
	c.mu.Lock()
	c.pendingResp[id] = respCh
	c.mu.Unlock()

	if err := c.writeJSON(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: "turn/start", Params: mergedParams}); err != nil {
		c.mu.Lock()
		delete(c.pendingResp, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("write turn/start: %w", err)
	}

	resp, ok := waitResp(ctx, respCh)
	if !ok {
		// Either readLoop died (channel closed) or caller's ctx cancelled.
		// Remove our registration so the reader doesn't deliver into an
		// abandoned channel and the map entry doesn't persist until Close().
		// If deliverResponse already deleted it, this delete is a no-op.
		c.mu.Lock()
		delete(c.pendingResp, id)
		c.mu.Unlock()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, c.closeErrOr(errors.New("connection closed before turn/start response"))
	}
	if resp.Error != nil {
		return nil, &TurnRPCError{Code: resp.Error.Code, Message: resp.Error.Message, Data: resp.Error.Data}
	}
	var startResp turnStartResponse
	if err := json.Unmarshal(resp.Result, &startResp); err != nil {
		return nil, fmt.Errorf("decode turn/start result: %w", err)
	}
	if startResp.Turn.ID == "" {
		return nil, fmt.Errorf("turn/start result missing turn.id")
	}

	turnCh := make(chan turnPayload, 1)
	c.mu.Lock()
	c.pendingTurns[startResp.Turn.ID] = turnCh
	c.mu.Unlock()

	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case payload, open := <-turnCh:
		if !open {
			return nil, c.closeErrOr(errors.New("connection closed before turn/completed"))
		}
		return payload.Raw, nil
	case <-tctx.Done():
		c.mu.Lock()
		delete(c.pendingTurns, startResp.Turn.ID)
		delete(c.itemsByTurn, startResp.Turn.ID)
		c.mu.Unlock()
		// Best-effort interrupt so codex doesn't keep working.
		bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ipB, _ := json.Marshal(turnInterruptParams{ThreadID: threadID, TurnID: startResp.Turn.ID})
		interruptID := c.nextID.Add(1)
		_ = c.writeJSON(bgCtx, rpcRequest{
			JSONRPC: "2.0", ID: &interruptID, Method: "turn/interrupt", Params: ipB,
		})
		cancel()
		// Treat the conn as poisoned: a timeout means either codex is
		// hung or our readLoop missed the response, and either way
		// subsequent Turns on this conn would inherit the bad state.
		// Close it so the Pool dials a fresh one on the next Get(). This
		// self-heals the "broker gets stuck for the whole workspace
		// after one timeout" failure mode observed in prod, where each
		// new Turn would brokerTimeout forever until CXG was kubectl-
		// restarted by hand.
		c.Close()
		return nil, &TimeoutError{ThreadID: threadID, TurnID: startResp.Turn.ID}
	}
}

// StartThread issues thread/start with empty params and returns the new
// thread id. Other ThreadStartResponse fields are discarded — CXG only
// owns the loopback, agentserver tracks per-conversation state.
func (c *Conn) StartThread(ctx context.Context) (string, error) {
	id := c.nextID.Add(1)
	respCh := make(chan rpcResponse, 1)
	c.mu.Lock()
	c.pendingResp[id] = respCh
	c.mu.Unlock()

	if err := c.writeJSON(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: "thread/start", Params: json.RawMessage(`{}`)}); err != nil {
		c.mu.Lock()
		delete(c.pendingResp, id)
		c.mu.Unlock()
		return "", fmt.Errorf("write thread/start: %w", err)
	}
	resp, ok := waitResp(ctx, respCh)
	if !ok {
		// Same cleanup as Turn() — see fix in commit fc24e81.
		c.mu.Lock()
		delete(c.pendingResp, id)
		c.mu.Unlock()
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", c.closeErrOr(errors.New("connection closed before thread/start response"))
	}
	if resp.Error != nil {
		return "", &TurnRPCError{Code: resp.Error.Code, Message: resp.Error.Message, Data: resp.Error.Data}
	}
	var tsResp threadStartResponse
	if err := json.Unmarshal(resp.Result, &tsResp); err != nil {
		return "", fmt.Errorf("decode thread/start: %w", err)
	}
	if tsResp.Thread.ID == "" {
		return "", errors.New("thread/start result missing thread.id")
	}
	return tsResp.Thread.ID, nil
}

func waitResp(ctx context.Context, ch chan rpcResponse) (rpcResponse, bool) {
	select {
	case resp, open := <-ch:
		return resp, open
	case <-ctx.Done():
		return rpcResponse{}, false
	}
}

func (c *Conn) closeErrOr(fallback error) error {
	if v := c.closeErr.Load(); v != nil {
		if h, ok := v.(*errHolder); ok && h.err != nil {
			return h.err
		}
	}
	return fallback
}

// mergeTurnParams takes the caller-supplied params blob (which must be
// a JSON object) and merges {"threadId": threadID} into it without
// overwriting other caller fields. The caller MUST NOT include
// threadId — broker owns thread routing.
func mergeTurnParams(threadID string, caller json.RawMessage) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if len(caller) == 0 {
		m = map[string]json.RawMessage{}
	} else if err := json.Unmarshal(caller, &m); err != nil {
		return nil, fmt.Errorf("caller params is not a JSON object: %w", err)
	}
	if _, exists := m["threadId"]; exists {
		return nil, errors.New("caller params must not include threadId")
	}
	tid, _ := json.Marshal(threadID)
	m["threadId"] = tid
	return json.Marshal(m)
}

// TurnRPCError is returned by Turn when codex returns a JSON-RPC error
// in response to turn/start (rare; usually means malformed request).
type TurnRPCError struct {
	Code    int
	Message string
	Data    json.RawMessage
}

func (e *TurnRPCError) Error() string {
	return fmt.Sprintf("codex rpc error %d: %s", e.Code, e.Message)
}

// TimeoutError is returned when timeoutMs elapses without turn/completed.
type TimeoutError struct {
	ThreadID, TurnID string
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("turn timed out (thread=%s turn=%s)", e.ThreadID, e.TurnID)
}

// Close shuts down the ws. Safe to call multiple times.
func (c *Conn) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.ws.Close(websocket.StatusNormalClosure, "")
	})
}

func (c *Conn) writeJSON(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.Write(ctx, websocket.MessageText, b)
}
